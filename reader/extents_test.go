// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// checkExtentsTile verifies the structural invariants every extent list must
// satisfy: exact tiling of the requested range, ascending order, positive
// lengths, and no two adjacent extents that should have been merged.
func checkExtentsTile(t *testing.T, extents []Extent, off, length int64) {
	t.Helper()

	if len(extents) == 0 {
		t.Fatalf("no extents returned for [%d, %d)", off, off+length)
	}

	at := off
	for i, e := range extents {
		if e.VirtualOffset != at {
			t.Fatalf("extent %d starts at %d, want %d (gap or overlap)", i, e.VirtualOffset, at)
		}
		if e.Length <= 0 {
			t.Fatalf("extent %d has length %d", i, e.Length)
		}
		if e.Kind == ExtentMapped && e.FileOffset < 0 {
			t.Fatalf("extent %d is mapped but has file offset %d", i, e.FileOffset)
		}
		if e.Kind == ExtentZero && (e.FileOffset != 0 || e.Path != "") {
			t.Fatalf("extent %d is a zero run but carries provenance: %+v", i, e)
		}
		if i > 0 && canMerge(extents[i-1], e) {
			t.Fatalf("extents %d and %d should have been merged: %+v then %+v", i-1, i, extents[i-1], e)
		}
		at = e.End()
	}

	if at != off+length {
		t.Fatalf("extents cover up to %d, want %d", at, off+length)
	}
}

// checkExtentsMatchReads is the property that makes the extent map trustworthy:
// what the map claims must agree with what ReadAt returns. A zero extent must
// read as zeroes, and a mapped extent's bytes must be present at its declared
// file offset in its declared file.
func checkExtentsMatchReads(t *testing.T, d *VirtualDisk, extents []Extent, files map[string][]byte) {
	t.Helper()

	for i, e := range extents {
		got := make([]byte, e.Length)
		if _, err := d.ReadAt(got, e.VirtualOffset); err != nil {
			if e.Kind == ExtentUnresolved && errors.Is(err, ErrParentRequired) {
				continue // expected: the backing disk is missing
			}
			t.Fatalf("extent %d ReadAt(%d, %d): %v", i, e.VirtualOffset, e.Length, err)
		}

		switch e.Kind {
		case ExtentZero:
			if !bytes.Equal(got, make([]byte, e.Length)) {
				t.Errorf("extent %d is a zero run but reads %#x...", i, got[:min(8, len(got))])
			}

		case ExtentMapped:
			raw, ok := files[e.Path]
			if !ok {
				continue // caller did not supply this file's bytes
			}
			if e.FileOffset+e.Length > int64(len(raw)) {
				t.Errorf("extent %d maps to [%d, %d) beyond the %d byte file %s",
					i, e.FileOffset, e.FileOffset+e.Length, len(raw), e.Path)
				continue
			}
			want := raw[e.FileOffset : e.FileOffset+e.Length]
			if !bytes.Equal(got, want) {
				t.Errorf("extent %d: disk bytes differ from file %s at offset %d",
					i, e.Path, e.FileOffset)
			}

		case ExtentUnresolved:
			t.Errorf("extent %d is unresolved but ReadAt succeeded", i)
		}
	}
}

// ============================================================================
// Non-differencing disks
// ============================================================================

func TestExtentsFixedVHDIsOneMappedRun(t *testing.T) {
	const size = 8192
	img := buildFixedVHD(size, func(p []byte) {
		for i := range p {
			p[i] = byte(i % 251)
		}
	})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	extents, err := d.AllExtents()
	if err != nil {
		t.Fatalf("AllExtents: %v", err)
	}
	checkExtentsTile(t, extents, 0, size)

	if len(extents) != 1 {
		t.Fatalf("got %d extents, want 1 for a fixed disk: %+v", len(extents), extents)
	}
	e := extents[0]
	if e.Kind != ExtentMapped || e.FileOffset != 0 || e.Length != size {
		t.Errorf("extent = %+v, want one mapped run of %d bytes at file offset 0", e, size)
	}

	mapped, err := d.MappedBytes()
	if err != nil {
		t.Fatalf("MappedBytes: %v", err)
	}
	if mapped != size {
		t.Errorf("MappedBytes() = %d, want %d", mapped, size)
	}
}

