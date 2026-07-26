// SPDX-License-Identifier: MIT

// Package metadata provides VHDX metadata table parsing and value extraction.
package metadata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// Metadata table layout constants. Every count and offset read from the table is
// bounded against these and the region size, since all of them come from the
// image and would otherwise drive unbounded reads and allocations.
const (
	metadataTableHeaderSize = 32
	metadataTableEntrySize  = 32

	// parentLocatorHeaderSize is the 16-byte type GUID plus 2 reserved bytes
	// plus the 2-byte entry count.
	parentLocatorHeaderSize = 20

	// parentLocatorEntrySize is two 4-byte offsets and two 2-byte lengths.
	parentLocatorEntrySize = 12
)

// Parser parses the VHDX metadata region and extracts structured values.
type Parser struct {
	reader       io.ReaderAt
	regionOffset int64 // absolute offset of the metadata region in the file
	regionSize   uint32
}

// NewParser creates a new metadata parser positioned at the given region offset.
func NewParser(r io.ReaderAt, regionOffset int64, regionSize uint32) *Parser {
	return &Parser{reader: r, regionOffset: regionOffset, regionSize: regionSize}
}

// Parse reads and decodes the metadata table, returning all extracted values.
func (p *Parser) Parse() (*types.MetadataValues, error) {
	entries, err := p.readTable()
	if err != nil {
		return nil, err
	}
	return p.extractValues(entries)
}

// readTable reads the metadata table header and entries.
func (p *Parser) readTable() ([]types.ParsedMetadataTableEntry, error) {
	br := binaryutil.NewReadAtReader(p.reader, p.regionOffset)

	// Read signature (8 bytes)
	sig, err := br.ReadBytes(8)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(sig, []byte(types.VHDXMetadataSignature)) {
		return nil, errors.New("invalid VHDX metadata table signature")
	}

	// Reserved (2 bytes)
	if _, err = br.ReadBytes(2); err != nil {
		return nil, err
	}

	// Number of entries (2 bytes, little-endian)
	numEntries, err := br.ReadUint16LittleEndian()
	if err != nil {
		return nil, err
	}

	// The table header is 32 bytes and each entry is 32 bytes, so the region
	// physically bounds how many entries can exist. Without this check a
	// crafted count drives reads and allocations far beyond the region.
	if p.regionSize > 0 {
		maxEntries := (p.regionSize - metadataTableHeaderSize) / metadataTableEntrySize
		if uint32(numEntries) > maxEntries {
			return nil, fmt.Errorf("VHDX metadata table declares %d entries but the %d byte region holds at most %d",
				numEntries, p.regionSize, maxEntries)
		}
	}

	// Reserved (20 bytes)
	if _, err = br.ReadBytes(20); err != nil {
		return nil, err
	}

	// Read each entry (32 bytes each)
	entries := make([]types.ParsedMetadataTableEntry, 0, numEntries)
	for i := uint16(0); i < numEntries; i++ {
		var guid [16]byte

		guidBytes, err := br.ReadBytes(16)
		if err != nil {
			return nil, err
		}
		copy(guid[:], guidBytes)

		itemOffset, err := br.ReadUint32LittleEndian()
		if err != nil {
			return nil, err
		}

		itemSize, err := br.ReadUint32LittleEndian()
		if err != nil {
			return nil, err
		}

		// Flags word (4 bytes, little-endian) embedded in the 8 reserved bytes:
		//   bit 0 = IsUser, bit 1 = IsVirtualDisk, bit 2 = IsRequired
		flags, err := br.ReadUint32LittleEndian()
		if err != nil {
			return nil, err
		}

		// Reserved (4 bytes)
		if _, err = br.ReadBytes(4); err != nil {
			return nil, err
		}

		entries = append(entries, types.ParsedMetadataTableEntry{
			ItemIdentifier: guid,
			ItemOffset:     itemOffset,
			ItemSize:       itemSize,
			IsRequired:     (flags>>2)&1 == 1,
		})
	}

	return entries, nil
}

