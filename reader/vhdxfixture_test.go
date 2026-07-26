// SPDX-License-Identifier: MIT

package reader

import (
	"encoding/binary"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// ============================================================================
// VHDX test fixture builder
//
// Layout produced (1 MB-aligned where the format requires it):
//
//	0        file identifier ("vhdxfile")
//	64 KB    header 1
//	128 KB   header 2
//	192 KB   region table 1
//	256 KB   region table 2
//	1 MB     BAT region
//	2 MB     metadata region
//	3 MB     sector bitmap block for chunk 0
//	4 MB     payload block 0
// ============================================================================

const (
	mb = 1024 * 1024

	// The BAT region is sized to hold the largest table the fixtures need. A
	// 4096-byte logical sector raises the chunk ratio to 32768, so the chunk-0
	// sector bitmap entry sits at BAT index 32768 and the table runs to 256 KB.
	vhdxBATRegionSize = 512 * 1024

	vhdxBATRegionOff    = 1 * mb
	vhdxMetaRegionOff   = 2 * mb
	vhdxBitmapBlockOff  = 3 * mb
	vhdxPayloadBlockOff = 4 * mb
	vhdxLogRegionOff    = 5 * mb
	vhdxLogRegionSize   = 1 * mb
	vhdxImageSize       = 6 * mb
)

// logSectorSize is the VHDX log's fixed sector size.
const logSectorSize = 4096

// logWrite is one sector a fixture's log entry replays.
type logWrite struct {
	fileOffset uint64
	// data is the full logSectorSize payload to appear at fileOffset after
	// replay.
	data []byte
}

// vhdxParams describes a VHDX fixture.
type vhdxParams struct {
	blockSize       uint32
	sectorSize      uint32
	virtualDiskSize uint64
	hasParent       bool

	// blockState is the BAT state for payload block 0 (6 = fully present,
	// 7 = partially present, 0 = not present).
	blockState types.BlockState

	// presentSectors lists sector indices within block 0 whose bits are set in
	// the chunk sector bitmap. Only meaningful for state 7.
	presentSectors []int

	// payload fills block 0.
	payload []byte

	// omitFileParameters drops the File Parameters metadata item. The item is
	// required by the specification, so its absence models a corrupt or crafted
	// image; block size is then unknown.
	omitFileParameters bool

	// omitVirtualDiskSize drops the Virtual Disk Size metadata item.
	omitVirtualDiskSize bool

	// dirtyLog writes a non-zero log GUID into both headers, marking the image
	// as having a log. With no logWrites, the log region holds no valid entry,
	// so the log is present but unreplayable.
	dirtyLog bool

	// logWrites, when non-empty, are placed in a single valid log entry at the
	// start of the log region. A log GUID is written into both headers, so the
	// image is dirty until the entry is replayed.
	logWrites []logWrite
}

// buildVHDX assembles a VHDX image per vhdxParams.
func buildVHDX(p vhdxParams) []byte {
	img := make([]byte, vhdxImageSize)

	// File identifier.
	copy(img[0:8], []byte(types.VHDXFileSignature))

	// Both headers, sequence numbers 1 and 2. A zero log GUID marks the file
	// clean, needing no log replay.
	withLog := p.dirtyLog || len(p.logWrites) > 0
	writeVHDXImageHeaderWith(img, types.VHDXFirstHeaderOffset, 1, withLog)
	writeVHDXImageHeaderWith(img, types.VHDXSecondHeaderOffset, 2, withLog)

	// Region table: BAT + metadata, both required.
	writeRegionTableHeader(img, types.VHDXFirstRegionTableOffset, 2)
	writeRegionEntry(img, types.VHDXFirstRegionTableOffset+16, types.RegionTypeBAT, vhdxBATRegionOff, vhdxBATRegionSize, true)
	writeRegionEntry(img, types.VHDXFirstRegionTableOffset+48, types.RegionTypeMetadata, vhdxMetaRegionOff, 64*1024, true)
	finalizeRegionTableCRC(img, types.VHDXFirstRegionTableOffset)

	// The secondary table mirrors the primary.
	copy(img[types.VHDXSecondRegionTableOffset:types.VHDXSecondRegionTableOffset+64*1024],
		img[types.VHDXFirstRegionTableOffset:types.VHDXFirstRegionTableOffset+64*1024])

	writeVHDXMetadataRegion(img, vhdxMetaRegionOff, p)
	writeVHDXBAT(img, vhdxBATRegionOff, p)

	// Payload block 0.
	if p.payload != nil {
		copy(img[vhdxPayloadBlockOff:vhdxPayloadBlockOff+int(p.blockSize)], p.payload)
	}

	// Sector bitmap block for chunk 0 (VHDX bitmaps are LSB-first).
	if p.blockState == types.BlockStatePartiallyAllocated {
		bitmap := img[vhdxBitmapBlockOff : vhdxBitmapBlockOff+mb]
		for _, s := range p.presentSectors {
			bitmap[s/8] |= 1 << uint(s%8)
		}
	}

	// A valid log entry journalling the requested sector writes.
	if len(p.logWrites) > 0 {
		entry := buildLogEntry(testLogGUID, 1, 0, p.logWrites)
		copy(img[vhdxLogRegionOff:vhdxLogRegionOff+vhdxLogRegionSize], entry)
	}

	return img
}

// testLogGUID is the log GUID fixtures write into the headers and their entries.
// The two must match or the entry is treated as belonging to a previous log.
var testLogGUID = [16]byte{0x77, 0x6C, 0x6F, 0x67, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}

// buildLogEntry assembles a VHDX log entry containing one data descriptor per
// write, per the VHDX Format Specification.
//
// Layout: 64-byte entry header, then 32 bytes per descriptor, then one 4 KB data
// sector per data descriptor starting at the next sector boundary. The writer
// overwrites each data sector's first 8 and last 4 bytes with a signature and the
// split sequence number, stashing the displaced bytes in the descriptor, so the
// builder does the same.
func buildLogEntry(logGUID [16]byte, sequenceNumber uint64, tail uint32, writes []logWrite) []byte {
	headerAndDescs := alignUpInt(64+len(writes)*32, logSectorSize)
	entryLength := headerAndDescs + len(writes)*logSectorSize
	buf := make([]byte, entryLength)

	copy(buf[0:4], []byte("loge"))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(entryLength))
	binary.LittleEndian.PutUint32(buf[12:16], tail)
	binary.LittleEndian.PutUint64(buf[16:24], sequenceNumber)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(len(writes)))
	copy(buf[32:48], logGUID[:])

	for i, w := range writes {
		slot := buf[64+i*32:][:32]
		copy(slot[0:4], []byte("desc"))
		copy(slot[4:8], w.data[4092:4096]) // displaced trailing bytes
		copy(slot[8:16], w.data[0:8])      // displaced leading bytes
		binary.LittleEndian.PutUint64(slot[16:24], w.fileOffset)
		binary.LittleEndian.PutUint64(slot[24:32], sequenceNumber)

		sector := buf[headerAndDescs+i*logSectorSize:][:logSectorSize]
		copy(sector, w.data)
		copy(sector[0:4], []byte("data"))
		binary.LittleEndian.PutUint32(sector[4:8], uint32(sequenceNumber>>32))
		binary.LittleEndian.PutUint32(sector[4092:4096], uint32(sequenceNumber))
	}

	// Checksum over the whole entry with the field zeroed.
	binary.LittleEndian.PutUint32(buf[4:8], 0)
	binary.LittleEndian.PutUint32(buf[4:8], binaryutil.CRC32(buf))
	return buf
}