// TestExtentsDynamicVHDMarksHoles is the case an acquisition tool cares about:
// unallocated blocks must be reported as zero runs so they can be skipped.
func TestExtentsDynamicVHDMarksHoles(t *testing.T) {
	const (
		blockSize = 2 * 1024 * 1024
		blocks    = 4
		mediaSize = uint64(blockSize * blocks)
	)

	// Blocks 0 and 2 allocated; 1 and 3 are holes.
	img := buildDynamicVHD(types.DiskTypeDynamic, mediaSize, blockSize, []vhdBlock{
		{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x11, blockSize)},
		{allocated: false},
		{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x33, blockSize)},
		{allocated: false},
	}, "", [16]byte{})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	extents, err := d.AllExtents()
	if err != nil {
		t.Fatalf("AllExtents: %v", err)
	}
	checkExtentsTile(t, extents, 0, int64(mediaSize))

	if len(extents) != 4 {
		t.Fatalf("got %d extents, want 4 (mapped, zero, mapped, zero): %+v", len(extents), extents)
	}
	wantKinds := []ExtentKind{ExtentMapped, ExtentZero, ExtentMapped, ExtentZero}
	for i, want := range wantKinds {
		if extents[i].Kind != want {
			t.Errorf("extent %d kind = %v, want %v", i, extents[i].Kind, want)
		}
		if extents[i].Length != blockSize {
			t.Errorf("extent %d length = %d, want %d", i, extents[i].Length, blockSize)
		}
	}

	mapped, err := d.MappedBytes()
	if err != nil {
		t.Fatalf("MappedBytes: %v", err)
	}
	if want := int64(2 * blockSize); mapped != want {
		t.Errorf("MappedBytes() = %d, want %d (half the disk is sparse)", mapped, want)
	}

	checkExtentsMatchReads(t, d, extents, map[string][]byte{"": img})
}

func TestExtentsMergesAdjacentAllocatedBlocks(t *testing.T) {
	const (
		blockSize = 2 * 1024 * 1024
		mediaSize = uint64(blockSize * 2)
	)

	// buildDynamicVHD lays allocated blocks out consecutively, so two adjacent
	// allocated blocks are contiguous in the file and must merge into one extent.
	img := buildDynamicVHD(types.DiskTypeDynamic, mediaSize, blockSize, []vhdBlock{
		{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x11, blockSize)},
		{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x22, blockSize)},
	}, "", [16]byte{})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	extents, err := d.AllExtents()
	if err != nil {
		t.Fatalf("AllExtents: %v", err)
	}
	checkExtentsTile(t, extents, 0, int64(mediaSize))

	// A VHD interleaves a sector bitmap before each block, so consecutive blocks
	// are not byte-contiguous and must stay separate extents.
	for i, e := range extents {
		if e.Kind != ExtentMapped {
			t.Errorf("extent %d kind = %v, want mapped", i, e.Kind)
		}
	}
	checkExtentsMatchReads(t, d, extents, map[string][]byte{"": img})
}

// TestExtentsSubRange checks that a request narrower than the disk is honoured
// exactly, including starting and ending mid-block.
func TestExtentsSubRange(t *testing.T) {
	const (
		blockSize = 2 * 1024 * 1024
		mediaSize = uint64(blockSize * 3)
	)

	img := buildDynamicVHD(types.DiskTypeDynamic, mediaSize, blockSize, []vhdBlock{
		{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x11, blockSize)},
		{allocated: false},
		{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x33, blockSize)},
	}, "", [16]byte{})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	// Start halfway through block 0 and end halfway through block 2.
	off := int64(blockSize / 2)
	length := int64(blockSize * 2)

	extents, err := d.Extents(off, length)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}
	checkExtentsTile(t, extents, off, length)
	checkExtentsMatchReads(t, d, extents, map[string][]byte{"": img})
}

func TestExtentsClampsToDiskSize(t *testing.T) {
	const size = 8192
	img := buildFixedVHD(size, nil)

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	extents, err := d.Extents(size-512, 1<<20)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}
	checkExtentsTile(t, extents, size-512, 512)

	if _, err := d.Extents(size, 512); !errors.Is(err, io.EOF) {
		t.Errorf("Extents past the end returned %v, want io.EOF", err)
	}
}

// ============================================================================
// Differencing chains
// ============================================================================