// extractValues finds and decodes each known metadata item by GUID.
func (p *Parser) extractValues(entries []types.ParsedMetadataTableEntry) (*types.MetadataValues, error) {
	vals := &types.MetadataValues{
		LogicalSectorSize:  512,
		PhysicalSectorSize: 512,
	}

	for _, entry := range entries {
		// Item data is at p.regionOffset + entry.ItemOffset
		itemOffset := p.regionOffset + int64(entry.ItemOffset)

		switch entry.ItemIdentifier {
		case types.MetadataItemFileParameters:
			if err := p.parseFileParameters(itemOffset, vals); err != nil {
				return nil, err
			}
		case types.MetadataItemVirtualDiskSize:
			if err := p.parseVirtualDiskSize(itemOffset, vals); err != nil {
				return nil, err
			}
		case types.MetadataItemLogicalSectorSize:
			if err := p.parseLogicalSectorSize(itemOffset, vals); err != nil {
				return nil, err
			}
		case types.MetadataItemPhysicalSectorSize:
			if err := p.parsePhysicalSectorSize(itemOffset, vals); err != nil {
				return nil, err
			}
		case types.MetadataItemVirtualDiskIdentifier:
			if err := p.parseVirtualDiskIdentifier(itemOffset, vals); err != nil {
				return nil, err
			}
		case types.MetadataItemParentLocator:
			if err := p.parseParentLocator(itemOffset, entry.ItemSize, vals); err != nil {
				return nil, err
			}
		default:
			// Unknown metadata item: fail if marked required.
			if entry.IsRequired {
				return nil, errors.New("VHDX metadata table contains unknown required item")
			}
		}
	}

	return vals, nil
}

// parseFileParameters decodes the File Parameters metadata item (8 bytes).
func (p *Parser) parseFileParameters(offset int64, vals *types.MetadataValues) error {
	br := binaryutil.NewReadAtReader(p.reader, offset)

	// Block size (4 bytes, little-endian)
	blockSize, err := br.ReadUint32LittleEndian()
	if err != nil {
		return err
	}
	vals.BlockSize = blockSize

	// Flags (4 bytes, little-endian): bit 0 = LeaveBlocksAllocated, bit 1 = HasParent
	flags, err := br.ReadUint32LittleEndian()
	if err != nil {
		return err
	}
	vals.LeaveBitsUnallocated = (flags & 0x1) == 0
	// bit 1 (HasParent) → differencing; bit 0 only (LeaveBlocksAllocated) → fixed
	if flags&0x2 != 0 {
		vals.DiskType = types.DiskTypeDifferential
	} else if flags&0x1 != 0 {
		vals.DiskType = types.DiskTypeFixed
	} else {
		vals.DiskType = types.DiskTypeDynamic
	}

	// Validate block size: must be >= 1MB, <= 256MB, and a multiple of 512.
	const minBlockSize = 1 << 20         // 1 MB
	const maxBlockSize = 256 * (1 << 20) // 256 MB
	if blockSize < minBlockSize || blockSize > maxBlockSize || blockSize%512 != 0 {
		return errors.New("invalid VHDX block size")
	}

	return nil
}

// parseVirtualDiskSize decodes the Virtual Disk Size metadata item (8 bytes).
func (p *Parser) parseVirtualDiskSize(offset int64, vals *types.MetadataValues) error {
	br := binaryutil.NewReadAtReader(p.reader, offset)

	size, err := br.ReadUint64LittleEndian()
	if err != nil {
		return err
	}
	vals.VirtualDiskSize = size
	return nil
}

// parseLogicalSectorSize decodes the Logical Sector Size metadata item (4 bytes).
func (p *Parser) parseLogicalSectorSize(offset int64, vals *types.MetadataValues) error {
	br := binaryutil.NewReadAtReader(p.reader, offset)

	size, err := br.ReadUint32LittleEndian()
	if err != nil {
		return err
	}
	if size != 512 && size != 4096 {
		return errors.New("invalid VHDX logical sector size: must be 512 or 4096")
	}
	vals.LogicalSectorSize = size
	return nil
}

// parsePhysicalSectorSize decodes the Physical Sector Size metadata item (4 bytes).
func (p *Parser) parsePhysicalSectorSize(offset int64, vals *types.MetadataValues) error {
	br := binaryutil.NewReadAtReader(p.reader, offset)

	size, err := br.ReadUint32LittleEndian()
	if err != nil {
		return err
	}
	if size != 512 && size != 4096 {
		return errors.New("invalid VHDX physical sector size: must be 512 or 4096")
	}
	vals.PhysicalSectorSize = size
	return nil
}

// parseVirtualDiskIdentifier decodes the Virtual Disk Identifier metadata item (16 bytes).
func (p *Parser) parseVirtualDiskIdentifier(offset int64, vals *types.MetadataValues) error {
	br := binaryutil.NewReadAtReader(p.reader, offset)

	id, err := br.ReadBytes(16)
	if err != nil {
		return err
	}
	copy(vals.VirtualDiskIdentifier[:], id)
	return nil
}

