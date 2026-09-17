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

// Chain walks upward from one image to its ancestors, which is the wrong
// direction for checkpoints. Checkpoints branch: applying an earlier checkpoint
// and continuing produces two children of the same parent, and from a leaf you
// cannot see the sibling. These tests are mostly about that shape.

const chkSize = 1024 * 1024

// writeDisk writes a base or differencing VHD into dir and returns its path.
func writeDisk(t *testing.T, dir, name string, selfID, parentID [16]byte, parentName string) string {
	t.Helper()

	diskType := types.DiskTypeDynamic
	blocks := []vhdBlock{
		{allocated: true, presentSectors: []int{0}, data: repeatByte(selfID[0], chkSize)},
	}
	if parentName != "" {
		diskType = types.DiskTypeDifferential
		// A checkpoint that has written nothing still reads through to its
		// parent, which is what makes the lineage worth opening.
		blocks = []vhdBlock{{allocated: true, data: make([]byte, chkSize)}}
	}

	img := buildVHD(vhdImageParams{
		diskType:   diskType,
		mediaSize:  chkSize,
		blockSize:  chkSize,
		blocks:     blocks,
		parentName: parentName,
		parentID:   parentID,
		selfID:     selfID,
	})

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// Distinct identifiers for the checkpoint fixtures. idBase lives in
// open_test.go, which every test in this package shares.
var (
	idCP1 = [16]byte{0xC1, 0x02}
	idCP2 = [16]byte{0xC2, 0x03}
	idCP3 = [16]byte{0xC3, 0x04}
)

// linearTree writes base <- cp1 <- cp2.
func linearTree(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	writeDisk(t, dir, "base.vhd", idBase, [16]byte{}, "")
	writeDisk(t, dir, "cp1.avhd", idCP1, idBase, "base.vhd")
	writeDisk(t, dir, "cp2.avhd", idCP2, idCP1, "cp1.avhd")
	return dir
}

func discover(t *testing.T, dir string) *Tree {
	t.Helper()
	tree, err := DiscoverChain(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("DiscoverChain: %v", err)
	}
	return tree
}

func TestDiscoverChainBuildsALinearChain(t *testing.T) {
	tree := discover(t, linearTree(t))

	if len(tree.Nodes) != 3 {
		t.Fatalf("found %d images, want 3", len(tree.Nodes))
	}

	roots := tree.Roots()
	if len(roots) != 1 {
		t.Fatalf("found %d roots, want 1: %v", len(roots), roots)
	}
	if filepath.Base(roots[0].Path) != "base.vhd" {
		t.Fatalf("root is %s, want base.vhd", roots[0])
	}
	if roots[0].Role() != RoleBase {
		t.Fatalf("root role is %v, want %v", roots[0].Role(), RoleBase)
	}

	leaves := tree.Leaves()
	if len(leaves) != 1 {
		t.Fatalf("found %d leaves, want 1: %v", len(leaves), leaves)
	}
	if filepath.Base(leaves[0].Path) != "cp2.avhd" {
		t.Fatalf("leaf is %s, want cp2.avhd", leaves[0])
	}
	if tree.Branched() {
		t.Fatal("a linear chain reports as branched")
	}
}

func TestDiscoverChainSeesBranches(t *testing.T) {
	// The case Chain cannot express. Applying cp1 and continuing gives cp1 two
	// children, and from either leaf the sibling is invisible.
	dir := t.TempDir()
	writeDisk(t, dir, "base.vhd", idBase, [16]byte{}, "")
	writeDisk(t, dir, "cp1.avhd", idCP1, idBase, "base.vhd")
	writeDisk(t, dir, "branch-a.avhd", idCP2, idCP1, "cp1.avhd")
	writeDisk(t, dir, "branch-b.avhd", idCP3, idCP1, "cp1.avhd")

	tree := discover(t, dir)

	if !tree.Branched() {
		t.Fatal("a branched tree does not report as branched")
	}

	leaves := tree.Leaves()
	if len(leaves) != 2 {
		t.Fatalf("found %d leaves, want 2: %v", len(leaves), leaves)
	}

	cp1, err := tree.Find(idCP1)
	if err != nil {
		t.Fatalf("Find(cp1): %v", err)
	}
	if len(cp1.Children) != 2 {
		t.Fatalf("cp1 has %d children, want 2", len(cp1.Children))
	}
	// Children are ordered by path so a scan is reproducible.
	if filepath.Base(cp1.Children[0].Path) != "branch-a.avhd" {
		t.Fatalf("children are not ordered by path: %v", cp1.Children)
	}

	// One lineage per leaf, each a complete device in its own right.
	lineages := tree.Lineages()
	if len(lineages) != 2 {
		t.Fatalf("found %d lineages, want 2", len(lineages))
	}
	for _, l := range lineages {
		if !l.Complete() {
			t.Errorf("lineage %v is incomplete", l.Paths())
		}
		if len(l.Nodes) != 3 {
			t.Errorf("lineage %v has %d nodes, want 3", l.Paths(), len(l.Nodes))
		}
	}
}

func TestLineageIsOrderedRootFirst(t *testing.T) {
	tree := discover(t, linearTree(t))

	l := tree.Lineage(tree.Leaves()[0])
	got := []string{}
	for _, p := range l.Paths() {
		got = append(got, filepath.Base(p))
	}

	want := []string{"base.vhd", "cp1.avhd", "cp2.avhd"}
	if len(got) != len(want) {
		t.Fatalf("lineage = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lineage = %v, want %v", got, want)
		}
	}

	if filepath.Base(l.Root().Path) != "base.vhd" {
		t.Errorf("Root() = %s, want base.vhd", l.Root())
	}
	if filepath.Base(l.Leaf().Path) != "cp2.avhd" {
		t.Errorf("Leaf() = %s, want cp2.avhd", l.Leaf())
	}
	if n := len(l.Checkpoints()); n != 2 {
		t.Errorf("Checkpoints() returned %d, want 2", n)
	}
}

