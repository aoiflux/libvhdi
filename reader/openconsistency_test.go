// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// Open and OpenFile documented near-identical parent resolution behaviour and
// then behaved differently: OpenFile searched the image's directory, Open with
// nil options searched nowhere. Nothing reported the difference. A differencing
// disk opened the second way simply never found its parent, and the symptom --
// ErrParentRequired on read -- looks exactly like a genuinely missing parent.
//
// Nil now means "the default" in both. A caller who wants no resolution says so
// with NoParentResolution.

// writeChain writes a two-disk differencing chain into dir and returns the
// child's path.
func writeChain(t *testing.T, dir string) string {
	t.Helper()

	const (
		mediaSize = 1024 * 1024
		blockSize = 1024 * 1024
	)
	parentID := [16]byte{0xAA, 0x01}

	parent := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: mediaSize,
		blockSize: blockSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: repeatByte(0x11, blockSize)},
		},
		selfID: parentID,
	})

	child := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDifferential,
		mediaSize: mediaSize,
		blockSize: blockSize,
		// No sectors present in the child, so every read must come from the
		// parent. That makes a failed resolution impossible to miss.
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, blockSize)}},
		parentName: "parent.vhd",
		parentID:   parentID,
		selfID:     [16]byte{0xBB, 0x02},
	})

	parentPath := filepath.Join(dir, "parent.vhd")
	childPath := filepath.Join(dir, "child.vhd")
	if err := os.WriteFile(parentPath, parent, 0o600); err != nil {
		t.Fatalf("writing parent: %v", err)
	}
	if err := os.WriteFile(childPath, child, 0o600); err != nil {
		t.Fatalf("writing child: %v", err)
	}
	return childPath
}

// readFirstSector returns the first sector of the assembled device.
func readFirstSector(t *testing.T, d *VirtualDisk) []byte {
	t.Helper()
	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	return buf
}

func TestOpenWithNilOptionsResolvesTheChain(t *testing.T) {
	childPath := writeChain(t, t.TempDir())

	f, err := os.Open(childPath)
	if err != nil {
		t.Fatalf("opening child: %v", err)
	}
	defer f.Close()

	d, err := Open(f, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("Open(f, nil) left the chain unresolved: %v", d.ParentResolveError())
	}
	if got := readFirstSector(t, d); got[0] != 0x11 {
		t.Fatalf("first sector begins %#x, want the parent's %#x", got[0], 0x11)
	}
}

func TestOpenAndOpenFileAgreeOnNilOptions(t *testing.T) {
	// The property that was violated: the two entry points must produce the
	// same device from the same file.
	childPath := writeChain(t, t.TempDir())

	viaOpenFile, err := OpenFile(childPath)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer viaOpenFile.Close()

	f, err := os.Open(childPath)
	if err != nil {
		t.Fatalf("opening child: %v", err)
	}
	defer f.Close()

	viaOpen, err := Open(f, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer viaOpen.Close()

	if viaOpen.NeedsParent() != viaOpenFile.NeedsParent() {
		t.Fatalf("NeedsParent differs: Open=%v OpenFile=%v",
			viaOpen.NeedsParent(), viaOpenFile.NeedsParent())
	}
	if viaOpen.ChainDepth() != viaOpenFile.ChainDepth() {
		t.Fatalf("ChainDepth differs: Open=%d OpenFile=%d",
			viaOpen.ChainDepth(), viaOpenFile.ChainDepth())
	}
	if !bytes.Equal(readFirstSector(t, viaOpen), readFirstSector(t, viaOpenFile)) {
		t.Fatal("Open and OpenFile produced different device contents")
	}
}

func TestNoParentResolutionSuppressesTheSearch(t *testing.T) {
	// A caller who wants to attach parents by hand must be able to say so, and
	// the result must fail closed rather than substituting zeroes.
	childPath := writeChain(t, t.TempDir())

	d, err := OpenFileWith(childPath, &Options{ParentResolver: NoParentResolution})
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	if !d.NeedsParent() {
		t.Fatal("NoParentResolution still resolved the chain")
	}
	if err := d.ParentResolveError(); err != nil {
		t.Fatalf("NoParentResolution recorded a resolution failure: %v", err)
	}

	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err == nil {
		t.Fatal("a read needing the absent parent returned data instead of failing closed")
	}
}

func TestOpenFromABareReaderCannotSearchButStillFailsClosed(t *testing.T) {
	// A bytes.Reader names no file, so there is no directory to search. The
	// disk must still open, and reads needing the parent must still fail rather
	// than return zeroes that cannot be told apart from disk contents.
	dir := t.TempDir()
	childPath := writeChain(t, dir)

	img, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatalf("reading child: %v", err)
	}

	d, err := Open(bytes.NewReader(img), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if d.Path() != "" {
		t.Fatalf("Path() = %q for a reader that names no file", d.Path())
	}
	if !d.NeedsParent() {
		t.Fatal("a bare reader resolved a parent it had no way to locate")
	}

	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err == nil {
		t.Fatal("a read needing the absent parent returned data instead of failing closed")
	}
}

func TestOpenRecordsThePathFromAnOsFile(t *testing.T) {
	// The path is what gives the chain search somewhere to look and gives cycle
	// detection its most faithful key.
	childPath := writeChain(t, t.TempDir())

	f, err := os.Open(childPath)
	if err != nil {
		t.Fatalf("opening child: %v", err)
	}
	defer f.Close()

	d, err := Open(f, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if d.Path() != childPath {
		t.Fatalf("Path() = %q, want %q", d.Path(), childPath)
	}
}

func TestRequireParentChainStillFailsLoudly(t *testing.T) {
	// Best-effort resolution is the default, but a caller who cannot proceed
	// without the whole chain must be able to demand it.
	dir := t.TempDir()
	childPath := writeChain(t, dir)
	if err := os.Remove(filepath.Join(dir, "parent.vhd")); err != nil {
		t.Fatalf("removing parent: %v", err)
	}

	if _, err := OpenFileWith(childPath, &Options{RequireParentChain: true}); err == nil {
		t.Fatal("RequireParentChain opened a chain whose parent is missing")
	}
}
