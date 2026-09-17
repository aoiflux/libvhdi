// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// Gaps the existing chain tests leave. Cycle detection is the notable one: the
// fixtures carry a comment saying a self-referential chain should be caught,
// and nothing ever built one.

const gapSize = 1024 * 1024

// writeChainDisk writes one image of a chain and returns its path.
func writeChainDisk(t *testing.T, dir, name string, p vhdImageParams) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buildVHD(p), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

func TestCycleIsDetectedNotWalkedForever(t *testing.T) {
	// A points at B and B points back at A. Without cycle detection the
	// resolver walks between them until it hits the depth limit, which reports
	// the wrong reason -- "too deep" rather than "this chain refers to itself".
	dir := t.TempDir()

	idA := [16]byte{0xAA, 0x01}
	idB := [16]byte{0xBB, 0x02}

	writeChainDisk(t, dir, "a.vhd", vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  gapSize,
		blockSize:  gapSize,
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, gapSize)}},
		parentName: "b.vhd",
		parentID:   idB,
		selfID:     idA,
	})
	writeChainDisk(t, dir, "b.vhd", vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  gapSize,
		blockSize:  gapSize,
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, gapSize)}},
		parentName: "a.vhd",
		parentID:   idA,
		selfID:     idB,
	})

	// Best effort: the disk opens with the cycle recorded rather than walked.
	d, err := OpenFile(filepath.Join(dir, "a.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if err := d.ParentResolveError(); !errors.Is(err, ErrChainCycle) {
		t.Fatalf("ParentResolveError() = %v, want ErrChainCycle", err)
	}

	// The chain must be bounded, not merely terminated. Two links is a
	// self-referential pair; anything longer means the loop was walked.
	if got := d.ChainDepth(); got > 2 {
		t.Fatalf("ChainDepth() = %d for a two-image cycle", got)
	}
}

func TestCycleFailsLoudlyWithRequireParentChain(t *testing.T) {
	dir := t.TempDir()

	idA := [16]byte{0xAA, 0x03}
	idB := [16]byte{0xBB, 0x04}

	writeChainDisk(t, dir, "a.vhd", vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  gapSize,
		blockSize:  gapSize,
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, gapSize)}},
		parentName: "b.vhd",
		parentID:   idB,
		selfID:     idA,
	})
	writeChainDisk(t, dir, "b.vhd", vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  gapSize,
		blockSize:  gapSize,
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, gapSize)}},
		parentName: "a.vhd",
		parentID:   idA,
		selfID:     idB,
	})

	_, err := OpenFileWith(filepath.Join(dir, "a.vhd"), &Options{RequireParentChain: true})
	if !errors.Is(err, ErrChainCycle) {
		t.Fatalf("OpenFileWith returned %v, want ErrChainCycle", err)
	}
}

func TestSelfReferentialDiskIsACycle(t *testing.T) {
	// The degenerate case: a disk naming itself as its own parent.
	dir := t.TempDir()
	id := [16]byte{0xCC, 0x05}

	writeChainDisk(t, dir, "self.vhd", vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  gapSize,
		blockSize:  gapSize,
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, gapSize)}},
		parentName: "self.vhd",
		parentID:   id,
		selfID:     id,
	})

	d, err := OpenFile(filepath.Join(dir, "self.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if err := d.ParentResolveError(); !errors.Is(err, ErrChainCycle) {
		t.Fatalf("ParentResolveError() = %v, want ErrChainCycle", err)
	}
	if got := d.ChainDepth(); got != 1 {
		t.Fatalf("ChainDepth() = %d, want 1 -- the disk must not attach to itself", got)
	}
}

