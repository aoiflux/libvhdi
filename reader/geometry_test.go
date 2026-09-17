// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoiflux/libvhdi/types"
)

// The parsers always decoded CHS geometry, the creator application and version,
// the saved-state flag, the feature word and the size a disk had at creation --
// and then dropped every one of them on the floor. None was reachable through
// the public API, so none could appear in a report. These tests pin that they
// now survive the trip from footer to accessor.

func TestGeometryReportsCHSFromTheFooter(t *testing.T) {
	// 65 heads and 17 sectors per track is what Hyper-V writes for a disk in
	// this size range, so these are the shapes a real image carries.
	img := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: 2 * 1024 * 1024,
		blockSize: 1024 * 1024,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: repeatByte(0x10, 1024*1024)},
			{allocated: false},
		},
		cylinders:       1024,
		heads:           65,
		sectorsPerTrack: 17,
	})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	g := d.Geometry()
	if !g.HasCHS() {
		t.Fatal("HasCHS() is false for a footer carrying a geometry")
	}
	if g.Cylinders != 1024 || g.Heads != 65 || g.SectorsPerTrack != 17 {
		t.Fatalf("CHS = %d/%d/%d, want 1024/65/17", g.Cylinders, g.Heads, g.SectorsPerTrack)
	}

	capacity, ok := g.CHSCapacity()
	if !ok {
		t.Fatal("CHSCapacity() reports no geometry")
	}
	if want := uint64(1024) * 65 * 17 * 512; capacity != want {
		t.Fatalf("CHSCapacity() = %d, want %d", capacity, want)
	}
}

func TestGeometryReportsNoCHSForVHDX(t *testing.T) {
	// VHDX has no CHS field at all. Reporting a zero geometry as though the
	// image recorded one would state a fact the format cannot express.
	img := buildVHDX(vhdxParams{
		virtualDiskSize: 2 * 1024 * 1024,
		blockSize:       1024 * 1024,
		sectorSize:      512,
	})

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	if d.Geometry().HasCHS() {
		t.Fatal("HasCHS() is true for a VHDX, which has no CHS geometry")
	}
	if _, ok := d.Geometry().CHSCapacity(); ok {
		t.Fatal("CHSCapacity() claims a geometry for a VHDX")
	}
}

func TestGeometryReportsBothSectorSizes(t *testing.T) {
	// A 512e disk presents 512-byte sectors to the guest while sitting on
	// 4096-byte media. Collapsing the two would lose the alignment the guest's
	// filesystem was actually built for.
	img := buildVHDX(vhdxParams{
		virtualDiskSize:    4 * 1024 * 1024,
		blockSize:          1024 * 1024,
		sectorSize:         512,
		physicalSectorSize: 4096,
	})

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	g := d.Geometry()
	if g.LogicalSectorSize != 512 {
		t.Errorf("LogicalSectorSize = %d, want 512", g.LogicalSectorSize)
	}
	if g.PhysicalSectorSize != 4096 {
		t.Errorf("PhysicalSectorSize = %d, want 4096", g.PhysicalSectorSize)
	}
	if g.VirtualSize != 4*1024*1024 {
		t.Errorf("VirtualSize = %d, want %d", g.VirtualSize, 4*1024*1024)
	}
	if g.BlockSize != 1024*1024 {
		t.Errorf("BlockSize = %d, want %d", g.BlockSize, 1024*1024)
	}
}

func TestProvenanceReportsTheVHDCreator(t *testing.T) {
	created := time.Date(2019, time.November, 5, 9, 15, 0, 0, time.UTC)

	img := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: 1024 * 1024,
		blockSize: 1024 * 1024,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: repeatByte(0x20, 1024*1024)},
		},
		createdAt: created,
	})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	p := d.Provenance()
	if p.CreatorApplication != "libv" {
		t.Errorf("CreatorApplication = %q, want %q", p.CreatorApplication, "libv")
	}
	if p.CreatorOS != "Wi2k" {
		t.Errorf("CreatorOS = %q, want %q", p.CreatorOS, "Wi2k")
	}
	if got := p.CreatorVersionString(); got != "1.0" {
		t.Errorf("CreatorVersionString() = %q, want %q", got, "1.0")
	}
	if !p.Created.Equal(created) {
		t.Errorf("Created = %s, want %s", p.Created, created)
	}
	if p.Format != types.FileFormatVHD {
		t.Errorf("Format = %v, want VHD", p.Format)
	}
	if p.FooterSource != FooterSourceTrailing {
		t.Errorf("FooterSource = %v, want %v", p.FooterSource, FooterSourceTrailing)
	}
	if p.FileSize != int64(len(img)) {
		t.Errorf("FileSize = %d, want %d", p.FileSize, len(img))
	}
}