// TestExtentsDifferencingReportsPerSectorProvenance is the case
// VirtualToFileOffset cannot express: within a single partially-written block,
// alternating sectors come from the child and the parent.
func TestExtentsDifferencingReportsPerSectorProvenance(t *testing.T) {
	parentImg, childImg, _ := diffVHDPair("parent.vhd")
	dir := writeChainDir(t, map[string][]byte{
		"parent.vhd": parentImg,
		"child.vhd":  childImg,
	})

	d, err := OpenFile(filepath.Join(dir, "child.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("chain incomplete: %v", d.ParentResolveError())
	}

	// The child holds sectors 0, 2 and 4; every other sector is the parent's.
	const probe = 6 * 512
	extents, err := d.Extents(0, probe)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}
	checkExtentsTile(t, extents, 0, probe)

	if len(extents) != 6 {
		t.Fatalf("got %d extents, want 6 alternating sectors: %+v", len(extents), extents)
	}
	for i, e := range extents {
		wantChain := 1 // parent
		if i%2 == 0 {
			wantChain = 0 // child
		}
		if e.Kind != ExtentMapped {
			t.Errorf("extent %d kind = %v, want mapped", i, e.Kind)
		}
		if e.ChainIndex != wantChain {
			t.Errorf("extent %d ChainIndex = %d, want %d", i, e.ChainIndex, wantChain)
		}
		if e.Length != 512 {
			t.Errorf("extent %d length = %d, want 512", i, e.Length)
		}
	}

	checkExtentsMatchReads(t, d, extents, map[string][]byte{
		filepath.Join(dir, "child.vhd"):  childImg,
		filepath.Join(dir, "parent.vhd"): parentImg,
	})
}

// TestExtentsThreeDeepChainProvenance checks the chain index counts through more
// than one level.
func TestExtentsThreeDeepChainProvenance(t *testing.T) {
	const (
		blockSize = chainBlockSize
		mediaSize = chainMediaSize
	)
	sectors := blockSize / 512

	base := buildVHD(vhdImageParams{
		diskType: types.DiskTypeDynamic, mediaSize: mediaSize, blockSize: blockSize,
		blocks: []vhdBlock{{allocated: true, presentSectors: allSectors(sectors), data: repeatByte(0xA0, blockSize)}},
		selfID: idBase,
	})

	midData := make([]byte, blockSize)
	copy(midData[0:512], repeatByte(0xB0, 512))
	copy(midData[512:1024], repeatByte(0xB0, 512))
	mid := buildVHD(vhdImageParams{
		diskType: types.DiskTypeDifferential, mediaSize: mediaSize, blockSize: blockSize,
		blocks:     []vhdBlock{{allocated: true, presentSectors: []int{0, 1}, data: midData}},
		parentName: "base.vhd", parentID: idBase, selfID: idMid,
	})

	topData := make([]byte, blockSize)
	copy(topData[0:512], repeatByte(0xC0, 512))
	top := buildVHD(vhdImageParams{
		diskType: types.DiskTypeDifferential, mediaSize: mediaSize, blockSize: blockSize,
		blocks:     []vhdBlock{{allocated: true, presentSectors: []int{0}, data: topData}},
		parentName: "mid.vhd", parentID: idMid, selfID: idTop,
	})

	dir := writeChainDir(t, map[string][]byte{
		"base.vhd": base, "mid.vhd": mid, "top.vhd": top,
	})

	d, err := OpenFile(filepath.Join(dir, "top.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()
	if d.NeedsParent() {
		t.Fatalf("chain incomplete: %v", d.ParentResolveError())
	}

	extents, err := d.Extents(0, 3*512)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}
	checkExtentsTile(t, extents, 0, 3*512)

	if len(extents) != 3 {
		t.Fatalf("got %d extents, want 3 (top, mid, base): %+v", len(extents), extents)
	}
	for i, wantChain := range []int{0, 1, 2} {
		if extents[i].ChainIndex != wantChain {
			t.Errorf("extent %d ChainIndex = %d, want %d", i, extents[i].ChainIndex, wantChain)
		}
	}

	checkExtentsMatchReads(t, d, extents, map[string][]byte{
		filepath.Join(dir, "top.vhd"):  top,
		filepath.Join(dir, "mid.vhd"):  mid,
		filepath.Join(dir, "base.vhd"): base,
	})
}

// TestExtentsUnresolvedWhenParentMissing checks that a missing parent surfaces as
// ExtentUnresolved, so a caller can see exactly which ranges it cannot account
// for instead of getting an error for the whole request.
func TestExtentsUnresolvedWhenParentMissing(t *testing.T) {
	_, childImg, _ := diffVHDPair("parent.vhd")
	dir := writeChainDir(t, map[string][]byte{"child.vhd": childImg})

	d, err := OpenFile(filepath.Join(dir, "child.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()
	if !d.NeedsParent() {
		t.Fatal("expected an incomplete chain")
	}

	const probe = 6 * 512
	extents, err := d.Extents(0, probe)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}
	checkExtentsTile(t, extents, 0, probe)

	var unresolved int
	for i, e := range extents {
		switch i % 2 {
		case 0:
			if e.Kind != ExtentMapped {
				t.Errorf("extent %d kind = %v, want mapped from the child", i, e.Kind)
			}
		default:
			if e.Kind != ExtentUnresolved {
				t.Errorf("extent %d kind = %v, want unresolved", i, e.Kind)
			}
			unresolved++
		}
	}
	if unresolved != 3 {
		t.Errorf("got %d unresolved extents, want 3", unresolved)
	}

	checkExtentsMatchReads(t, d, extents, map[string][]byte{
		filepath.Join(dir, "child.vhd"): childImg,
	})
}

// ============================================================================
// VHDX
// ============================================================================

func TestExtentsVHDXSparseDisk(t *testing.T) {
	// Only block 0 is present; every other block of the 4097 MB disk is a hole.
	img := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
	})

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	extents, err := d.AllExtents()
	if err != nil {
		t.Fatalf("AllExtents: %v", err)
	}
	checkExtentsTile(t, extents, 0, int64(vhdxDiffVirtual))

	if len(extents) != 2 {
		t.Fatalf("got %d extents, want 2 (one mapped block, one long hole): %+v", len(extents), extents)
	}
	if extents[0].Kind != ExtentMapped || extents[0].Length != vhdxDiffBlockSize {
		t.Errorf("extent 0 = %+v, want one mapped block", extents[0])
	}
	if extents[1].Kind != ExtentZero {
		t.Errorf("extent 1 kind = %v, want zero", extents[1].Kind)
	}

	// The whole 4 GB device is backed by a single megabyte of real data, which is
	// precisely what makes the extent map worth having.
	mapped, err := d.MappedBytes()
	if err != nil {
		t.Fatalf("MappedBytes: %v", err)
	}
	if mapped != vhdxDiffBlockSize {
		t.Errorf("MappedBytes() = %d, want %d", mapped, vhdxDiffBlockSize)
	}
}