func TestFiveDeepChainResolves(t *testing.T) {
	// The existing tests stop at three. A checkpoint tree several days old is
	// routinely deeper, and each extra link is another level of recursion in
	// the extent walk.
	dir := t.TempDir()

	const depth = 5
	ids := make([][16]byte, depth)
	for i := range ids {
		ids[i] = [16]byte{0xD0, byte(i)}
	}

	// ids[0] is the base; each subsequent image differences against the one
	// before it.
	writeChainDisk(t, dir, "d0.vhd", vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: gapSize,
		blockSize: gapSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: allSectors(gapSize / 512), data: repeatByte(0xE0, gapSize)},
		},
		selfID: ids[0],
	})
	for i := 1; i < depth; i++ {
		writeChainDisk(t, dir, "d"+string(rune('0'+i))+".vhd", vhdImageParams{
			diskType:   types.DiskTypeDifferential,
			mediaSize:  gapSize,
			blockSize:  gapSize,
			blocks:     []vhdBlock{{allocated: true, data: make([]byte, gapSize)}},
			parentName: "d" + string(rune('0'+i-1)) + ".vhd",
			parentID:   ids[i-1],
			selfID:     ids[i],
		})
	}

	d, err := OpenFile(filepath.Join(dir, "d4.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("chain did not resolve: %v", d.ParentResolveError())
	}
	if got := d.ChainDepth(); got != depth {
		t.Fatalf("ChainDepth() = %d, want %d", got, depth)
	}

	// Every link wrote nothing, so the base's bytes must reach the leaf through
	// four levels of resolution.
	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if buf[0] != 0xE0 {
		t.Fatalf("first byte is %#x, want the base's %#x", buf[0], 0xE0)
	}

	// And the extent map has to agree with the read about which file supplies
	// the bytes.
	extents, err := d.AllExtents()
	if err != nil {
		t.Fatalf("AllExtents: %v", err)
	}
	if len(extents) == 0 {
		t.Fatal("no extents for a fully backed device")
	}
	if extents[0].ChainIndex != depth-1 {
		t.Fatalf("the first extent comes from chain index %d, want the base at %d",
			extents[0].ChainIndex, depth-1)
	}
}

func TestParentWithDifferentVirtualSizeIsRejected(t *testing.T) {
	// Every link in a valid chain reports the same virtual size. A mismatch
	// means the located image is not this child's parent, whatever its
	// identifier says -- and attaching it would produce a device whose length
	// disagrees with its own contents.
	dir := t.TempDir()
	parentID := [16]byte{0xF0, 0x06}

	writeChainDisk(t, dir, "parent.vhd", vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: 2 * gapSize, // deliberately different
		blockSize: gapSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: allSectors(gapSize / 512), data: repeatByte(0xF1, gapSize)},
			{allocated: false},
		},
		selfID: parentID,
	})
	writeChainDisk(t, dir, "child.vhd", vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  gapSize,
		blockSize:  gapSize,
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, gapSize)}},
		parentName: "parent.vhd",
		parentID:   parentID,
		selfID:     [16]byte{0xF2, 0x07},
	})

	d, err := OpenFile(filepath.Join(dir, "child.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if !d.NeedsParent() {
		t.Fatal("a parent of the wrong virtual size was attached")
	}
	if err := d.ParentResolveError(); !errors.Is(err, ErrParentMismatch) {
		t.Fatalf("ParentResolveError() = %v, want ErrParentMismatch", err)
	}
}

func TestVHDXHeaderSelectionPrefersTheHigherSequence(t *testing.T) {
	// The fixtures always write sequence 1 then 2, so the second header wins by
	// default and the selection logic is never actually exercised. Here the
	// FIRST header carries the higher sequence.
	img := buildVHDX(vhdxParams{
		virtualDiskSize: 2 * 1024 * 1024,
		blockSize:       1024 * 1024,
		sectorSize:      512,
	})

	// Rewrite header 1 with a high sequence and header 2 with a low one.
	writeVHDXImageHeaderWith(img, types.VHDXFirstHeaderOffset, 9, false, [16]byte{})
	writeVHDXImageHeaderWith(img, types.VHDXSecondHeaderOffset, 2, false, [16]byte{})

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	ihp := NewVHDXImageHeaderParser(bytes.NewReader(img))
	chosen, err := ihp.ReadImageHeader()
	if err != nil {
		t.Fatalf("ReadImageHeader: %v", err)
	}
	if chosen.SequenceNumber != 9 {
		t.Fatalf("selected sequence %d, want the higher 9", chosen.SequenceNumber)
	}
}

