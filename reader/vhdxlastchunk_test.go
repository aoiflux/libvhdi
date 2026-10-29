// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// Sector bitmaps of a differencing VHDX that ends partway through a chunk
// (PAR-33).
//
// The sector bitmap entry of chunk c sits at BAT index c*(R+1)+R, after the
// chunk's R payload entries. A differencing disk's BAT holds whole chunks, so
// that entry exists even when the disk ends inside chunk c (MS-VHDX 2.5). The
// parser read only N+ceil(N/R) entries, which stops short of it whenever N is
// not a multiple of R, and took the bitmap as absent. Every PARTIALLY_PRESENT
// block in the last chunk was then read wholly from the parent, replacing the
// snapshot's own sectors with older data and reporting no error. A disk smaller
// than one chunk lost its only bitmap.

// putVHDXBATEntry writes one raw BAT entry of a buildVHDX image.
func putVHDXBATEntry(img []byte, index int, state types.BlockState, fileOffsetMB int) {
	off := vhdxBATRegionOff + index*8
	binary.LittleEndian.PutUint64(img[off:off+8], uint64(fileOffsetMB)<<20|uint64(state))
}

// growImage extends img with zeroes to at least size bytes.
func growImage(img []byte, size int) []byte {
	if len(img) >= size {
		return img
	}
	return append(img, make([]byte, size-len(img))...)
}

// placeVHDXBlock points payload block blockIndex of a buildVHDX image at
// fileOffsetMB with the given state and copies data there, growing the image as
// needed.
func placeVHDXBlock(img []byte, chunkRatio, blockIndex int, state types.BlockState, fileOffsetMB int, data []byte) []byte {
	img = growImage(img, fileOffsetMB*mb+len(data))
	putVHDXBATEntry(img, blockIndex+blockIndex/chunkRatio, state, fileOffsetMB)
	copy(img[fileOffsetMB*mb:], data)
	return img
}

// placeVHDXSectorBitmap gives chunk a sector bitmap block at fileOffsetMB with
// the given bits set, growing the image as needed.
func placeVHDXSectorBitmap(img []byte, chunkRatio, chunk, fileOffsetMB int, bits []int) []byte {
	img = growImage(img, (fileOffsetMB+1)*mb)
	// State 6 == SECTOR_BITMAP_BLOCK_PRESENT.
	putVHDXBATEntry(img, chunk*(chunkRatio+1)+chunkRatio, 6, fileOffsetMB)
	bitmap := img[fileOffsetMB*mb:]
	for _, b := range bits {
		bitmap[b/8] |= 1 << uint(b%8) // VHDX sector bitmaps are LSB-first
	}
	return img
}

// lastChunkBlock is the only payload block in the second chunk of a
// vhdxDiffVirtual disk: 4097 blocks at a chunk ratio of 4096.
const lastChunkBlock = 4096

// lastChunkPresent lists the sectors of lastChunkBlock present in the child.
var lastChunkPresent = []int{0, 3}

// lastChunkChain writes a dynamic parent and a differencing child that ends one
// block into its second chunk. In the child that block is PARTIALLY_PRESENT,
// with its bitmap in the chunk-1 sector bitmap block; in the parent it is fully
// present. The two hold different fills, and the child's absent sectors hold
// junk, so each sector's source is unambiguous.
func lastChunkChain(t *testing.T) (childPath string) {
	t.Helper()

	const chunkRatio = (1 << 23) * vhdxDiffSectorSize / vhdxDiffBlockSize

	parent := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
		dataWriteGUID:   guidRightParent,
		virtualDiskID:   idBase,
	})
	parent = placeVHDXBlock(parent, chunkRatio, lastChunkBlock,
		types.BlockStateFullyAllocated, 6, repeatByte(0xA1, vhdxDiffBlockSize))

	child := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		hasParent:       true,
		blockState:      types.BlockStateNone,
		parentLinkage:   linkageValue(guidRightParent),
		virtualDiskID:   idTop,
	})
	childBlock := repeatByte(0xEE, vhdxDiffBlockSize)
	for _, s := range lastChunkPresent {
		copy(childBlock[s*vhdxDiffSectorSize:(s+1)*vhdxDiffSectorSize],
			repeatByte(0xC1, vhdxDiffSectorSize))
	}
	child = placeVHDXBlock(child, chunkRatio, lastChunkBlock,
		types.BlockStatePartiallyAllocated, 6, childBlock)
	// The block is the first in its chunk, so its sectors are the chunk's first
	// bits.
	child = placeVHDXSectorBitmap(child, chunkRatio, 1, 7, lastChunkPresent)

	dir := writeChainDir(t, map[string][]byte{
		"parent.vhdx": parent,
		"child.vhdx":  child,
	})
	return filepath.Join(dir, "child.vhdx")
}