func TestExtentsVHDXDifferencingPerSector(t *testing.T) {
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

	const probe = 6 * vhdxDiffSectorSize
	extents, err := child.Extents(0, probe)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}
	checkExtentsTile(t, extents, 0, probe)

	if len(extents) != 6 {
		t.Fatalf("got %d extents, want 6 alternating sectors: %+v", len(extents), extents)
	}
	for i, e := range extents {
		wantChain := 1
		if i%2 == 0 {
			wantChain = 0
		}
		if e.ChainIndex != wantChain {
			t.Errorf("extent %d ChainIndex = %d, want %d", i, e.ChainIndex, wantChain)
		}
	}
}

// TestExtentsAgreeWithReadsAcrossFixtures is a broad consistency sweep: for every
// fixture, the extent map's claims must match what ReadAt returns.
func TestExtentsAgreeWithReadsAcrossFixtures(t *testing.T) {
	const (
		blockSize = 2 * 1024 * 1024
		mediaSize = uint64(blockSize * 3)
	)

	cases := map[string][]byte{
		"fixed": buildFixedVHD(8192, func(p []byte) {
			for i := range p {
				p[i] = byte(i % 251)
			}
		}),
		"dynamic-holes": buildDynamicVHD(types.DiskTypeDynamic, mediaSize, blockSize, []vhdBlock{
			{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x11, blockSize)},
			{allocated: false},
			{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x33, blockSize)},
		}, "", [16]byte{}),
		"dynamic-all-holes": buildDynamicVHD(types.DiskTypeDynamic, mediaSize, blockSize, []vhdBlock{
			{allocated: false}, {allocated: false}, {allocated: false},
		}, "", [16]byte{}),
	}

	for name, img := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatalf("OpenVHD: %v", err)
			}
			defer d.Close()

			extents, err := d.AllExtents()
			if err != nil {
				t.Fatalf("AllExtents: %v", err)
			}
			checkExtentsTile(t, extents, 0, int64(d.Size()))
			checkExtentsMatchReads(t, d, extents, map[string][]byte{"": img})
		})
	}
}
