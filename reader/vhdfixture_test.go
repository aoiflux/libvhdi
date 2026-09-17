// SPDX-License-Identifier: MIT

package reader

import (
	"encoding/binary"
	"time"

	"github.com/aoiflux/libvhdi/types"
)

// ============================================================================
// VHD test fixture builders
//
// These builders emit VHD images that conform to the Microsoft VHD Image
// Format Specification, most importantly its checksum definition: "a one's
// complement of the sum of all the bytes in the footer without the checksum
// field". Fixtures are built to the spec, not to whatever the parser
// currently happens to accept, so that a parser that disagrees with the spec
// shows up as a failing test.
// ============================================================================

// vhdSpecChecksum computes the VHD footer/dynamic-header checksum exactly as
// the VHD specification defines it: a one's complement of the unsigned sum of
// every byte of the structure with the checksum field zeroed.
func vhdSpecChecksum(b []byte) uint32 {
	var sum uint32
	for _, c := range b {
		sum += uint32(c)
	}
	return ^sum
}

const (
	vhdFooterLen    = 512
	vhdDynHeaderLen = 1024
)

// vhdFooterParams describes the fields of a VHD footer that the tests vary.
type vhdFooterParams struct {
	diskType   types.DiskType
	mediaSize  uint64
	nextOffset uint64
	identifier [16]byte

	// createdAt is the footer's own time stamp. The zero value writes a zero
	// field, which means "not recorded".
	createdAt time.Time

	// dataSize is the disk's size at creation. Zero writes mediaSize, which is
	// what an image that has never been expanded carries.
	dataSize uint64

	// cylinders, heads and sectorsPerTrack are the CHS geometry. All zero
	// writes a zero geometry, which is what most modern producers emit.
	cylinders       uint16
	heads           uint8
	sectorsPerTrack uint8

	// savedState marks an image saved from a running machine.
	savedState bool
}

// writeVHDFooter serialises a 512-byte VHD footer into dst (which must be at
// least 512 bytes) and fills in the spec checksum.
func writeVHDFooter(dst []byte, p vhdFooterParams) {
	f := dst[:vhdFooterLen]
	clearBytes(f)

	copy(f[0:8], []byte(types.VHDFooterSignature))
	binary.BigEndian.PutUint32(f[8:12], 0x00000002) // features: reserved bit
	binary.BigEndian.PutUint32(f[12:16], 0x00010000)
	binary.BigEndian.PutUint64(f[16:24], p.nextOffset)
	binary.BigEndian.PutUint32(f[24:28], vhdTimestampRaw(p.createdAt))
	binary.BigEndian.PutUint32(f[28:32], 0x6C696276) // creator app "libv"
	binary.BigEndian.PutUint32(f[32:36], 0x00010000) // creator version
	binary.BigEndian.PutUint32(f[36:40], 0x5769326B) // creator OS "Wi2k"
	binary.BigEndian.PutUint64(f[40:48], p.mediaSize)
	dataSize := p.dataSize
	if dataSize == 0 {
		dataSize = p.mediaSize
	}
	binary.BigEndian.PutUint64(f[48:56], dataSize)
	geometry := uint32(p.cylinders)<<16 | uint32(p.heads)<<8 | uint32(p.sectorsPerTrack)
	binary.BigEndian.PutUint32(f[56:60], geometry)
	binary.BigEndian.PutUint32(f[60:64], uint32(p.diskType))
	binary.BigEndian.PutUint32(f[64:68], 0) // checksum placeholder
	copy(f[68:84], p.identifier[:])
	if p.savedState {
		f[84] = 1
	}

	binary.BigEndian.PutUint32(f[64:68], vhdSpecChecksum(f))
}

// vhdDynHeaderParams describes the fields of a VHD dynamic disk header that
// the tests vary.
type vhdDynHeaderParams struct {
	blockTableOffset uint64
	numberOfBlocks   uint32
	blockSize        uint32
	parentIdentifier [16]byte
	parentFilename   string

	// parentModTime is the parent's modification time as the child records it.
	// The zero value writes a zero field, which is what a non-differencing disk
	// carries and which means "not recorded".
	parentModTime time.Time
}