// parseParentLocator decodes the Parent Locator metadata item.
// Layout: 4-byte type GUID (16 bytes) + 2-byte reserved + 2-byte entry count,
// then N × 12-byte key/value offset+length pairs, then the actual string data.
func (p *Parser) parseParentLocator(offset int64, size uint32, vals *types.MetadataValues) error {
	br := binaryutil.NewReadAtReader(p.reader, offset)

	// Locator type GUID (16 bytes)
	locatorTypeRaw, err := br.ReadBytes(16)
	if err != nil {
		return err
	}
	// We don't validate the type GUID further here; parent paths are in the entries.
	_ = locatorTypeRaw

	// Reserved (2 bytes)
	if _, err = br.ReadBytes(2); err != nil {
		return err
	}

	// Number of key/value entries (2 bytes, little-endian)
	entryCount, err := br.ReadUint16LittleEndian()
	if err != nil {
		return err
	}

	// The item's declared size bounds how many descriptors can exist. Without
	// this, a crafted count of 65535 with maximal key and value lengths drives
	// gigabytes of reads and allocations from a few bytes of input.
	if size < parentLocatorHeaderSize {
		return fmt.Errorf("VHDX parent locator item is %d bytes, shorter than its %d byte header",
			size, parentLocatorHeaderSize)
	}
	maxEntries := (size - parentLocatorHeaderSize) / parentLocatorEntrySize
	if uint32(entryCount) > maxEntries {
		return fmt.Errorf("VHDX parent locator declares %d entries but its %d byte item holds at most %d",
			entryCount, size, maxEntries)
	}

	type locatorEntry struct {
		keyOffset   uint32
		valueOffset uint32
		keyLength   uint16
		valueLength uint16
	}
	locEntries := make([]locatorEntry, entryCount)

	// Read the fixed-size entry descriptors (12 bytes each)
	for i := uint16(0); i < entryCount; i++ {
		ko, err := br.ReadUint32LittleEndian()
		if err != nil {
			return err
		}
		vo, err := br.ReadUint32LittleEndian()
		if err != nil {
			return err
		}
		kl, err := br.ReadUint16LittleEndian()
		if err != nil {
			return err
		}
		vl, err := br.ReadUint16LittleEndian()
		if err != nil {
			return err
		}
		locEntries[i] = locatorEntry{ko, vo, kl, vl}
	}

	// Read key/value pairs. Offsets are relative to the start of this item, so
	// every span must lie inside it.
	base := offset
	within := func(off uint32, length uint16) bool {
		end := uint64(off) + uint64(length)
		return end <= uint64(size)
	}

	for _, le := range locEntries {
		if !within(le.keyOffset, le.keyLength) || !within(le.valueOffset, le.valueLength) {
			return fmt.Errorf("VHDX parent locator entry spans outside its %d byte item", size)
		}

		keyBytes := make([]byte, le.keyLength)
		if _, err := p.reader.ReadAt(keyBytes, base+int64(le.keyOffset)); err != nil {
			return err
		}
		valBytes := make([]byte, le.valueLength)
		if _, err := p.reader.ReadAt(valBytes, base+int64(le.valueOffset)); err != nil {
			return err
		}

		// Both key and value are UTF-16 LE strings
		key := decodeUTF16LE(keyBytes)
		value := decodeUTF16LE(valBytes)

		// The "relative_path" or "absolute_win32_path" key identifies the parent path
		switch key {
		case "relative_path", "absolute_win32_path", "volume_path":
			if vals.ParentFilename == "" {
				vals.ParentFilename = value
			}
			// Retain every candidate, in the order the image lists them, so a
			// resolver can fall back when the first path no longer exists.
			vals.ParentLocators = append(vals.ParentLocators, types.ParentLocatorEntry{
				Key:   key,
				Value: value,
			})
		}
	}

	return nil
}

// decodeUTF16LE decodes a UTF-16 little-endian byte slice to a Go string.
func decodeUTF16LE(data []byte) string {
	if len(data)%2 != 0 {
		return ""
	}
	u16 := make([]uint16, 0, len(data)/2)
	for i := 0; i < len(data); i += 2 {
		v := binary.LittleEndian.Uint16(data[i : i+2])
		if v == 0 {
			break
		}
		u16 = append(u16, v)
	}
	return string(utf16.Decode(u16))
}
