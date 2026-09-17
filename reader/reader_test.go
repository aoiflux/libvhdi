// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

func TestOpenVHDX_FallsBackToSecondaryRegionTable(t *testing.T) {
	// Every region offset is a 1 MB multiple, which MS-VHDX requires and which
	// the first megabyte -- file identifier, both headers, both region tables --
	// depends on.
	const (
		fileSize   = 4 * 1024 * 1024
		batOffset  = 1 * 1024 * 1024
		metaOffset = 2 * 1024 * 1024
	)

	img := make([]byte, fileSize)

	// VHDX file signature at offset 0.
	copy(img[0:], []byte(types.VHDXFileSignature))

	// Primary image header at 64KB: write signature + format version, then CRC.
	copy(img[types.VHDXFirstHeaderOffset:], []byte(types.VHDXHeaderSignature))
	// Format version 0x0001 is at bytes 66-67 of the header.
	binary.LittleEndian.PutUint16(img[types.VHDXFirstHeaderOffset+66:], 0x0001)
	// Sequence number = 1.
	binary.LittleEndian.PutUint64(img[types.VHDXFirstHeaderOffset+8:], 1)
	finalizeImageHeaderCRC(img, int64(types.VHDXFirstHeaderOffset))

	// Primary region table: valid table with an unknown region only (no
	// BAT/metadata). The entry is deliberately NOT required: an unknown region
	// marked required must fail the open outright rather than fall back, which
	// TestOpenVHDX_RejectsUnknownRequiredRegion covers separately.
	writeRegionTableHeader(img, types.VHDXFirstRegionTableOffset, 1)
	primaryUnknownGUID := [16]byte{0xaa, 0xbb, 0xcc, 0xdd, 0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80, 0x90, 0xa0, 0xb0, 0xc0}
	writeRegionEntry(img, types.VHDXFirstRegionTableOffset+16, primaryUnknownGUID, 3*1024*1024, 4096, false)
	finalizeRegionTableCRC(img, types.VHDXFirstRegionTableOffset)

	// Secondary region table: contains BAT + metadata.
	writeRegionTableHeader(img, types.VHDXSecondRegionTableOffset, 2)
	writeRegionEntry(img, types.VHDXSecondRegionTableOffset+16, types.RegionTypeBAT, batOffset, 4096, true)
	writeRegionEntry(img, types.VHDXSecondRegionTableOffset+48, types.RegionTypeMetadata, metaOffset, 4096, true)
	finalizeRegionTableCRC(img, types.VHDXSecondRegionTableOffset)

	// Metadata table with required items for OpenVHDX.
	writeMetadataTable(img, metaOffset)

	// BAT with one unallocated entry (state=0) is enough for open/parsing.
	binary.LittleEndian.PutUint64(img[batOffset:batOffset+8], 0)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX returned error: %v", err)
	}

	if d.Format() != types.FileFormatVHDX {
		t.Fatalf("Format() = %v, want %v", d.Format(), types.FileFormatVHDX)
	}
	if d.BlockSize() != 1*1024*1024 {
		t.Fatalf("BlockSize() = %d, want 1048576", d.BlockSize())
	}
	if d.Size() != 1*1024*1024 {
		t.Fatalf("Size() = %d, want 1048576", d.Size())
	}
	if d.SectorSize() != 512 {
		t.Fatalf("SectorSize() = %d, want 512", d.SectorSize())
	}
}

func writeRegionTableHeader(buf []byte, off int, entries uint32) {
	copy(buf[off:off+4], []byte(types.VHDXRegionSignature))
	binary.LittleEndian.PutUint32(buf[off+4:off+8], 0) // checksum (unused by parser)
	binary.LittleEndian.PutUint32(buf[off+8:off+12], entries)
	binary.LittleEndian.PutUint32(buf[off+12:off+16], 0)
}

func writeRegionEntry(buf []byte, off int, id [16]byte, dataOffset uint64, dataSize uint32, required bool) {
	copy(buf[off:off+16], id[:])
	binary.LittleEndian.PutUint64(buf[off+16:off+24], dataOffset)
	binary.LittleEndian.PutUint32(buf[off+24:off+28], dataSize)
	if required {
		binary.LittleEndian.PutUint32(buf[off+28:off+32], 1)
	}
}