// writeVHDDynHeader serialises a 1024-byte VHD dynamic disk header into dst
// and fills in the spec checksum.
func writeVHDDynHeader(dst []byte, p vhdDynHeaderParams) {
	h := dst[:vhdDynHeaderLen]
	clearBytes(h)

	copy(h[0:8], []byte(types.VHDDynamicDiskSignature))
	binary.BigEndian.PutUint64(h[8:16], 0xFFFFFFFFFFFFFFFF) // next offset: none
	binary.BigEndian.PutUint64(h[16:24], p.blockTableOffset)
	binary.BigEndian.PutUint32(h[24:28], 0x00010000)
	binary.BigEndian.PutUint32(h[28:32], p.numberOfBlocks)
	binary.BigEndian.PutUint32(h[32:36], p.blockSize)
	binary.BigEndian.PutUint32(h[36:40], 0) // checksum placeholder
	copy(h[40:56], p.parentIdentifier[:])
	binary.BigEndian.PutUint32(h[56:60], vhdTimestampRaw(p.parentModTime))
	// h[60:64] reserved1

	// Parent filename: 512 bytes of UTF-16 big-endian at offset 64.
	putUTF16BE(h[64:576], p.parentFilename)
	// h[576:768] parent locator entries, h[768:1024] reserved2.

	binary.BigEndian.PutUint32(h[36:40], vhdSpecChecksum(h))
}

// putUTF16BE encodes s as null-terminated UTF-16 big-endian into dst.
func putUTF16BE(dst []byte, s string) {
	i := 0
	for _, r := range s {
		if i+2 > len(dst) {
			return
		}
		binary.BigEndian.PutUint16(dst[i:i+2], uint16(r))
		i += 2
	}
}

func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// vhdSectorBitmapSize mirrors the sector bitmap sizing the BAT parser uses:
// one bit per 512-byte sector, rounded up to a 512-byte boundary.
// vhdSectorBitmapSize mirrors the library's own bitmap sizing. The two must
// agree exactly: a fixture that lays blocks out on a different stride than the
// parser expects tests nothing.
func vhdSectorBitmapSize(blockSize uint32) uint32 {
	const sectorSize = 512

	n := blockSize / (sectorSize * 8)
	if blockSize%(sectorSize*8) != 0 {
		n++
	}
	if rem := n % sectorSize; rem != 0 {
		n += sectorSize - rem
	}
	if n == 0 {
		n = sectorSize
	}
	return n
}

// buildFixedVHD returns a fixed VHD image of mediaSize payload bytes followed
// by the 512-byte footer. fill, if non-nil, populates the payload.
func buildFixedVHD(mediaSize int, fill func(payload []byte)) []byte {
	img := make([]byte, mediaSize+vhdFooterLen)
	if fill != nil {
		fill(img[:mediaSize])
	}
	writeVHDFooter(img[mediaSize:], vhdFooterParams{
		diskType:   types.DiskTypeFixed,
		mediaSize:  uint64(mediaSize),
		nextOffset: 0xFFFFFFFFFFFFFFFF,
		identifier: [16]byte{0x01},
	})
	return img
}

// vhdBlock describes one block of a dynamic or differencing VHD fixture.
type vhdBlock struct {
	// allocated reports whether the block has a BAT entry at all. An
	// unallocated block reads as zeroes (dynamic) or from the parent
	// (differencing).
	allocated bool
	// presentSectors lists the sector indices within the block whose bits are
	// set in the block's sector bitmap. For a differencing disk, sectors whose
	// bit is clear must be served from the parent.
	presentSectors []int
	// data is the block payload written into the file, blockSize bytes.
	data []byte
}

// vhdImageParams fully describes a dynamic or differencing VHD fixture.
type vhdImageParams struct {
	diskType  types.DiskType
	mediaSize uint64
	blockSize uint32
	blocks    []vhdBlock

	// parentName and parentID are the parent path and identifier recorded in
	// the child's dynamic header.
	parentName string
	parentID   [16]byte

	// parentModTime is the parent modification time recorded in the child's
	// dynamic header.
	parentModTime time.Time

	// createdAt, dataSize, savedState and the CHS triple are footer fields the
	// provenance and geometry accessors surface. Their zero values produce the
	// footer a plain fixture carries.
	createdAt       time.Time
	dataSize        uint64
	savedState      bool
	cylinders       uint16
	heads           uint8
	sectorsPerTrack uint8

	// selfID is this image's own identifier. Every disk in a chain needs a
	// distinct one, both so a child can name its parent unambiguously and so
	// cycle detection has something real to compare.
	selfID [16]byte
}

