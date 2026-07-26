// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"errors"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// ============================================================================
// 4096-byte logical sectors
//
// The logical sector size feeds two pieces of arithmetic that 512-byte fixtures
// never exercise:
//
//	chunkRatio      = 2^23 * logicalSectorSize / blockSize
//	sectorsPerBlock = blockSize / logicalSectorSize
//
// With a 1 MB block, moving from 512 to 4096 bytes takes the chunk ratio from
// 4096 to 32768 and sectors-per-block from 2048 to 256. That relocates the
// chunk-0 sector bitmap entry from BAT index 4096 to 32768 and changes every bit
// position within the bitmap, so the whole 4K path through the BAT parser and the
// differencing resolver is separate code from the 512-byte path.
// ============================================================================

const (
	vhdx4kBlockSize  = 1 * mb
	vhdx4kSectorSize = 4096

	// chunkRatio is 32768 for these parameters, so the disk has to span more
	// than 32768 blocks for the chunk-0 bitmap entry to exist at all.
	vhdx4kVirtual = uint64(32769) * mb
)

// vhdx4kFixtures builds a matched parent/child VHDX pair with 4096-byte logical
// sectors. The child holds 0xC0 in presentSectors; the parent holds 0xA0
// everywhere.
func vhdx4kFixtures(presentSectors []int) (parent, child []byte) {
	parent = buildVHDX(vhdxParams{
		blockSize:       vhdx4kBlockSize,
		sectorSize:      vhdx4kSectorSize,
		virtualDiskSize: vhdx4kVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdx4kBlockSize),
	})

	childPayload := make([]byte, vhdx4kBlockSize)
	for _, s := range presentSectors {
		copy(childPayload[s*vhdx4kSectorSize:(s+1)*vhdx4kSectorSize],
			repeatByte(0xC0, vhdx4kSectorSize))
	}
	child = buildVHDX(vhdxParams{
		blockSize:       vhdx4kBlockSize,
		sectorSize:      vhdx4kSectorSize,
		virtualDiskSize: vhdx4kVirtual,
		hasParent:       true,
		blockState:      types.BlockStatePartiallyAllocated,
		presentSectors:  presentSectors,
		payload:         childPayload,
	})

	return parent, child
}

// TestVHDX4kSectorGeometry checks the reported geometry and a plain read, so a
// failure in the differencing tests below can be attributed to resolution rather
// than to basic parsing.
func TestVHDX4kSectorGeometry(t *testing.T) {
	img := buildVHDX(vhdxParams{
		blockSize:       vhdx4kBlockSize,
		sectorSize:      vhdx4kSectorSize,
		virtualDiskSize: vhdx4kVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdx4kBlockSize),
	})

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	if d.SectorSize() != vhdx4kSectorSize {
		t.Errorf("SectorSize() = %d, want %d", d.SectorSize(), vhdx4kSectorSize)
	}
	if d.BlockSize() != vhdx4kBlockSize {
		t.Errorf("BlockSize() = %d, want %d", d.BlockSize(), vhdx4kBlockSize)
	}
	if d.Size() != vhdx4kVirtual {
		t.Errorf("Size() = %d, want %d", d.Size(), vhdx4kVirtual)
	}

	buf := make([]byte, vhdx4kSectorSize)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, repeatByte(0xA0, vhdx4kSectorSize)) {
		t.Errorf("block 0 = %#x..., want 0xA0", buf[:4])
	}

	// Block 1 onward is unallocated, so it must read as zeroes.
	if _, err := d.ReadAt(buf, vhdx4kBlockSize); err != nil {
		t.Fatalf("ReadAt block 1: %v", err)
	}
	if !bytes.Equal(buf, make([]byte, vhdx4kSectorSize)) {
		t.Errorf("block 1 = %#x..., want zeroes", buf[:4])
	}
}