func writeMetadataTable(buf []byte, metaOffset int) {
	copy(buf[metaOffset:metaOffset+8], []byte(types.VHDXMetadataSignature))
	binary.LittleEndian.PutUint16(buf[metaOffset+8:metaOffset+10], 0)  // reserved
	binary.LittleEndian.PutUint16(buf[metaOffset+10:metaOffset+12], 4) // entry count
	// 20-byte reserved already zeroed.

	// Entries start at +32; each entry is 32 bytes.
	writeMetadataEntry(buf, metaOffset+32, types.MetadataItemFileParameters, 0x100, 8)
	writeMetadataEntry(buf, metaOffset+64, types.MetadataItemVirtualDiskSize, 0x108, 8)
	writeMetadataEntry(buf, metaOffset+96, types.MetadataItemVirtualDiskIdentifier, 0x110, 16)
	writeMetadataEntry(buf, metaOffset+128, types.MetadataItemLogicalSectorSize, 0x120, 4)

	// File parameters: block size + flags.
	binary.LittleEndian.PutUint32(buf[metaOffset+0x100:metaOffset+0x104], 1*1024*1024)
	binary.LittleEndian.PutUint32(buf[metaOffset+0x104:metaOffset+0x108], 0)

	// Virtual disk size.
	binary.LittleEndian.PutUint64(buf[metaOffset+0x108:metaOffset+0x110], 1*1024*1024)

	// Virtual disk identifier.
	copy(buf[metaOffset+0x110:metaOffset+0x120], []byte{0x43, 0xef, 0xe0, 0x41, 0x6f, 0xc7, 0x42, 0xeb, 0x89, 0xa1, 0x08, 0x55, 0x75, 0xa3, 0x61, 0xc3})

	// Logical sector size.
	binary.LittleEndian.PutUint32(buf[metaOffset+0x120:metaOffset+0x124], 512)
}

func writeMetadataEntry(buf []byte, off int, id [16]byte, itemOffset uint32, itemSize uint32) {
	copy(buf[off:off+16], id[:])
	binary.LittleEndian.PutUint32(buf[off+16:off+20], itemOffset)
	binary.LittleEndian.PutUint32(buf[off+20:off+24], itemSize)
	// trailing reserved 8 bytes left zero.
}

// finalizeRegionTableCRC computes the CRC-32 over the 64KB region table at the
// given offset within buf and writes it into bytes [off+4..off+7].
func finalizeRegionTableCRC(buf []byte, off int64) {
	table := buf[off : off+int64(types.VHDXRegionTableSize)]
	// Zero the checksum field before computing.
	table[4], table[5], table[6], table[7] = 0, 0, 0, 0
	crc := binaryutil.CRC32(table)
	binary.LittleEndian.PutUint32(table[4:8], crc)
}

// finalizeImageHeaderCRC computes the CRC-32 over the 4096-byte image header at
// the given offset within buf and writes it into bytes [off+4..off+7].
func finalizeImageHeaderCRC(buf []byte, off int64) {
	header := buf[off : off+int64(types.VHDXHeaderSize)]
	header[4], header[5], header[6], header[7] = 0, 0, 0, 0
	crc := binaryutil.CRC32(header)
	binary.LittleEndian.PutUint32(header[4:8], crc)
}

// TestOpenVHDX_RejectsUnknownRequiredRegion pins the specification rule that a
// region table entry marked required, whose type this library does not
// recognise, must fail the open.
//
// The rule was implemented but unreachable: locateRegions returned the error and
// the caller discarded it with `_`, so a primary table carrying a required
// unknown entry alongside valid BAT and metadata entries yielded usable offsets
// and opened as if the unknown region did not exist. Whatever that region
// describes would have been silently ignored.
func TestOpenVHDX_RejectsUnknownRequiredRegion(t *testing.T) {
	img := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
	})

	// Sanity: the unmodified image must open, so a failure below is caused by
	// the entry added next and not by the fixture.
	if _, err := OpenVHDX(bytes.NewReader(img), int64(len(img))); err != nil {
		t.Fatalf("baseline fixture does not open: %v", err)
	}

	// Add a third, required entry of an unknown type to the primary table,
	// leaving the valid BAT and metadata entries in place.
	unknown := [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}
	writeRegionTableHeader(img, types.VHDXFirstRegionTableOffset, 3)
	writeRegionEntry(img, types.VHDXFirstRegionTableOffset+16, types.RegionTypeBAT, vhdxBATRegionOff, vhdxBATRegionSize, true)
	writeRegionEntry(img, types.VHDXFirstRegionTableOffset+48, types.RegionTypeMetadata, vhdxMetaRegionOff, 64*1024, true)
	writeRegionEntry(img, types.VHDXFirstRegionTableOffset+80, unknown, 320*1024, 4096, true)
	finalizeRegionTableCRC(img, types.VHDXFirstRegionTableOffset)

	if _, err := OpenVHDX(bytes.NewReader(img), int64(len(img))); err == nil {
		t.Fatal("opened a VHDX whose primary region table declares an unknown required region")
	}
}
