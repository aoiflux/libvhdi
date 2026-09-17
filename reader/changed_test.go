// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// The question this tier answers is "which bytes did this checkpoint write?".
// It needs no filesystem knowledge, which is why it lives in the core, and it
// is enough on its own for delta imaging and targeted acquisition.
//
// The subtle half is deletion. A checkpoint that clears a region records it as
// zero or unmapped, and that is a write -- but it reads back identically to a
// region nothing ever touched. Without ExtentZeroedByChild the two are
// indistinguishable and change detection silently under-reports deletions.

const chgSize = 4 * 1024 * 1024
const chgBlock = 1024 * 1024

// chainOf writes base <- child into dir, with the child's blocks as given, and
// returns the child's path. A nil entry in blocks leaves the block
// unallocated, so it reads through to the parent.
func chainOf(t *testing.T, dir string, childBlocks []vhdBlock) string {
	t.Helper()

	parentID := [16]byte{0xDE, 0x11}
	base := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: chgSize,
		blockSize: chgBlock,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0xA0, chgBlock)},
			{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0xA1, chgBlock)},
			{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0xA2, chgBlock)},
			{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0xA3, chgBlock)},
		},
		selfID: parentID,
	})
	child := buildVHD(vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  chgSize,
		blockSize:  chgBlock,
		blocks:     childBlocks,
		parentName: "base.vhd",
		parentID:   parentID,
		selfID:     [16]byte{0xCD, 0x22},
	})

	if err := os.WriteFile(filepath.Join(dir, "base.vhd"), base, 0o600); err != nil {
		t.Fatalf("writing base: %v", err)
	}
	childPath := filepath.Join(dir, "child.avhd")
	if err := os.WriteFile(childPath, child, 0o600); err != nil {
		t.Fatalf("writing child: %v", err)
	}
	return childPath
}

func openChain(t *testing.T, path string) *VirtualDisk {
	t.Helper()
	d, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if d.NeedsParent() {
		t.Fatalf("chain did not resolve: %v", d.ParentResolveError())
	}
	return d
}

func TestChangedExtentsReportsOnlyWhatTheChildWrote(t *testing.T) {
	// The child writes block 1 and leaves the other three to the parent.
	childPath := chainOf(t, t.TempDir(), []vhdBlock{
		{allocated: false},
		{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0xC1, chgBlock)},
		{allocated: false},
		{allocated: false},
	})

	d := openChain(t, childPath)

	changed, err := d.ChangedExtents(context.Background(), 1)
	if err != nil {
		t.Fatalf("ChangedExtents: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("got %d changed extents, want 1: %+v", len(changed), changed)
	}

	e := changed[0]
	if e.VirtualOffset != chgBlock || e.Length != chgBlock {
		t.Fatalf("changed range is [%d,%d), want [%d,%d)",
			e.VirtualOffset, e.End(), chgBlock, 2*chgBlock)
	}
	if e.Kind != ExtentMapped {
		t.Fatalf("changed extent kind is %v, want %v", e.Kind, ExtentMapped)
	}
	if e.ChainIndex != 0 {
		t.Fatalf("changed extent came from chain index %d, want 0", e.ChainIndex)
	}

	written, unresolved, err := d.ChangedBytes(context.Background(), 1)
	if err != nil {
		t.Fatalf("ChangedBytes: %v", err)
	}
	if written != chgBlock {
		t.Fatalf("ChangedBytes reported %d written, want %d", written, chgBlock)
	}
	if unresolved != 0 {
		t.Fatalf("ChangedBytes reported %d unresolved bytes on a complete chain", unresolved)
	}
}

func TestUnchangedRangesAreNotReported(t *testing.T) {
	// A child that wrote nothing has changed nothing, even though every byte of
	// the device is backed by the parent.
	childPath := chainOf(t, t.TempDir(), []vhdBlock{
		{allocated: false}, {allocated: false}, {allocated: false}, {allocated: false},
	})

	d := openChain(t, childPath)

	changed, err := d.ChangedExtents(context.Background(), 1)
	if err != nil {
		t.Fatalf("ChangedExtents: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("a child that wrote nothing reports %d changed extents: %+v", len(changed), changed)
	}

	// The device is still entirely readable, which is what makes the empty
	// result meaningful rather than a failure to look.
	mapped, err := d.MappedBytes()
	if err != nil {
		t.Fatalf("MappedBytes: %v", err)
	}
	if mapped != chgSize {
		t.Fatalf("MappedBytes = %d, want the whole %d byte device", mapped, chgSize)
	}
}

// vhdxZeroedPair builds a parent holding data in block 0 and a differencing
// child that explicitly zeroes it.
//
// This has to be VHDX. A VHD differencing disk cannot express "explicitly
// zero" -- its sector bitmap only distinguishes "mine" from "the parent's" --
// so a cleared region on a VHD chain reads back as the parent's old contents
// and the clearing is invisible at the format level, not merely to this
// library.
func vhdxZeroedPair(t *testing.T, childState types.BlockState) (parent, child *VirtualDisk) {
	t.Helper()

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
		blockState:      childState,
	})

	parent, err := OpenVHDX(bytes.NewReader(parentImg), int64(len(parentImg)))
	if err != nil {
		t.Fatalf("opening the parent: %v", err)
	}
	t.Cleanup(func() { parent.Close() })

	child, err = OpenVHDX(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		t.Fatalf("opening the child: %v", err)
	}
	t.Cleanup(func() { child.Close() })

	if err := child.SetParent(parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	return parent, child
}