// TestVHDX4kDifferencingSectorFallthrough is the case the 512-byte tests cannot
// reach: per-sector parent resolution where a sector is 4096 bytes and the chunk
// ratio is 32768.
func TestVHDX4kDifferencingSectorFallthrough(t *testing.T) {
	present := []int{0, 2, 4}
	parentImg, childImg := vhdx4kFixtures(present)

	parent, err := OpenVHDX(bytes.NewReader(parentImg), int64(len(parentImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(parent): %v", err)
	}
	defer parent.Close()

	child, err := OpenVHDX(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(child): %v", err)
	}
	defer child.Close()

	if !child.IsDifferencing() {
		t.Fatal("child IsDifferencing() = false")
	}
	if err := child.SetParent(parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}

	const testSectors = 6
	buf := make([]byte, testSectors*vhdx4kSectorSize)
	if _, err := child.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	for s := 0; s < testSectors; s++ {
		got := buf[s*vhdx4kSectorSize : (s+1)*vhdx4kSectorSize]
		want := byte(0xA0) // from parent
		if s%2 == 0 {
			want = 0xC0 // present in child
		}
		if !bytes.Equal(got, repeatByte(want, vhdx4kSectorSize)) {
			t.Errorf("sector %d = %#x..., want fill %#x", s, got[:4], want)
		}
	}
}

// TestVHDX4kDifferencingHighSectorIndex probes the far end of the block's bitmap.
// With 4096-byte sectors a 1 MB block holds 256 sectors, so sector 255 is the
// last bit of the block's 32-byte slice of the chunk bitmap — the position most
// likely to expose an off-by-one in the bit arithmetic.
func TestVHDX4kDifferencingHighSectorIndex(t *testing.T) {
	const sectorsPerBlock = vhdx4kBlockSize / vhdx4kSectorSize // 256
	present := []int{sectorsPerBlock - 2, sectorsPerBlock - 1} // 254, 255

	parentImg, childImg := vhdx4kFixtures(present)

	parent, err := OpenVHDX(bytes.NewReader(parentImg), int64(len(parentImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(parent): %v", err)
	}
	defer parent.Close()
	child, err := OpenVHDX(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(child): %v", err)
	}
	defer child.Close()
	if err := child.SetParent(parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}

	// Read the final four sectors of block 0: 252 and 253 belong to the parent,
	// 254 and 255 to the child.
	start := int64(sectorsPerBlock-4) * vhdx4kSectorSize
	buf := make([]byte, 4*vhdx4kSectorSize)
	if _, err := child.ReadAt(buf, start); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	for i, want := range []byte{0xA0, 0xA0, 0xC0, 0xC0} {
		got := buf[i*vhdx4kSectorSize : (i+1)*vhdx4kSectorSize]
		if !bytes.Equal(got, repeatByte(want, vhdx4kSectorSize)) {
			t.Errorf("sector %d = %#x..., want fill %#x", sectorsPerBlock-4+i, got[:4], want)
		}
	}
}

// TestVHDX4kUnwiredParentFailsClosed checks the fail-closed behaviour holds on
// the 4K path too.
func TestVHDX4kUnwiredParentFailsClosed(t *testing.T) {
	_, childImg := vhdx4kFixtures([]int{0, 2, 4})

	child, err := OpenVHDX(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(child): %v", err)
	}
	defer child.Close()

	// Sector 1 is absent from the child, so it resolves to the missing parent.
	buf := make([]byte, vhdx4kSectorSize)
	if _, err := child.ReadAt(buf, vhdx4kSectorSize); !errors.Is(err, ErrParentRequired) {
		t.Fatalf("ReadAt error = %v, want ErrParentRequired", err)
	}
}

// TestVHDX4kExtentsPerSectorProvenance checks the extent map's sector arithmetic
// at 4096 bytes, where extent boundaries land on different offsets than the
// 512-byte fixtures produce.
func TestVHDX4kExtentsPerSectorProvenance(t *testing.T) {
	parentImg, childImg := vhdx4kFixtures([]int{0, 2, 4})

	parent, err := OpenVHDX(bytes.NewReader(parentImg), int64(len(parentImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(parent): %v", err)
	}
	defer parent.Close()
	child, err := OpenVHDX(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(child): %v", err)
	}
	defer child.Close()
	if err := child.SetParent(parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}

	const probe = 6 * vhdx4kSectorSize
	extents, err := child.Extents(0, probe)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}
	checkExtentsTile(t, extents, 0, probe)

	if len(extents) != 6 {
		t.Fatalf("got %d extents, want 6 alternating 4 KB sectors: %+v", len(extents), extents)
	}
	for i, e := range extents {
		wantChain := 1
		if i%2 == 0 {
			wantChain = 0
		}
		if e.ChainIndex != wantChain {
			t.Errorf("extent %d ChainIndex = %d, want %d", i, e.ChainIndex, wantChain)
		}
		if e.Length != vhdx4kSectorSize {
			t.Errorf("extent %d length = %d, want %d", i, e.Length, vhdx4kSectorSize)
		}
	}
}

// TestVHDXRejectsInvalidLogicalSectorSize guards the validation: only 512 and
// 4096 are legal, and a virtual size that is not a multiple of the sector size is
// inconsistent.
func TestVHDXRejectsInvalidLogicalSectorSize(t *testing.T) {
	// 1024 is not a permitted logical sector size.
	img := buildVHDX(vhdxParams{
		blockSize:       vhdx4kBlockSize,
		sectorSize:      1024,
		virtualDiskSize: vhdx4kVirtual,
		blockState:      types.BlockStateFullyAllocated,
	})
	if _, err := OpenVHDX(bytes.NewReader(img), int64(len(img))); err == nil {
		t.Error("OpenVHDX accepted a 1024-byte logical sector size")
	}

	// A virtual disk size that is not a whole number of 4 KB sectors.
	img = buildVHDX(vhdxParams{
		blockSize:       vhdx4kBlockSize,
		sectorSize:      vhdx4kSectorSize,
		virtualDiskSize: vhdx4kVirtual + 512,
		blockState:      types.BlockStateFullyAllocated,
	})
	if _, err := OpenVHDX(bytes.NewReader(img), int64(len(img))); !errors.Is(err, ErrCorruptImage) {
		t.Errorf("OpenVHDX error = %v, want ErrCorruptImage for a size that is not sector aligned", err)
	}
}
