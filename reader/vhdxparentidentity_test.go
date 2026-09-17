// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// VHDX parent identity verification.
//
// A VHDX differencing child records the identity of its parent in the
// parent_linkage entry of its parent locator, and that value is the parent's
// DataWriteGuid -- not its Virtual Disk Identifier, which is what Identifier
// returns. Conflating the two, or not parsing parent_linkage at all, reduces the
// check to "is the parent the same virtual size", which any same-sized image
// satisfies. These tests pin the behaviour end to end.

var (
	guidRightParent = [16]byte{0xB0, 0x5E, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E}
	guidWrongParent = [16]byte{0xFE, 0xFE, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1F}
)

// linkageValue renders a GUID the way a parent locator records it: registry
// form, with braces.
func linkageValue(guid [16]byte) string {
	return "{" + binaryutil.GUIDToString(guid) + "}"
}

// vhdxIdentityPair builds a parent carrying parentGUID and a differencing child
// naming linkage, both the same virtual size. Sector 0 belongs to the child;
// every other sector must come from the parent.
func vhdxIdentityPair(parentGUID [16]byte, linkage string) (parent, child []byte) {
	parent = buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
		dataWriteGUID:   parentGUID,
		virtualDiskID:   idBase,
	})

	childPayload := make([]byte, vhdxDiffBlockSize)
	copy(childPayload[0:vhdxDiffSectorSize], repeatByte(0xC0, vhdxDiffSectorSize))

	child = buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		hasParent:       true,
		blockState:      types.BlockStatePartiallyAllocated,
		presentSectors:  []int{0},
		payload:         childPayload,
		parentLinkage:   linkage,
		virtualDiskID:   idTop,
	})
	return parent, child
}

// TestVHDXParentLinkageIsParsed is the unit-level guard. Without it the tests
// below could pass for the wrong reason, since a child recording no parent
// identifier is skipped by verifyParent rather than checked.
func TestVHDXParentLinkageIsParsed(t *testing.T) {
	_, child := vhdxIdentityPair(guidRightParent, linkageValue(guidRightParent))

	d, err := OpenVHDX(bytes.NewReader(child), int64(len(child)))
	if err != nil {
		t.Fatalf("OpenVHDX(child): %v", err)
	}
	defer d.Close()

	if got := d.ParentIdentifier(); got != guidRightParent {
		t.Fatalf("ParentIdentifier() = %s, want %s",
			binaryutil.GUIDToString(got), binaryutil.GUIDToString(guidRightParent))
	}
}

// TestVHDXDataWriteIdentifierIsExposed pins the parent side of the comparison.
func TestVHDXDataWriteIdentifierIsExposed(t *testing.T) {
	parent, _ := vhdxIdentityPair(guidRightParent, "")

	d, err := OpenVHDX(bytes.NewReader(parent), int64(len(parent)))
	if err != nil {
		t.Fatalf("OpenVHDX(parent): %v", err)
	}
	defer d.Close()

	if got := d.DataWriteIdentifier(); got != guidRightParent {
		t.Errorf("DataWriteIdentifier() = %s, want %s",
			binaryutil.GUIDToString(got), binaryutil.GUIDToString(guidRightParent))
	}
	if d.Identifier() == d.DataWriteIdentifier() {
		t.Error("Identifier and DataWriteIdentifier coincide; the fixture cannot tell them apart")
	}
}

// TestOpenFileWith_VHDXRejectsWrongParentByGUID is the regression test: a
// same-sized but wrong VHDX parent used to be accepted silently.
func TestOpenFileWith_VHDXRejectsWrongParentByGUID(t *testing.T) {
	wrongParent, child := vhdxIdentityPair(guidWrongParent, linkageValue(guidRightParent))

	dir := writeChainDir(t, map[string][]byte{
		"parent.vhdx": wrongParent,
		"child.vhdx":  child,
	})

	d, err := OpenFileWith(filepath.Join(dir, "child.vhdx"), nil)
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	if !d.NeedsParent() {
		t.Fatal("attached a VHDX parent whose DataWriteGuid does not match the parent_linkage")
	}
	if !errors.Is(d.ParentResolveError(), ErrParentMismatch) {
		t.Errorf("ParentResolveError() = %v, want ErrParentMismatch", d.ParentResolveError())
	}

	buf := make([]byte, vhdxDiffSectorSize)
	if _, err := d.ReadAt(buf, vhdxDiffSectorSize); !errors.Is(err, ErrParentRequired) {
		t.Errorf("ReadAt error = %v, want ErrParentRequired", err)
	}
}

// TestOpenFileWith_VHDXAcceptsCorrectParent is the other half: the check must
// not be so strict that a genuine chain stops resolving.
func TestOpenFileWith_VHDXAcceptsCorrectParent(t *testing.T) {
	parent, child := vhdxIdentityPair(guidRightParent, linkageValue(guidRightParent))

	dir := writeChainDir(t, map[string][]byte{
		"parent.vhdx": parent,
		"child.vhdx":  child,
	})

	d, err := OpenFileWith(filepath.Join(dir, "child.vhdx"), nil)
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("correct parent was not attached: %v", d.ParentResolveError())
	}

	buf := make([]byte, vhdxDiffSectorSize)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt(child sector): %v", err)
	}
	if !bytes.Equal(buf, repeatByte(0xC0, vhdxDiffSectorSize)) {
		t.Error("sector 0 did not come from the child")
	}
	if _, err := d.ReadAt(buf, vhdxDiffSectorSize); err != nil {
		t.Fatalf("ReadAt(parent sector): %v", err)
	}
	if !bytes.Equal(buf, repeatByte(0xA0, vhdxDiffSectorSize)) {
		t.Error("sector 1 did not come from the parent")
	}
}

// TestOpenFileWith_VHDXGUIDMismatchOverride checks the documented escape hatch
// still works now that the check actually runs for VHDX.
func TestOpenFileWith_VHDXGUIDMismatchOverride(t *testing.T) {
	wrongParent, child := vhdxIdentityPair(guidWrongParent, linkageValue(guidRightParent))

	dir := writeChainDir(t, map[string][]byte{
		"parent.vhdx": wrongParent,
		"child.vhdx":  child,
	})

	d, err := OpenFileWith(filepath.Join(dir, "child.vhdx"), &Options{AllowParentGUIDMismatch: true})
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("AllowParentGUIDMismatch did not attach the parent: %v", d.ParentResolveError())
	}
	buf := make([]byte, vhdxDiffSectorSize)
	if _, err := d.ReadAt(buf, vhdxDiffSectorSize); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, repeatByte(0xA0, vhdxDiffSectorSize)) {
		t.Error("override attached the parent but did not read through to it")
	}
}

// TestVHDXRejectsUnknownParentLocatorType pins the rule that the locator type
// GUID names how the entries are to be read. Under an unknown type the key
// "relative_path" is not defined to mean anything, so treating it as a path
// would invent a parent the image never named.
func TestVHDXRejectsUnknownParentLocatorType(t *testing.T) {
	_, child := vhdxIdentityPair(guidRightParent, linkageValue(guidRightParent))

	off := vhdxMetaRegionOff + 0x1100
	if !bytes.Equal(child[off:off+16], types.ParentLocatorTypeVHDX[:]) {
		t.Fatal("parent locator type GUID not at the expected offset; fixture changed")
	}
	copy(child[off:off+16], repeatByte(0x5B, 16))

	if _, err := OpenVHDX(bytes.NewReader(child), int64(len(child))); err == nil {
		t.Fatal("opened a VHDX whose parent locator declares an unknown type")
	}
}
