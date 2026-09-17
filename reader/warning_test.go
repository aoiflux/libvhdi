// SPDX-License-Identifier: MIT

package reader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoiflux/libvhdi/types"
)

// Refusing to open a damaged image and opening it silently are both wrong for
// forensic use: the first loses recoverable evidence, the second presents a
// recovered or unverified reconstruction as an intact one. Warnings are how the
// image gets read and the caveat gets into the record.

func hasWarning(ws []Warning, kind WarningKind) bool {
	for _, w := range ws {
		if w.Kind == kind {
			return true
		}
	}
	return false
}

func findWarning(t *testing.T, ws []Warning, kind WarningKind) Warning {
	t.Helper()
	for _, w := range ws {
		if w.Kind == kind {
			return w
		}
	}
	t.Fatalf("no %s warning among %v", kind, ws)
	return Warning{}
}

func TestAnIntactImageWarnsAboutNothing(t *testing.T) {
	// The baseline that makes every other test in this file mean something. If
	// a clean image produced warnings, the mechanism would be noise.
	img := buildDynamicVHD(types.DiskTypeDynamic, 1024*1024, 1024*1024, []vhdBlock{
		{allocated: true, presentSectors: []int{0}, data: repeatByte(0x33, 1024*1024)},
	}, "", [16]byte{})

	path := filepath.Join(t.TempDir(), "clean.vhd")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatalf("writing image: %v", err)
	}

	d, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if d.HasWarnings() {
		t.Fatalf("an intact image produced warnings: %v", d.Warnings())
	}
}

func TestRecoveringFromTheMirrorFooterRaisesAWarning(t *testing.T) {
	// An image opened from a fallback footer copy is a damaged image. Opening
	// it is right; presenting it as an intact read is not.
	img := buildDynamicVHD(types.DiskTypeDynamic, 1024*1024, 1024*1024, []vhdBlock{
		{allocated: true, presentSectors: []int{0}, data: repeatByte(0x44, 1024*1024)},
	}, "", [16]byte{})
	destroyTrailingFooter(img)

	path := filepath.Join(t.TempDir(), "damaged.vhd")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatalf("writing image: %v", err)
	}

	d, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	w := findWarning(t, d.Warnings(), WarningFooterRecovered)
	if w.Path != path {
		t.Errorf("warning Path = %q, want %q", w.Path, path)
	}
	if !strings.Contains(w.Detail, FooterSourceMirror.String()) {
		t.Errorf("warning does not name the footer copy used: %q", w.Detail)
	}
}

// writeTimestampedChain writes a differencing chain whose child records
// recordedParentTime as its parent's modification time, and stamps the parent
// file on disk with actualParentTime.
func writeTimestampedChain(t *testing.T, dir string, recorded, actual time.Time) string {
	t.Helper()

	const size = 1024 * 1024
	parentID := [16]byte{0xCC, 0x01}

	parent := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: size,
		blockSize: size,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: repeatByte(0x55, size)},
		},
		selfID: parentID,
	})
	child := buildVHD(vhdImageParams{
		diskType:      types.DiskTypeDifferential,
		mediaSize:     size,
		blockSize:     size,
		blocks:        []vhdBlock{{allocated: true, data: make([]byte, size)}},
		parentName:    "parent.vhd",
		parentID:      parentID,
		parentModTime: recorded,
		selfID:        [16]byte{0xDD, 0x02},
	})

	parentPath := filepath.Join(dir, "parent.vhd")
	childPath := filepath.Join(dir, "child.vhd")
	if err := os.WriteFile(parentPath, parent, 0o600); err != nil {
		t.Fatalf("writing parent: %v", err)
	}
	if err := os.WriteFile(childPath, child, 0o600); err != nil {
		t.Fatalf("writing child: %v", err)
	}
	if !actual.IsZero() {
		if err := os.Chtimes(parentPath, actual, actual); err != nil {
			t.Fatalf("stamping parent: %v", err)
		}
	}
	return childPath
}

func TestParentTimestampMismatchRaisesAWarning(t *testing.T) {
	// A parent written to since the child was created makes the reconstructed
	// device wrong in a way nothing else detects: the identifier still matches,
	// the size still matches, and the blocks the child does not override now
	// hold different data than they did.
	recorded := time.Date(2024, time.March, 1, 12, 0, 0, 0, time.UTC)
	actual := time.Date(2025, time.July, 9, 8, 30, 0, 0, time.UTC)

	childPath := writeTimestampedChain(t, t.TempDir(), recorded, actual)

	d, err := OpenFile(childPath)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("chain did not resolve: %v", d.ParentResolveError())
	}

	w := findWarning(t, d.Warnings(), WarningParentTimestampMismatch)
	if !strings.Contains(w.Detail, "2024") || !strings.Contains(w.Detail, "2025") {
		t.Errorf("warning does not show both timestamps: %q", w.Detail)
	}
}

func TestMatchingParentTimestampRaisesNoWarning(t *testing.T) {
	// The check must not fire on the ordinary case, or it would be ignored.
	when := time.Date(2024, time.March, 1, 12, 0, 0, 0, time.UTC)
	childPath := writeTimestampedChain(t, t.TempDir(), when, when)

	d, err := OpenFile(childPath)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if hasWarning(d.Warnings(), WarningParentTimestampMismatch) {
		t.Fatalf("warned about a parent whose timestamp matches: %v", d.Warnings())
	}
}