func alignUpInt(n, to int) int {
	if n%to == 0 {
		return n
	}
	return (n/to + 1) * to
}

// writeVHDXImageHeader writes a clean 4096-byte VHDX header and its CRC-32C.
func writeVHDXImageHeader(img []byte, off int, seq uint64) {
	writeVHDXImageHeaderWith(img, off, seq, false)
}

// writeVHDXImageHeaderWith writes a 4096-byte VHDX header and its CRC-32C,
// optionally marking the image dirty via a non-zero log GUID.
func writeVHDXImageHeaderWith(img []byte, off int, seq uint64, withLog bool) {
	copy(img[off:off+4], []byte(types.VHDXHeaderSignature))
	binary.LittleEndian.PutUint64(img[off+8:off+16], seq)
	// FileWriteGuid at +16, DataWriteGuid at +32, LogGuid at +48.
	binary.LittleEndian.PutUint16(img[off+64:off+66], 0)      // log format version
	binary.LittleEndian.PutUint16(img[off+66:off+68], 0x0001) // format version
	if withLog {
		copy(img[off+48:off+64], testLogGUID[:])
		binary.LittleEndian.PutUint32(img[off+68:off+72], vhdxLogRegionSize)
		binary.LittleEndian.PutUint64(img[off+72:off+80], vhdxLogRegionOff)
	} else {
		copy(img[off+48:off+64], make([]byte, 16))
		binary.LittleEndian.PutUint32(img[off+68:off+72], 0)
		binary.LittleEndian.PutUint64(img[off+72:off+80], 0)
	}
	finalizeImageHeaderCRC(img, int64(off))
}