// buildDynamicVHD assembles a dynamic or differencing VHD image with the
// default self identifier. Use buildVHD when the identifier matters, such as
// when building a chain.
func buildDynamicVHD(diskType types.DiskType, mediaSize uint64, blockSize uint32, blocks []vhdBlock, parentName string, parentID [16]byte) []byte {
	return buildVHD(vhdImageParams{
		diskType:   diskType,
		mediaSize:  mediaSize,
		blockSize:  blockSize,
		blocks:     blocks,
		parentName: parentName,
		parentID:   parentID,
		selfID:     defaultVHDSelfID,
	})
}

// defaultVHDSelfID is the identifier stamped into single-image fixtures.
var defaultVHDSelfID = [16]byte{0x02}

// buildVHD assembles a dynamic or differencing VHD image.
//
// Layout: footer copy | dynamic header | BAT | (bitmap + data)* | footer
func buildVHD(p vhdImageParams) []byte {
	diskType, mediaSize, blockSize, blocks := p.diskType, p.mediaSize, p.blockSize, p.blocks

	const (
		footerCopyOff = 0
		dynHeaderOff  = vhdFooterLen
		batOff        = vhdFooterLen + vhdDynHeaderLen
	)

	bitmapSize := vhdSectorBitmapSize(blockSize)
	batBytes := align512(uint32(len(blocks)) * 4)
	dataStart := uint32(batOff) + batBytes

	// One (bitmap + data) unit per allocated block.
	unit := bitmapSize + blockSize
	allocatedCount := uint32(0)
	for _, b := range blocks {
		if b.allocated {
			allocatedCount++
		}
	}

	total := dataStart + allocatedCount*unit + vhdFooterLen
	img := make([]byte, total)

	selfID := p.selfID
	if selfID == ([16]byte{}) {
		selfID = defaultVHDSelfID
	}

	footer := vhdFooterParams{
		diskType:        diskType,
		mediaSize:       mediaSize,
		nextOffset:      uint64(dynHeaderOff),
		identifier:      selfID,
		createdAt:       p.createdAt,
		dataSize:        p.dataSize,
		savedState:      p.savedState,
		cylinders:       p.cylinders,
		heads:           p.heads,
		sectorsPerTrack: p.sectorsPerTrack,
	}
	writeVHDFooter(img[footerCopyOff:], footer)
	writeVHDFooter(img[total-vhdFooterLen:], footer)

	writeVHDDynHeader(img[dynHeaderOff:], vhdDynHeaderParams{
		blockTableOffset: uint64(batOff),
		numberOfBlocks:   uint32(len(blocks)),
		blockSize:        blockSize,
		parentIdentifier: p.parentID,
		parentFilename:   p.parentName,
		parentModTime:    p.parentModTime,
	})

	cursor := dataStart
	for i, b := range blocks {
		entryOff := batOff + i*4
		if !b.allocated {
			binary.BigEndian.PutUint32(img[entryOff:entryOff+4], types.VHDUnallocatedBlockMarker)
			continue
		}

		// The BAT entry holds the sector offset of the bitmap, in 512-byte units.
		binary.BigEndian.PutUint32(img[entryOff:entryOff+4], cursor/512)

		bitmap := img[cursor : cursor+bitmapSize]
		for _, s := range b.presentSectors {
			setVHDBitmapBit(bitmap, s)
		}
		if b.data != nil {
			copy(img[cursor+bitmapSize:cursor+unit], b.data)
		}
		cursor += unit
	}

	return img
}

// setVHDBitmapBit sets the bit for sector index s in a VHD block bitmap.
//
// The VHD specification stores this bitmap most-significant-bit first: the
// first sector of the block is bit 7 of byte 0, not bit 0. This matches the
// reference libvhdi C implementation and qemu's vpc driver.
func setVHDBitmapBit(bitmap []byte, s int) {
	bitmap[s/8] |= 1 << (7 - uint(s%8))
}

func align512(n uint32) uint32 {
	if n%512 == 0 {
		return n
	}
	return (n/512 + 1) * 512
}

// repeatByte returns n bytes all equal to v, for building recognisable payloads.
func repeatByte(v byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = v
	}
	return b
}