func TestAZeroedBlockCountsAsAChange(t *testing.T) {
	// The deletion case. The child records block 0 as PAYLOAD_BLOCK_ZERO, which
	// overrides the parent's data. Reading it back gives the same bytes as a
	// region nothing ever wrote -- so reported as a plain hole, the clearing
	// would be invisible.
	for _, tc := range []struct {
		name  string
		state types.BlockState
	}{
		{"PAYLOAD_BLOCK_ZERO", types.BlockStateZero},
		{"PAYLOAD_BLOCK_UNMAPPED", types.BlockStateUnmapped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, d := vhdxZeroedPair(t, tc.state)

			// The parent holds 0xA0 there, and the child's override wins.
			buf := make([]byte, 512)
			if _, err := d.ReadAt(buf, 0); err != nil {
				t.Fatalf("ReadAt: %v", err)
			}
			for _, b := range buf {
				if b != 0 {
					t.Fatalf("the cleared block reads as %#x, want zeroes", b)
				}
			}

			changed, err := d.ChangedExtents(context.Background(), 1)
			if err != nil {
				t.Fatalf("ChangedExtents: %v", err)
			}
			if len(changed) == 0 {
				t.Fatal("clearing a region was not reported as a change")
			}
			first := changed[0]
			if first.Kind != ExtentZeroedByChild {
				t.Fatalf("the cleared range has kind %v, want %v", first.Kind, ExtentZeroedByChild)
			}
			if first.VirtualOffset != 0 || first.Length != vhdxDiffBlockSize {
				t.Fatalf("the cleared range is [%d,%d), want [0,%d)",
					first.VirtualOffset, first.End(), vhdxDiffBlockSize)
			}

			// And it counts toward the delta, since a destination has to be
			// zeroed there rather than left alone.
			written, _, err := d.ChangedBytes(context.Background(), 1)
			if err != nil {
				t.Fatalf("ChangedBytes: %v", err)
			}
			if written < vhdxDiffBlockSize {
				t.Fatalf("ChangedBytes reported %d, want at least the %d cleared bytes",
					written, vhdxDiffBlockSize)
			}
		})
	}
}

func TestZeroedByChildIsDistinctFromNeverWritten(t *testing.T) {
	// The whole point of the new kind. Both ranges read as zeroes; only one is
	// a change.
	//
	// Block 0 is cleared by the child over a parent that holds data there.
	// Block 1 was never written by anyone. The two are indistinguishable to a
	// reader and must not be indistinguishable to a report.
	_, d := vhdxZeroedPair(t, types.BlockStateZero)

	extents, err := d.AllExtents()
	if err != nil {
		t.Fatalf("AllExtents: %v", err)
	}

	kindAt := func(off int64) ExtentKind {
		for _, e := range extents {
			if off >= e.VirtualOffset && off < e.End() {
				return e.Kind
			}
		}
		t.Fatalf("no extent covers offset %d", off)
		return ExtentZero
	}

	if got := kindAt(0); got != ExtentZeroedByChild {
		t.Errorf("the cleared block has kind %v, want %v", got, ExtentZeroedByChild)
	}
	if got := kindAt(vhdxDiffBlockSize); got != ExtentZero {
		t.Errorf("the never-written block has kind %v, want %v", got, ExtentZero)
	}

	// Both read as zeroes, which is exactly why the kinds have to differ.
	for _, off := range []int64{0, vhdxDiffBlockSize} {
		buf := make([]byte, 512)
		if _, err := d.ReadAt(buf, off); err != nil {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
		for _, b := range buf {
			if b != 0 {
				t.Fatalf("offset %d reads as %#x, want zeroes", off, b)
			}
		}
	}

	// And only the cleared one is a change.
	changed, err := d.ChangedExtents(context.Background(), 1)
	if err != nil {
		t.Fatalf("ChangedExtents: %v", err)
	}
	for _, e := range changed {
		if e.VirtualOffset >= vhdxDiffBlockSize {
			t.Fatalf("a never-written range at %d was reported as changed", e.VirtualOffset)
		}
	}
	if len(changed) != 1 {
		t.Fatalf("got %d changed extents, want only the cleared block: %+v", len(changed), changed)
	}
}

func TestVHDCannotExpressAnExplicitZero(t *testing.T) {
	// A limitation of the format, pinned so it is not mistaken for a bug here.
	//
	// The child allocates the block with an empty sector bitmap, which is the
	// closest a VHD can get to "I cleared this". The format reads that as "every
	// sector belongs to the parent", so the parent's old data comes back and no
	// change is reported. Deletion-aware change detection needs VHDX.
	childPath := chainOf(t, t.TempDir(), []vhdBlock{
		{allocated: false},
		{allocated: false},
		{allocated: true, presentSectors: nil, data: make([]byte, chgBlock)},
		{allocated: false},
	})

	d := openChain(t, childPath)

	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 2*chgBlock); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if buf[0] != 0xA2 {
		t.Fatalf("the block reads as %#x; a VHD child with an empty bitmap should "+
			"fall through to the parent's %#x", buf[0], 0xA2)
	}

	changed, err := d.ChangedExtents(context.Background(), 1)
	if err != nil {
		t.Fatalf("ChangedExtents: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("a VHD chain reported %d changes for a region it cannot express "+
			"as cleared: %+v", len(changed), changed)
	}
}