func TestVHDXHeaderSelectionSkipsACorruptHigherSequence(t *testing.T) {
	// A higher sequence number does not win if that header does not verify.
	// Selecting it would prefer a damaged structure over an intact one.
	img := buildVHDX(vhdxParams{
		virtualDiskSize: 2 * 1024 * 1024,
		blockSize:       1024 * 1024,
		sectorSize:      512,
	})

	writeVHDXImageHeaderWith(img, types.VHDXFirstHeaderOffset, 9, false, [16]byte{})
	writeVHDXImageHeaderWith(img, types.VHDXSecondHeaderOffset, 2, false, [16]byte{})
	// Break header 1's checksum after writing it.
	img[types.VHDXFirstHeaderOffset+4] ^= 0xFF

	ihp := NewVHDXImageHeaderParser(bytes.NewReader(img))
	chosen, err := ihp.ReadImageHeader()
	if err != nil {
		t.Fatalf("ReadImageHeader: %v", err)
	}
	if chosen.SequenceNumber != 2 {
		t.Fatalf("selected sequence %d, want 2 -- the intact header", chosen.SequenceNumber)
	}

	// And the image still opens, from the surviving header.
	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	d.Close()
}

func TestParentFormatIsDetectedNotInherited(t *testing.T) {
	// Some tools produce a VHDX child over a VHD parent, so a parent's format
	// must never be assumed from its child's.
	//
	// This is now structural rather than conditional: openParent takes no child
	// format at all and always reads the parent's own signature. Before v0.3.0
	// it accepted a childFormat argument and ignored it, while its documentation
	// claimed it preferred it -- a discrepancy that would have become a bug the
	// moment anyone made the parameter do what the comment said.
	//
	// What this test pins is that a resolved parent's reported format comes from
	// the parent itself. A genuinely cross-format fixture would be better still;
	// building one is noted as future work.
	dir := t.TempDir()
	parentID := [16]byte{0x1A, 0x08}

	// A VHD parent.
	writeChainDisk(t, dir, "parent.vhd", vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: gapSize,
		blockSize: gapSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: allSectors(gapSize / 512), data: repeatByte(0x1B, gapSize)},
		},
		selfID: parentID,
	})

	// A child pointing at it, resolved through the ordinary open path so the
	// resolver's format detection is what runs.
	childPath := writeChainDisk(t, dir, "child.vhd", vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  gapSize,
		blockSize:  gapSize,
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, gapSize)}},
		parentName: "parent.vhd",
		parentID:   parentID,
		selfID:     [16]byte{0x1C, 0x09},
	})

	d, err := OpenFile(childPath)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("chain did not resolve: %v", d.ParentResolveError())
	}

	// The parent's format is reported from the parent, and openParent's
	// signature carries nothing that could override it.
	if got := d.Parent().Format(); got != types.FileFormatVHD {
		t.Fatalf("parent format = %v, want VHD", got)
	}
	var _ func(ParentSource, bool) (*VirtualDisk, error) = openParent

	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if buf[0] != 0x1B {
		t.Fatalf("first byte is %#x, want the parent's %#x", buf[0], 0x1B)
	}
}

func TestParentWithAnUnreplayableLogIsRefused(t *testing.T) {
	// A dirty parent makes the whole device suspect, not just the parent: the
	// child's reads fall through to structures that may be stale. Refusing by
	// default is the same fail-closed rule the rest of the library follows.
	dir := t.TempDir()

	parentImg := buildVHDX(vhdxParams{
		virtualDiskSize: vhdxDiffVirtual,
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0x2A, vhdxDiffBlockSize),
		dirtyLog:        true,
	})

	path := filepath.Join(dir, "dirty.vhdx")
	if err := os.WriteFile(path, parentImg, 0o600); err != nil {
		t.Fatalf("writing the image: %v", err)
	}

	if _, err := OpenFile(path); !errors.Is(err, ErrDirtyImage) {
		t.Fatalf("OpenFile returned %v, want ErrDirtyImage", err)
	}

	// And the caller can take it anyway, with the state reported.
	d, err := OpenFileWith(path, &Options{AllowDirtyImage: true})
	if err != nil {
		t.Fatalf("OpenFileWith(AllowDirtyImage): %v", err)
	}
	defer d.Close()

	if !d.IsDirty() {
		t.Fatal("an image opened with an unreplayed log does not report as dirty")
	}
	if !d.HasLog() {
		t.Fatal("HasLog() is false for an image carrying a log")
	}
	if d.LogReplayed() {
		t.Fatal("LogReplayed() is true for a log that could not be replayed")
	}
}