func TestParentTimestampMismatchIsNotFatal(t *testing.T) {
	// Filesystem modification times do not survive a copy, so a chain moved
	// between machines mismatches routinely and is still the right chain.
	// Failing the open here would refuse far more good images than bad ones.
	recorded := time.Date(2024, time.March, 1, 12, 0, 0, 0, time.UTC)
	actual := time.Date(2025, time.July, 9, 8, 30, 0, 0, time.UTC)
	childPath := writeTimestampedChain(t, t.TempDir(), recorded, actual)

	d, err := OpenFile(childPath)
	if err != nil {
		t.Fatalf("a timestamp mismatch failed the open: %v", err)
	}
	defer d.Close()

	// And the device still reads through to the parent.
	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if buf[0] != 0x55 {
		t.Fatalf("first sector begins %#x, want the parent's %#x", buf[0], 0x55)
	}
}

func TestUnrecordedParentTimestampRaisesNoWarning(t *testing.T) {
	// A zero field means the producer recorded no timestamp. There is nothing
	// to compare, and warning would be inventing a discrepancy.
	childPath := writeTimestampedChain(t, t.TempDir(), time.Time{},
		time.Date(2025, time.July, 9, 8, 30, 0, 0, time.UTC))

	d, err := OpenFile(childPath)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if hasWarning(d.Warnings(), WarningParentTimestampMismatch) {
		t.Fatalf("warned about a parent timestamp the child never recorded: %v", d.Warnings())
	}
}

func TestChildWithNoParentIdentifierWarns(t *testing.T) {
	// A child recording no parent identifier can only have its parent checked
	// by virtual size, which any image of the same size satisfies. That is the
	// condition that let a wrong parent be attached before v0.3.0, and it is
	// worth saying out loud even when it is the image's fault, not the
	// library's.
	const size = 1024 * 1024
	dir := t.TempDir()

	parent := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: size,
		blockSize: size,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: repeatByte(0x66, size)},
		},
		selfID: [16]byte{0xEE, 0x01},
	})
	child := buildVHD(vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  size,
		blockSize:  size,
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, size)}},
		parentName: "parent.vhd",
		parentID:   [16]byte{}, // no identity recorded
		selfID:     [16]byte{0xFF, 0x02},
	})

	if err := os.WriteFile(filepath.Join(dir, "parent.vhd"), parent, 0o600); err != nil {
		t.Fatalf("writing parent: %v", err)
	}
	childPath := filepath.Join(dir, "child.vhd")
	if err := os.WriteFile(childPath, child, 0o600); err != nil {
		t.Fatalf("writing child: %v", err)
	}

	d, err := OpenFile(childPath)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if !hasWarning(d.Warnings(), WarningParentIdentityUnverifiable) {
		t.Fatalf("no unverifiable-identity warning: %v", d.Warnings())
	}
}

func TestAllowParentGUIDMismatchWarns(t *testing.T) {
	// Disabling the check is a legitimate choice for a damaged chain, but the
	// resulting device is unverified and the record has to say so.
	recorded := time.Date(2024, time.March, 1, 12, 0, 0, 0, time.UTC)
	childPath := writeTimestampedChain(t, t.TempDir(), recorded, recorded)

	d, err := OpenFileWith(childPath, &Options{AllowParentGUIDMismatch: true})
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	if !hasWarning(d.Warnings(), WarningParentIdentityUnchecked) {
		t.Fatalf("no unchecked-identity warning: %v", d.Warnings())
	}
}

func TestWarningsCoverTheWholeChain(t *testing.T) {
	// A differencing disk is only as trustworthy as the chain behind it, so a
	// caller recording provenance needs the whole chain's caveats, not the
	// leaf's alone. Here the damage is in the parent, not the child.
	const size = 1024 * 1024
	dir := t.TempDir()
	parentID := [16]byte{0x1A, 0x01}

	parent := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: size,
		blockSize: size,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: repeatByte(0x77, size)},
		},
		selfID: parentID,
	})
	destroyTrailingFooter(parent)

	child := buildVHD(vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  size,
		blockSize:  size,
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, size)}},
		parentName: "parent.vhd",
		parentID:   parentID,
		selfID:     [16]byte{0x2B, 0x02},
	})

	parentPath := filepath.Join(dir, "parent.vhd")
	childPath := filepath.Join(dir, "child.vhd")
	if err := os.WriteFile(parentPath, parent, 0o600); err != nil {
		t.Fatalf("writing parent: %v", err)
	}
	if err := os.WriteFile(childPath, child, 0o600); err != nil {
		t.Fatalf("writing child: %v", err)
	}

	d, err := OpenFile(childPath)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("chain did not resolve: %v", d.ParentResolveError())
	}

	w := findWarning(t, d.Warnings(), WarningFooterRecovered)
	if w.Path != parentPath {
		t.Fatalf("warning is attributed to %q, want the parent at %q", w.Path, parentPath)
	}
}

func TestWarningStringNamesKindAndPath(t *testing.T) {
	w := Warning{Kind: WarningFooterRecovered, Path: `C:\vm\d.vhd`, Detail: "detail here"}
	got := w.String()
	for _, want := range []string{string(WarningFooterRecovered), `C:\vm\d.vhd`, "detail here"} {
		if !strings.Contains(got, want) {
			t.Errorf("Warning.String() = %q, missing %q", got, want)
		}
	}

	bare := Warning{Kind: WarningFooterRecovered, Detail: "detail here"}
	if strings.Contains(bare.String(), "[]") {
		t.Errorf("Warning.String() renders an empty path bracket: %q", bare.String())
	}
}