func TestDifferencingVHDXPartialLastChunk(t *testing.T) {
	d, err := OpenFileWith(lastChunkChain(t), &Options{RequireParentChain: true})
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	base := int64(lastChunkBlock) * vhdxDiffBlockSize

	buf := make([]byte, 4*vhdxDiffSectorSize)
	if _, err := d.ReadAt(buf, base); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	for s := range 4 {
		got := buf[s*vhdxDiffSectorSize : (s+1)*vhdxDiffSectorSize]
		want := byte(0xA1) // from the parent
		if s == 0 || s == 3 {
			want = 0xC1 // present in the child
		}
		if !bytes.Equal(got, repeatByte(want, vhdxDiffSectorSize)) {
			t.Errorf("sector %d of block %d = %#x..., want fill %#x", s, lastChunkBlock, got[:4], want)
		}
	}

	for _, c := range []struct {
		offsetInBlock int64
		want          bool
	}{{0, true}, {vhdxDiffSectorSize, false}, {3 * vhdxDiffSectorSize, true}} {
		present, err := d.vhdxSectorPresent(lastChunkBlock, c.offsetInBlock)
		if err != nil {
			t.Fatalf("vhdxSectorPresent(%d): %v", c.offsetInBlock, err)
		}
		if present != c.want {
			t.Errorf("vhdxSectorPresent(%d) = %v, want %v", c.offsetInBlock, present, c.want)
		}
	}

	extents, err := d.Extents(base, vhdxDiffSectorSize)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}
	want := Extent{
		VirtualOffset: base,
		Length:        vhdxDiffSectorSize,
		Kind:          ExtentMapped,
		FileOffset:    6 * mb,
		ChainIndex:    0,
	}
	if len(extents) != 1 || extents[0].VirtualOffset != want.VirtualOffset ||
		extents[0].Length != want.Length || extents[0].Kind != want.Kind ||
		extents[0].FileOffset != want.FileOffset || extents[0].ChainIndex != want.ChainIndex {
		t.Errorf("Extents = %+v, want one extent %+v", extents, want)
	}
}

// openSmallDiffChain opens a child of the given virtual size whose block 0 is
// PARTIALLY_PRESENT with only sector 0 present, over a dynamic parent.
func openSmallDiffChain(t *testing.T, virtual uint64) *VirtualDisk {
	t.Helper()

	parent := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: virtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
		dataWriteGUID:   guidRightParent,
		virtualDiskID:   idBase,
	})
	childPayload := make([]byte, vhdxDiffBlockSize)
	copy(childPayload, repeatByte(0xC0, vhdxDiffSectorSize))
	child := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: virtual,
		hasParent:       true,
		blockState:      types.BlockStatePartiallyAllocated,
		presentSectors:  []int{0},
		payload:         childPayload,
		parentLinkage:   linkageValue(guidRightParent),
		virtualDiskID:   idTop,
	})

	dir := writeChainDir(t, map[string][]byte{
		"parent.vhdx": parent,
		"child.vhdx":  child,
	})
	d, err := OpenFileWith(filepath.Join(dir, "child.vhdx"), &Options{RequireParentChain: true})
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// checkSmallDiffChain asserts sector 0 comes from the child and sector 1 from
// the parent.
func checkSmallDiffChain(t *testing.T, d *VirtualDisk) {
	t.Helper()

	buf := make([]byte, 2*vhdxDiffSectorSize)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf[:vhdxDiffSectorSize], repeatByte(0xC0, vhdxDiffSectorSize)) {
		t.Errorf("sector 0 = %#x..., want the child's 0xC0", buf[:4])
	}
	if !bytes.Equal(buf[vhdxDiffSectorSize:], repeatByte(0xA0, vhdxDiffSectorSize)) {
		t.Errorf("sector 1 = %#x..., want the parent's 0xA0", buf[vhdxDiffSectorSize:vhdxDiffSectorSize+4])
	}
}

// TestDifferencingVHDXSingleChunkBitmap covers a differencing disk smaller than
// one chunk: two blocks at a chunk ratio of 4096. Its only sector bitmap entry
// is the last entry of its single, partial chunk.
func TestDifferencingVHDXSingleChunkBitmap(t *testing.T) {
	checkSmallDiffChain(t, openSmallDiffChain(t, 2*mb))
}

// TestDifferencingVHDXExactChunkMultiple is the control: a disk that ends on a
// chunk boundary was read correctly before the fix and must still be.
func TestDifferencingVHDXExactChunkMultiple(t *testing.T) {
	checkSmallDiffChain(t, openSmallDiffChain(t, 4096*mb))
}

// TestVHDXGeometryBATCount holds the BAT size check to the entry counts of
// MS-VHDX 2.5, which differ by disk type, so that what is validated is what the
// parser reads.
func TestVHDXGeometryBATCount(t *testing.T) {
	// 4097 blocks at a chunk ratio of 4096: two chunks, the second partial.
	const (
		legacyBytes = (4097 + 2) * 8     // N + ceil(N/R), as v0.4.1 counted
		diffBytes   = 2 * (4096 + 1) * 8 // SectorBitmapBlocksCount * (R+1)
		dynBytes    = (4097 + 1) * 8     // N + floor((N-1)/R)
	)

	for _, c := range []struct {
		name       string
		diskType   types.DiskType
		regionSize int64
		wantErr    bool
	}{
		{"differencing region sized to the old count", types.DiskTypeDifferential, legacyBytes, true},
		{"differencing region sized to the spec count", types.DiskTypeDifferential, diffBytes, false},
		{"dynamic region sized to the spec count", types.DiskTypeDynamic, dynBytes, false},
		{"dynamic region one entry short", types.DiskTypeDynamic, dynBytes - 8, true},
		{"fixed region sized to the spec count", types.DiskTypeFixed, dynBytes, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &types.MetadataValues{
				BlockSize:         vhdxDiffBlockSize,
				DiskType:          c.diskType,
				LogicalSectorSize: vhdxDiffSectorSize,
				VirtualDiskSize:   vhdxDiffVirtual,
			}
			err := validateVHDXGeometry(m, vhdxBATRegionOff, c.regionSize, 16*mb)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateVHDXGeometry(region %d) = %v, want error %v", c.regionSize, err, c.wantErr)
			}
		})
	}
}