// writeVHDXBAT writes the block allocation table.
//
// The BAT interleaves one sector bitmap entry after every chunkRatio payload
// entries, where chunkRatio = (2^23 * logicalSectorSize) / blockSize.
func writeVHDXBAT(img []byte, batOff int, p vhdxParams) {
	chunkRatio := int((1 << 23) * uint64(p.sectorSize) / uint64(p.blockSize))
	blockCount := int((p.virtualDiskSize + uint64(p.blockSize) - 1) / uint64(p.blockSize))

	// The BAT region is 64 KB in this layout. A fixture describing an
	// implausibly large virtual disk cannot fit its whole table, which is the
	// point of such a fixture, so entries beyond the region are dropped rather
	// than written out of bounds.
	batRegionEnd := batOff + vhdxBATRegionSize
	putEntry := func(index int, state types.BlockState, fileOffsetMB uint64) bool {
		off := batOff + index*8
		if off < 0 || off+8 > batRegionEnd || off+8 > len(img) {
			return false
		}
		binary.LittleEndian.PutUint64(img[off:off+8], fileOffsetMB<<20|uint64(state))
		return true
	}

	// Payload block 0.
	if p.blockState == types.BlockStateNone {
		putEntry(0, types.BlockStateNone, 0)
	} else {
		putEntry(0, p.blockState, vhdxPayloadBlockOff/mb)
	}

	// Remaining payload blocks are not present, accounting for the interleaved
	// bitmap entries.
	for i := 1; i < blockCount; i++ {
		if !putEntry(i+i/chunkRatio, types.BlockStateNone, 0) {
			break
		}
	}

	// Sector bitmap entry for chunk 0 sits at BAT index chunkRatio.
	if p.blockState == types.BlockStatePartiallyAllocated {
		// State 6 == SECTOR_BITMAP_BLOCK_PRESENT.
		putEntry(chunkRatio, 6, vhdxBitmapBlockOff/mb)
	}
}

// writeVHDXMetadataRegion writes the metadata table and its items.
func writeVHDXMetadataRegion(img []byte, metaOff int, p vhdxParams) {
	copy(img[metaOff:metaOff+8], []byte(types.VHDXMetadataSignature))

	const (
		offFileParams = 0x1000
		offDiskSize   = 0x1010
		offSectorSize = 0x1020
		offDiskID     = 0x1030
		offParentLoc  = 0x1100
	)

	// Entries are written contiguously from +32; the count must match however
	// many were actually emitted.
	entryOff := metaOff + 32
	count := 0
	emit := func(id [16]byte, itemOffset, itemSize uint32) {
		writeMetadataEntry(img, entryOff, id, itemOffset, itemSize)
		entryOff += 32
		count++
	}

	if !p.omitFileParameters {
		emit(types.MetadataItemFileParameters, offFileParams, 8)
	}
	if !p.omitVirtualDiskSize {
		emit(types.MetadataItemVirtualDiskSize, offDiskSize, 8)
	}
	emit(types.MetadataItemLogicalSectorSize, offSectorSize, 4)
	emit(types.MetadataItemVirtualDiskIdentifier, offDiskID, 16)
	if p.hasParent {
		emit(types.MetadataItemParentLocator, offParentLoc, 256)
	}

	binary.LittleEndian.PutUint16(img[metaOff+10:metaOff+12], uint16(count))

	// File parameters: block size + flags. Bit 1 (HasParent) marks a
	// differencing disk.
	binary.LittleEndian.PutUint32(img[metaOff+offFileParams:], p.blockSize)
	var flags uint32
	if p.hasParent {
		flags |= 0x2
	}
	binary.LittleEndian.PutUint32(img[metaOff+offFileParams+4:], flags)

	binary.LittleEndian.PutUint64(img[metaOff+offDiskSize:], p.virtualDiskSize)
	binary.LittleEndian.PutUint32(img[metaOff+offSectorSize:], p.sectorSize)
	copy(img[metaOff+offDiskID:metaOff+offDiskID+16], repeatByte(0x5A, 16))

	if p.hasParent {
		writeVHDXParentLocator(img, metaOff+offParentLoc, "relative_path", "parent.vhdx")
	}
}

// writeVHDXParentLocator writes a parent locator item with a single key/value
// pair. Offsets within the item are relative to the item's start.
func writeVHDXParentLocator(img []byte, itemOff int, key, value string) {
	// 16-byte locator type GUID, 2 reserved, 2 entry count.
	binary.LittleEndian.PutUint16(img[itemOff+18:itemOff+20], 1)

	const (
		descOff = 20 // first (and only) 12-byte descriptor
		keyOff  = 64
		valOff  = 128
	)

	keyBytes := utf16LEBytes(key)
	valBytes := utf16LEBytes(value)

	binary.LittleEndian.PutUint32(img[itemOff+descOff:], keyOff)
	binary.LittleEndian.PutUint32(img[itemOff+descOff+4:], valOff)
	binary.LittleEndian.PutUint16(img[itemOff+descOff+8:], uint16(len(keyBytes)))
	binary.LittleEndian.PutUint16(img[itemOff+descOff+10:], uint16(len(valBytes)))

	copy(img[itemOff+keyOff:], keyBytes)
	copy(img[itemOff+valOff:], valBytes)
}

func utf16LEBytes(s string) []byte {
	b := make([]byte, 0, len(s)*2)
	for _, r := range s {
		b = append(b, byte(uint16(r)), byte(uint16(r)>>8))
	}
	return b
}