func TestChangedExtentsAcrossADeeperChain(t *testing.T) {
	// base <- cp1 <- cp2. Asking since index 1 gives what cp2 wrote; since
	// index 2 gives what cp2 and cp1 wrote together.
	dir := t.TempDir()

	baseID := [16]byte{0x10, 0x01}
	cp1ID := [16]byte{0x20, 0x02}

	base := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: chgSize,
		blockSize: chgBlock,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0x01, chgBlock)},
			{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0x02, chgBlock)},
			{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0x03, chgBlock)},
			{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0x04, chgBlock)},
		},
		selfID: baseID,
	})
	cp1 := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDifferential,
		mediaSize: chgSize,
		blockSize: chgBlock,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0x11, chgBlock)},
			{allocated: false}, {allocated: false}, {allocated: false},
		},
		parentName: "base.vhd",
		parentID:   baseID,
		selfID:     cp1ID,
	})
	cp2 := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDifferential,
		mediaSize: chgSize,
		blockSize: chgBlock,
		blocks: []vhdBlock{
			{allocated: false},
			{allocated: false},
			{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0x22, chgBlock)},
			{allocated: false},
		},
		parentName: "cp1.avhd",
		parentID:   cp1ID,
		selfID:     [16]byte{0x30, 0x03},
	})

	for name, img := range map[string][]byte{
		"base.vhd": base, "cp1.avhd": cp1, "cp2.avhd": cp2,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), img, 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	d := openChain(t, filepath.Join(dir, "cp2.avhd"))
	if got := d.ChainDepth(); got != 3 {
		t.Fatalf("ChainDepth() = %d, want 3", got)
	}

	// Since cp1: only what cp2 wrote, which is block 2.
	sinceCP1, err := d.ChangedExtents(context.Background(), 1)
	if err != nil {
		t.Fatalf("ChangedExtents(1): %v", err)
	}
	if len(sinceCP1) != 1 || sinceCP1[0].VirtualOffset != 2*chgBlock {
		t.Fatalf("since cp1 = %+v, want only block 2", sinceCP1)
	}

	// Since base: what cp2 and cp1 wrote, which is blocks 0 and 2.
	sinceBase, err := d.ChangedExtents(context.Background(), 2)
	if err != nil {
		t.Fatalf("ChangedExtents(2): %v", err)
	}
	if len(sinceBase) != 2 {
		t.Fatalf("since base = %+v, want blocks 0 and 2", sinceBase)
	}
	if sinceBase[0].VirtualOffset != 0 || sinceBase[1].VirtualOffset != 2*chgBlock {
		t.Fatalf("since base = %+v, want blocks 0 and 2", sinceBase)
	}

	// And by path, which is how a caller holding a checkpoint tree thinks.
	byPath, err := d.ChangedSince(context.Background(), filepath.Join(dir, "cp1.avhd"))
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	if len(byPath) != len(sinceCP1) || byPath[0].VirtualOffset != sinceCP1[0].VirtualOffset {
		t.Fatalf("ChangedSince(cp1) = %+v, want %+v", byPath, sinceCP1)
	}
}