func TestProvenanceDecodesTheVHDXCreatorAsUTF16(t *testing.T) {
	// The Creator field is 512 bytes of UTF-16 little-endian. Read as a
	// NUL-terminated byte string it stops at the high byte of the first
	// character, so this would come back as "M".
	const creator = "Microsoft Windows 10.0"

	img := buildVHDX(vhdxParams{
		virtualDiskSize: 2 * 1024 * 1024,
		blockSize:       1024 * 1024,
		sectorSize:      512,
		creator:         creator,
	})

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	if got := d.Provenance().CreatorApplication; got != creator {
		t.Fatalf("CreatorApplication = %q, want %q", got, creator)
	}
}

func TestProvenanceReportsAnExpandedDisk(t *testing.T) {
	// The footer records the size at creation alongside the current size. Their
	// difference is the only record that an expansion happened.
	const (
		original = 1024 * 1024
		current  = 4 * 1024 * 1024
	)

	img := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: current,
		blockSize: 1024 * 1024,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: repeatByte(0x30, 1024*1024)},
			{allocated: false}, {allocated: false}, {allocated: false},
		},
		dataSize: original,
	})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	p := d.Provenance()
	if p.OriginalSize != original {
		t.Fatalf("OriginalSize = %d, want %d", p.OriginalSize, original)
	}
	if d.Geometry().VirtualSize != current {
		t.Fatalf("VirtualSize = %d, want %d", d.Geometry().VirtualSize, current)
	}
	if p.OriginalSize == d.Geometry().VirtualSize {
		t.Fatal("an expanded disk reports the same original and current size")
	}
}

func TestProvenanceReportsSavedState(t *testing.T) {
	// A saved-state image holds a crash-consistent filesystem rather than a
	// cleanly unmounted one, which changes what its contents can be taken to
	// mean. The flag was parsed and then discarded.
	img := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: 1024 * 1024,
		blockSize: 1024 * 1024,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: repeatByte(0x40, 1024*1024)},
		},
		savedState: true,
	})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	if !d.Provenance().SavedState {
		t.Fatal("SavedState is false for an image saved from a running machine")
	}
}

func TestProvenanceIsPerImageAcrossAChain(t *testing.T) {
	// A child and its parents are often produced by different tools at
	// different times. Flattening that into one record loses the detail that
	// makes a chain explicable, so Provenance describes one disk and the chain
	// is walked by the caller.
	const size = 1024 * 1024
	dir := t.TempDir()
	parentID := [16]byte{0x3C, 0x01}

	parentCreated := time.Date(2018, time.February, 2, 0, 0, 0, 0, time.UTC)
	childCreated := time.Date(2021, time.August, 8, 0, 0, 0, 0, time.UTC)

	parent := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: size,
		blockSize: size,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: repeatByte(0x50, size)},
		},
		selfID:    parentID,
		createdAt: parentCreated,
	})
	child := buildVHD(vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  size,
		blockSize:  size,
		blocks:     []vhdBlock{{allocated: true, data: make([]byte, size)}},
		parentName: "parent.vhd",
		parentID:   parentID,
		selfID:     [16]byte{0x4D, 0x02},
		createdAt:  childCreated,
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

	if d.NeedsParent() {
		t.Fatalf("chain did not resolve: %v", d.ParentResolveError())
	}

	if got := d.Provenance().Created; !got.Equal(childCreated) {
		t.Errorf("child Created = %s, want %s", got, childCreated)
	}
	if got := d.Parent().Provenance().Created; !got.Equal(parentCreated) {
		t.Errorf("parent Created = %s, want %s", got, parentCreated)
	}
	if d.Provenance().Path != childPath {
		t.Errorf("child Path = %q, want %q", d.Provenance().Path, childPath)
	}
}

func TestProvenanceCarriesTheImagesOwnWarnings(t *testing.T) {
	// Warnings on the disk cover the whole chain; Provenance is per-image, so
	// each disk's record carries only its own caveats.
	img := buildDynamicVHD(types.DiskTypeDynamic, 1024*1024, 1024*1024, []vhdBlock{
		{allocated: true, presentSectors: []int{0}, data: repeatByte(0x60, 1024*1024)},
	}, "", [16]byte{})
	destroyTrailingFooter(img)

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	p := d.Provenance()
	if !hasWarning(p.Warnings, WarningFooterRecovered) {
		t.Fatalf("Provenance carries no footer-recovered warning: %v", p.Warnings)
	}
	if p.FooterSource != FooterSourceMirror {
		t.Fatalf("FooterSource = %v, want %v", p.FooterSource, FooterSourceMirror)
	}
}

func TestCreatorCodePaddingIsStripped(t *testing.T) {
	// The four-byte codes are space-padded ("win ", "vpc "), and some producers
	// pad with NUL, which would otherwise end up embedded in a report.
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"win ", "win"},
		{"vpc ", "vpc"},
		{"qemu", "qemu"},
		{"d2v\x00", "d2v"},
		{"\x00\x00\x00\x00", ""},
		{"    ", ""},
	} {
		if got := trimCreatorCode(tc.in); got != tc.want {
			t.Errorf("trimCreatorCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