func TestLineageOpenPresentsTheMergedView(t *testing.T) {
	// Opening the leaf resolves the whole chain. This is the point of the tree:
	// having found a device, reading it is one call.
	tree := discover(t, linearTree(t))

	l := tree.Lineage(tree.Leaves()[0])
	d, err := l.Open(nil)
	if err != nil {
		t.Fatalf("Lineage.Open: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("the merged view is missing a parent: %v", d.ParentResolveError())
	}
	if got := d.ChainDepth(); got != 3 {
		t.Fatalf("ChainDepth() = %d, want 3", got)
	}

	// The checkpoints wrote nothing, so every byte comes from the base.
	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if buf[0] != idBase[0] {
		t.Fatalf("first byte is %#x, want the base's %#x", buf[0], idBase[0])
	}
}

func TestIncompleteLineageIsReportedAsSuch(t *testing.T) {
	// A directory holding checkpoints but not the base they rest on. The
	// images present cannot reconstruct the device, and saying so is the whole
	// value of Complete.
	dir := t.TempDir()
	writeDisk(t, dir, "cp1.avhd", idCP1, idBase, "base.vhd")
	writeDisk(t, dir, "cp2.avhd", idCP2, idCP1, "cp1.avhd")

	tree := discover(t, dir)

	roots := tree.Roots()
	if len(roots) != 1 {
		t.Fatalf("found %d roots, want 1", len(roots))
	}
	// The root is itself a differencing disk, which is what marks the chain as
	// missing its base rather than merely short.
	if !roots[0].Info.IsDifferencing() {
		t.Fatal("the root of a baseless chain does not report as differencing")
	}

	l := tree.Lineage(tree.Leaves()[0])
	if l.Complete() {
		t.Fatal("a chain with no base reports as complete")
	}
}

func TestUnreadableImagesStayInTheTree(t *testing.T) {
	// One corrupt image must not prevent the rest of the tree from being
	// described, and the unreadable file may be exactly what an examiner is
	// looking for -- it could be the link joining two halves of the tree.
	dir := linearTree(t)

	broken := filepath.Join(dir, "broken.avhdx")
	if err := os.WriteFile(broken, bytes.Repeat([]byte{0xEE}, 4096), 0o600); err != nil {
		t.Fatalf("writing the broken image: %v", err)
	}

	tree := discover(t, dir)

	if len(tree.Nodes) != 4 {
		t.Fatalf("found %d nodes, want 4 including the unreadable one", len(tree.Nodes))
	}

	bad := tree.Unreadable()
	if len(bad) != 1 {
		t.Fatalf("found %d unreadable nodes, want 1", len(bad))
	}
	if filepath.Base(bad[0].Path) != "broken.avhdx" {
		t.Fatalf("the unreadable node is %s, want broken.avhdx", bad[0])
	}
	if bad[0].Err == nil {
		t.Fatal("an unreadable node carries no error")
	}
	if bad[0].Role() != RoleUnknown {
		t.Fatalf("an unreadable node has role %v, want %v", bad[0].Role(), RoleUnknown)
	}

	// The readable chain is still fully described.
	if len(tree.Roots()) != 2 {
		t.Fatalf("found %d roots, want 2 (base plus the unlinkable broken file)", len(tree.Roots()))
	}
}

func TestNonImageFilesAreSkippedNotFailed(t *testing.T) {
	// A Hyper-V machine folder holds .vmcx, .vmrs, .bin and .vsv files
	// alongside the disks. Reporting those as failures would bury the real ones.
	dir := linearTree(t)
	for _, name := range []string{"machine.vmcx", "machine.vmrs", "machine.bin", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a disk"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	tree := discover(t, dir)

	if len(tree.Nodes) != 3 {
		t.Fatalf("found %d image nodes, want 3", len(tree.Nodes))
	}
	if len(tree.Unreadable()) != 0 {
		t.Fatalf("non-image files were reported as unreadable images: %v", tree.Unreadable())
	}
	if len(tree.Skipped) != 4 {
		t.Fatalf("skipped %d files, want 4: %v", len(tree.Skipped), tree.Skipped)
	}
}

func TestDiscoverChainDoesNotDescendByDefault(t *testing.T) {
	// Hyper-V keeps a machine's disks in one folder, and a recursive scan from
	// the wrong starting point could walk an entire volume.
	dir := linearTree(t)
	sub := filepath.Join(dir, "nested")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatalf("creating the subdirectory: %v", err)
	}
	writeDisk(t, sub, "other.vhd", [16]byte{0xEE, 0x09}, [16]byte{}, "")

	if got := len(discover(t, dir).Nodes); got != 3 {
		t.Fatalf("found %d nodes without Recursive, want 3", got)
	}

	tree, err := DiscoverChain(context.Background(), dir, &DiscoverOptions{Recursive: true})
	if err != nil {
		t.Fatalf("DiscoverChain: %v", err)
	}
	if len(tree.Nodes) != 4 {
		t.Fatalf("found %d nodes with Recursive, want 4", len(tree.Nodes))
	}
}

func TestLinkingUsesIdentityNotSize(t *testing.T) {
	// Every disk here is the same virtual size, and two of them record no
	// relationship at all. Matching on size would attach them to each other and
	// produce a plausible tree that is wrong.
	dir := t.TempDir()
	writeDisk(t, dir, "base.vhd", idBase, [16]byte{}, "")
	writeDisk(t, dir, "unrelated.vhd", [16]byte{0x7F, 0x7F}, [16]byte{}, "")
	writeDisk(t, dir, "cp1.avhd", idCP1, idBase, "base.vhd")

	tree := discover(t, dir)

	cp1, err := tree.FindPath(filepath.Join(dir, "cp1.avhd"))
	if err != nil {
		t.Fatalf("FindPath: %v", err)
	}
	if cp1.Parent == nil {
		t.Fatal("cp1 has no parent")
	}
	if filepath.Base(cp1.Parent.Path) != "base.vhd" {
		t.Fatalf("cp1's parent is %s, want base.vhd", cp1.Parent)
	}

	unrelated, err := tree.FindPath(filepath.Join(dir, "unrelated.vhd"))
	if err != nil {
		t.Fatalf("FindPath: %v", err)
	}
	if unrelated.Parent != nil || len(unrelated.Children) != 0 {
		t.Fatalf("a same-sized unrelated disk was linked into the tree: parent=%v children=%v",
			unrelated.Parent, unrelated.Children)
	}
}

func TestRoleFollowsDiskTypeNotExtension(t *testing.T) {
	// The .avhdx extension is a Hyper-V convention, not a format. A renamed
	// differencing disk is still a checkpoint, and a base disk given a
	// checkpoint extension is not one.
	dir := t.TempDir()
	writeDisk(t, dir, "base.avhdx", idBase, [16]byte{}, "")
	writeDisk(t, dir, "checkpoint.vhd", idCP1, idBase, "base.avhdx")

	tree := discover(t, dir)

	base, err := tree.FindPath(filepath.Join(dir, "base.avhdx"))
	if err != nil {
		t.Fatalf("FindPath: %v", err)
	}
	if base.Role() != RoleBase {
		t.Errorf("a non-differencing disk named .avhdx has role %v, want %v", base.Role(), RoleBase)
	}
	if !base.Info.HasCheckpointExtension() {
		t.Error("HasCheckpointExtension() is false for a .avhdx file")
	}

	cp, err := tree.FindPath(filepath.Join(dir, "checkpoint.vhd"))
	if err != nil {
		t.Fatalf("FindPath: %v", err)
	}
	if cp.Role() != RoleCheckpoint {
		t.Errorf("a differencing disk named .vhd has role %v, want %v", cp.Role(), RoleCheckpoint)
	}
	if cp.Info.HasCheckpointExtension() {
		t.Error("HasCheckpointExtension() is true for a .vhd file")
	}
}

func TestDiscoverChainIsCancellable(t *testing.T) {
	dir := linearTree(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := DiscoverChain(ctx, dir, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("DiscoverChain returned %v, want context.Canceled", err)
	}
}

func TestDiscoverChainRejectsAFile(t *testing.T) {
	dir := linearTree(t)
	if _, err := DiscoverChain(context.Background(), filepath.Join(dir, "base.vhd"), nil); err == nil {
		t.Fatal("DiscoverChain accepted a file where a directory was required")
	}
}

func TestProbeDoesNotNeedTheBlockAllocationTable(t *testing.T) {
	// The claim that makes scanning a directory of terabyte images cheap. The
	// BAT is truncated away entirely; a full open must fail and the probe must
	// still describe the image.
	img := buildVHD(vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  chkSize,
		blockSize:  chkSize,
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, chkSize)}},
		parentName: "base.vhd",
		parentID:   idBase,
		selfID:     idCP1,
	})

	// Keep only the mirror footer at offset 0 and the dynamic disk header.
	// Everything from the block allocation table onwards is gone, including the
	// trailing footer -- which is exactly the shape a probe has to cope with and
	// a full open must not.
	truncated := append([]byte{}, img[:vhdFooterLen+vhdDynHeaderLen]...)

	if _, err := OpenVHD(bytes.NewReader(truncated), int64(len(truncated))); err == nil {
		t.Fatal("a full open succeeded on an image with no BAT; this test proves nothing")
	}

	info, err := Probe(bytes.NewReader(truncated), int64(len(truncated)))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Identifier != idCP1 {
		t.Errorf("Identifier = %x, want %x", info.Identifier, idCP1)
	}
	if info.ParentIdentifier != idBase {
		t.Errorf("ParentIdentifier = %x, want %x", info.ParentIdentifier, idBase)
	}
	if !info.IsDifferencing() {
		t.Error("IsDifferencing() is false for a differencing image")
	}
	if info.VirtualSize != chkSize {
		t.Errorf("VirtualSize = %d, want %d", info.VirtualSize, chkSize)
	}
}