func TestChangedSinceTheLeafIsEmpty(t *testing.T) {
	childPath := chainOf(t, t.TempDir(), []vhdBlock{
		{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0xC0, chgBlock)},
		{allocated: false}, {allocated: false}, {allocated: false},
	})

	d := openChain(t, childPath)

	changed, err := d.ChangedSince(context.Background(), childPath)
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("nothing can have been written since the leaf, got %+v", changed)
	}
}

func TestChangedSinceRejectsADiskOutsideTheChain(t *testing.T) {
	childPath := chainOf(t, t.TempDir(), []vhdBlock{
		{allocated: false}, {allocated: false}, {allocated: false}, {allocated: false},
	})

	d := openChain(t, childPath)

	_, err := d.ChangedSince(context.Background(), filepath.Join(t.TempDir(), "elsewhere.vhd"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ChangedSince returned %v, want ErrNotFound", err)
	}
}

func TestChangedExtentsRejectsANonPositiveIndex(t *testing.T) {
	// Index 0 is the disk itself. "What changed since this disk" has no
	// meaning, and silently returning everything or nothing would both be
	// wrong in a way the caller could not see.
	childPath := chainOf(t, t.TempDir(), []vhdBlock{
		{allocated: false}, {allocated: false}, {allocated: false}, {allocated: false},
	})

	d := openChain(t, childPath)

	for _, idx := range []int{0, -1} {
		if _, err := d.ChangedExtents(context.Background(), idx); err == nil {
			t.Errorf("ChangedExtents(%d) was accepted", idx)
		}
	}
}

func TestChangedExtentsReportsUnresolvedRangesSeparately(t *testing.T) {
	// A chain whose parent is missing. Those ranges are neither changed nor
	// unchanged: the disk that would say is absent, and calling them unchanged
	// would be a guess in the direction that loses evidence.
	dir := t.TempDir()
	childPath := chainOf(t, dir, []vhdBlock{
		{allocated: true, presentSectors: allSectors(chgBlock / 512), data: repeatByte(0xC5, chgBlock)},
		{allocated: false}, {allocated: false}, {allocated: false},
	})
	if err := os.Remove(filepath.Join(dir, "base.vhd")); err != nil {
		t.Fatalf("removing the base: %v", err)
	}

	d, err := OpenFile(childPath)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()
	if !d.NeedsParent() {
		t.Fatal("the chain resolved a parent that was deleted")
	}

	changed, err := d.ChangedExtents(context.Background(), 1)
	if err != nil {
		t.Fatalf("ChangedExtents: %v", err)
	}

	var write, unresolved int64
	for _, e := range changed {
		switch {
		case e.Kind == ExtentUnresolved:
			unresolved += e.Length
		case e.Kind.IsWrite():
			write += e.Length
		}
	}

	if write != chgBlock {
		t.Errorf("reported %d written bytes, want %d", write, chgBlock)
	}
	if unresolved != chgSize-chgBlock {
		t.Errorf("reported %d unresolved bytes, want %d", unresolved, chgSize-chgBlock)
	}

	_, unresolvedCount, err := d.ChangedBytes(context.Background(), 1)
	if err != nil {
		t.Fatalf("ChangedBytes: %v", err)
	}
	if unresolvedCount != unresolved {
		t.Errorf("ChangedBytes reported %d unresolved, want %d", unresolvedCount, unresolved)
	}
}

func TestChangedExtentsIsCancellable(t *testing.T) {
	childPath := chainOf(t, t.TempDir(), []vhdBlock{
		{allocated: false}, {allocated: false}, {allocated: false}, {allocated: false},
	})

	d := openChain(t, childPath)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := d.ChangedExtents(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("ChangedExtents returned %v, want context.Canceled", err)
	}
}

func TestExtentKindPredicates(t *testing.T) {
	for _, tc := range []struct {
		kind        ExtentKind
		readsAsZero bool
		isWrite     bool
	}{
		{ExtentZero, true, false},
		{ExtentMapped, false, true},
		{ExtentUnresolved, false, false},
		{ExtentZeroedByChild, true, true},
	} {
		if got := tc.kind.ReadsAsZero(); got != tc.readsAsZero {
			t.Errorf("%v.ReadsAsZero() = %v, want %v", tc.kind, got, tc.readsAsZero)
		}
		if got := tc.kind.IsWrite(); got != tc.isWrite {
			t.Errorf("%v.IsWrite() = %v, want %v", tc.kind, got, tc.isWrite)
		}
	}

	// The two zero kinds must render differently, or a report cannot show the
	// distinction the kinds exist to make.
	if ExtentZero.String() == ExtentZeroedByChild.String() {
		t.Fatal("ExtentZero and ExtentZeroedByChild render identically")
	}
}
