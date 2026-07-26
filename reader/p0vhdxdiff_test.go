// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"errors"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// vhdxDiffFixtures builds a matched parent/child VHDX pair.
//
// chunkRatio for a 1 MB block size and 512-byte sectors is 4096, so the sector
// bitmap entry for chunk 0 lands at BAT index 4096. The virtual disk therefore
// has to span more than 4096 blocks for that entry to exist at all — hence the
// 4097 MB virtual size. Only block 0 is actually allocated, so the images stay
// small.
const (
	vhdxDiffBlockSize  = 1 * mb
	vhdxDiffSectorSize = 512
	vhdxDiffVirtual    = uint64(4097) * mb
)

func vhdxDiffFixtures(t *testing.T, presentSectors []int) (parent, child []byte) {
	t.Helper()

	// Parent: dynamic disk, block 0 fully present, filled with 0xA0.
	parent = buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		hasParent:       false,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
	})

	// Child: differencing disk, block 0 PARTIALLY_PRESENT. Only the sectors
	// listed in presentSectors carry child data (0xC0); the rest of the child's
	// block is zero on disk, which is exactly why those sectors must resolve to
	// the parent rather than to the child.
	childPayload := make([]byte, vhdxDiffBlockSize)
	for _, s := range presentSectors {
		copy(childPayload[s*vhdxDiffSectorSize:(s+1)*vhdxDiffSectorSize],
			repeatByte(0xC0, vhdxDiffSectorSize))
	}
	child = buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		hasParent:       true,
		blockState:      types.BlockStatePartiallyAllocated,
		presentSectors:  presentSectors,
		payload:         childPayload,
	})

	return parent, child
}

// TestVHDXFixtureOpens is a guard: if the fixture itself is malformed the
// correctness tests below would fail for the wrong reason.
func TestVHDXFixtureOpens(t *testing.T) {
	parentImg, childImg := vhdxDiffFixtures(t, []int{0, 2, 4})

	p, err := OpenVHDX(bytes.NewReader(parentImg), int64(len(parentImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(parent): %v", err)
	}
	defer p.Close()
	if p.DiskType() != types.DiskTypeDynamic {
		t.Errorf("parent DiskType() = %d, want dynamic", p.DiskType())
	}
	if p.Size() != vhdxDiffVirtual {
		t.Errorf("parent Size() = %d, want %d", p.Size(), vhdxDiffVirtual)
	}

	c, err := OpenVHDX(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(child): %v", err)
	}
	defer c.Close()
	if !c.IsDifferencing() {
		t.Error("child IsDifferencing() = false, want true")
	}
	if got := c.ParentFilename(); got != "parent.vhdx" {
		t.Errorf("child ParentFilename() = %q, want \"parent.vhdx\"", got)
	}
}

// TestDifferencingVHDX_SectorLevelParentFallthrough is the VHDX counterpart to
// the VHD case. PAYLOAD_BLOCK_PARTIALLY_PRESENT (state 7) occurs only in
// differencing files: sectors whose bitmap bit is clear must be served from the
// parent, not zero-filled.
func TestDifferencingVHDX_SectorLevelParentFallthrough(t *testing.T) {
	present := []int{0, 2, 4}
	parentImg, childImg := vhdxDiffFixtures(t, present)

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

	const testSectors = 6
	buf := make([]byte, testSectors*vhdxDiffSectorSize)
	if _, err := child.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	for s := 0; s < testSectors; s++ {
		got := buf[s*vhdxDiffSectorSize : (s+1)*vhdxDiffSectorSize]
		want := byte(0xA0) // from parent
		if s%2 == 0 {
			want = 0xC0 // present in child
		}
		if !bytes.Equal(got, repeatByte(want, vhdxDiffSectorSize)) {
			t.Errorf("sector %d = %#x..., want fill %#x", s, got[:4], want)
		}
	}
}

// TestDifferencingVHDX_UnwiredParentFailsClosed mirrors the VHD case: a state-7
// block with no parent attached must error rather than yield zeroes.
func TestDifferencingVHDX_UnwiredParentFailsClosed(t *testing.T) {
	_, childImg := vhdxDiffFixtures(t, []int{0, 2, 4})

	child, err := OpenVHDX(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(child): %v", err)
	}
	defer child.Close()

	if !child.NeedsParent() {
		t.Error("NeedsParent() = false, want true")
	}

	// Sector 1 is absent from the child and so resolves to the parent.
	buf := make([]byte, vhdxDiffSectorSize)
	if _, err := child.ReadAt(buf, vhdxDiffSectorSize); !errors.Is(err, ErrParentRequired) {
		t.Fatalf("ReadAt error = %v, want ErrParentRequired", err)
	}
}

// TestDifferencingVHDX_NotPresentBlockResolvesToParent covers state 0
// (PAYLOAD_BLOCK_NOT_PRESENT), where the whole block belongs to the parent.
func TestDifferencingVHDX_NotPresentBlockResolvesToParent(t *testing.T) {
	parentImg := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
	})
	childImg := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		hasParent:       true,
		blockState:      types.BlockStateNone,
	})

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

	buf := make([]byte, 512)
	if _, err := child.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, repeatByte(0xA0, 512)) {
		t.Errorf("state-0 block = %#x..., want 0xA0 from parent", buf[:4])
	}
}