func TestProbeReportsVHDXLinkIdentitySeparately(t *testing.T) {
	// A VHDX child names its parent by the parent's data-write GUID, not by the
	// parent's virtual disk identifier. Conflating the two is what makes a VHDX
	// chain silently fail to link up.
	dataWrite := [16]byte{0xDA, 0x7A}

	img := buildVHDX(vhdxParams{
		virtualDiskSize: 2 * 1024 * 1024,
		blockSize:       1024 * 1024,
		sectorSize:      512,
		dataWriteGUID:   dataWrite,
	})

	info, err := Probe(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if info.LinkIdentity != dataWrite {
		t.Fatalf("LinkIdentity = %x, want the data-write GUID %x", info.LinkIdentity, dataWrite)
	}
	if info.Identifier == info.LinkIdentity {
		t.Fatal("LinkIdentity and Identifier are the same; the two fields have been conflated")
	}
}

func TestSplitSegmentNamesAreRejectedClearly(t *testing.T) {
	// Neither specification defines a split layout: a multi-file disk in these
	// formats is always a differencing chain. A file named like a VMDK extent
	// is a different format, and saying so beats "invalid signature".
	dir := t.TempDir()
	path := filepath.Join(dir, "disk-s001.vmdk")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x00}, 4096), 0o600); err != nil {
		t.Fatalf("writing the fake segment: %v", err)
	}

	_, err := OpenFile(path)
	if err == nil {
		t.Fatal("a split-disk segment was opened as a VHD")
	}
	if !errors.Is(err, ErrSplitImage) {
		t.Fatalf("error is %v, want it to wrap ErrSplitImage", err)
	}
	if !errors.Is(err, ErrUnsupportedFeature) {
		t.Fatalf("ErrSplitImage does not wrap ErrUnsupportedFeature: %v", err)
	}
}

func TestSplitSegmentDetectionDoesNotCatchRealImages(t *testing.T) {
	// The check must not fire on ordinary names, or it would mask real parse
	// failures behind a misleading explanation.
	for _, name := range []string{
		"disk.vhd", "disk.vhdx", "disk.avhdx",
		"Windows Server 2022.vhdx",
		"base-2024.vhdx",
		"snapshot-s.vhdx",
	} {
		if looksLikeSplitSegment(name) {
			t.Errorf("looksLikeSplitSegment(%q) = true", name)
		}
	}

	for _, name := range []string{"disk-s001.vmdk", "disk-f002.vmdk", "vm.vdi"} {
		if !looksLikeSplitSegment(name) {
			t.Errorf("looksLikeSplitSegment(%q) = false", name)
		}
	}
}
