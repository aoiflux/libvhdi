// SPDX-License-Identifier: MIT

// Package metadata provides VHDX metadata table parsing and value extraction.
package metadata

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
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

	// maxMetadataItemSize is the largest metadata item MS-VHDX permits. The
	// largest this library actually reads is a parent locator of a few hundred
	// bytes, so anything near this bound is already anomalous -- the check is
	// here to stop a crafted length from driving a huge read, not to admit one.
	maxMetadataItemSize = 1 << 20
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
		return nil, types.Corruptf("read VHDX metadata table", types.FileFormatVHDX,
			p.regionOffset, "Signature", "signature is not %q", types.VHDXMetadataSignature)
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
			return nil, types.Corruptf("read VHDX metadata table", types.FileFormatVHDX,
				p.regionOffset, "Entry Count",
				"declares %d entries but the %d byte region holds at most %d",
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

		entry := types.ParsedMetadataTableEntry{
			ItemIdentifier: guid,
			ItemOffset:     itemOffset,
			ItemSize:       itemSize,
			IsRequired:     (flags>>2)&1 == 1,
		}
		if err := p.validateEntry(entry, uint32(numEntries)); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

// validateEntry bounds one metadata table entry against the region that
// contains it.
//
// Both ItemOffset and ItemSize come straight out of the image and are used to
// seek and read, so an unchecked pair reads outside the metadata region
// entirely -- at best an I/O error from deep inside a value parser, at worst a
// structure decoded out of unrelated bytes and reported as metadata.
func (p *Parser) validateEntry(e types.ParsedMetadataTableEntry, numEntries uint32) error {
	name := metadataItemName(e.ItemIdentifier)
	const op = "read VHDX metadata table"
	at := p.regionOffset + int64(e.ItemOffset)

	if e.ItemSize == 0 {
		// A zero-length item has no data, so it must not claim a location
		// either. The pair is meaningless otherwise.
		if e.ItemOffset != 0 {
			return types.Corruptf(op, types.FileFormatVHDX, at, name,
				"zero length but a non-zero offset %d", e.ItemOffset)
		}
		return nil
	}

	if e.ItemSize > maxMetadataItemSize {
		return types.Corruptf(op, types.FileFormatVHDX, at, name,
			"declares %d bytes, more than the %d byte maximum", e.ItemSize, maxMetadataItemSize)
	}

	// The table header and its entries occupy the start of the region. An item
	// claiming to live inside the table describes its own directory as its
	// payload, which no conformant producer emits.
	tableEnd := uint64(metadataTableHeaderSize) + uint64(numEntries)*metadataTableEntrySize
	if uint64(e.ItemOffset) < tableEnd {
		return types.Corruptf(op, types.FileFormatVHDX, at, name,
			"item at region offset %d overlaps the metadata table, which ends at %d",
			e.ItemOffset, tableEnd)
	}

	if p.regionSize > 0 {
		end := uint64(e.ItemOffset) + uint64(e.ItemSize)
		if end > uint64(p.regionSize) {
			return types.Corruptf(op, types.FileFormatVHDX, at, name,
				"spans %d bytes from region offset %d, past the end of the %d byte metadata region",
				e.ItemSize, e.ItemOffset, p.regionSize)
		}
	}

	return nil
}

// metadataItemName labels a metadata item for an error message.
func metadataItemName(id [16]byte) string {
	switch id {
	case types.MetadataItemFileParameters:
		return "File Parameters"
	case types.MetadataItemVirtualDiskSize:
		return "Virtual Disk Size"
	case types.MetadataItemLogicalSectorSize:
		return "Logical Sector Size"
	case types.MetadataItemPhysicalSectorSize:
		return "Physical Sector Size"
	case types.MetadataItemVirtualDiskIdentifier:
		return "Virtual Disk Identifier"
	case types.MetadataItemParentLocator:
		return "Parent Locator"
	default:
		return "unknown (" + binaryutil.GUIDToString(id) + ")"
	}
}

// extractValues finds and decodes each known metadata item by GUID.
func (p *Parser) extractValues(entries []types.ParsedMetadataTableEntry) (*types.MetadataValues, error) {
	vals := &types.MetadataValues{
		LogicalSectorSize:  512,
		PhysicalSectorSize: 512,
	}

	for _, entry := range entries {
		// A zero-length item carries no data. Its offset is required to be zero
		// too, so parsing it anyway would decode the metadata table's own header
		// as a value -- and the signature bytes make a plausible-looking one.
		if entry.ItemSize == 0 {
			continue
		}

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
			// An unknown item marked required means the image cannot be
			// understood without something this library does not implement.
			// Decoding the rest and ignoring it would produce a device that is
			// wrong in a way nothing downstream can detect.
			if entry.IsRequired {
				return nil, types.Unsupportedf("read VHDX metadata", types.FileFormatVHDX,
					itemOffset, "Metadata Table Entry",
					"unknown required item %s", metadataItemName(entry.ItemIdentifier))
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
		return types.Corruptf("read VHDX file parameters", types.FileFormatVHDX, offset, "BlockSize",
			"block size %d is outside the permitted 1 MB to 256 MB range or is not a multiple of 512",
			blockSize)
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
		return types.Corruptf("read VHDX logical sector size", types.FileFormatVHDX, offset,
			"LogicalSectorSize", "sector size %d, the format permits only 512 or 4096", size)
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
		return types.Corruptf("read VHDX physical sector size", types.FileFormatVHDX, offset,
			"PhysicalSectorSize", "sector size %d, the format permits only 512 or 4096", size)
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

	// Locator type GUID (16 bytes). The specification defines exactly one type,
	// and its keys are only meaningful under that type, so an image declaring a
	// different one is describing its parent by a scheme this parser does not
	// understand. Reading its entries as paths would invent a parent.
	locatorTypeRaw, err := br.ReadBytes(16)
	if err != nil {
		return err
	}
	var locatorType [16]byte
	copy(locatorType[:], locatorTypeRaw)
	if locatorType != types.ParentLocatorTypeVHDX {
		return fmt.Errorf("VHDX parent locator declares unsupported type %s",
			binaryutil.GUIDToString(locatorType))
	}

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

		case "parent_linkage":
			// The identity half of the locator: the GUID the parent must carry
			// for this chain to be the one the child was built on. Without it a
			// parent can only be checked by virtual size, which any same-sized
			// image satisfies. Deliberately not appended to ParentLocators,
			// whose entries are treated as candidate paths.
			guid, err := parseLocatorGUID(value)
			if err != nil {
				return fmt.Errorf("VHDX parent locator has malformed parent_linkage %q: %w", value, err)
			}
			vals.ParentIdentifier = guid
		}
	}

	return nil
}

// parseLocatorGUID parses a parent locator GUID value, which the specification
// writes in registry form with surrounding braces. The result is in the same
// on-disk byte order as the GUIDs read from the image header, so the two are
// directly comparable.
func parseLocatorGUID(value string) ([16]byte, error) {
	s := strings.TrimSpace(value)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	return binaryutil.GUIDFromString(s)
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
