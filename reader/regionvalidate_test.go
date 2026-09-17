// SPDX-License-Identifier: MIT

package reader

import (
	"errors"
	"strings"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// MS-VHDX requires every region to begin on a 1 MB boundary, and the first
// megabyte holds the file identifier, both headers and both region tables. A
// 512-byte alignment check, which is what this used to be, admits a region
// starting inside that reserved area.
//
// Overlap is the more dangerous gap. Two regions occupying the same bytes both
// parse, both return plausible structures, and whichever is read second decodes
// bytes belonging to the other -- so the defect surfaces as wrong data rather
// than as an error.

func region(id [16]byte, offset int64, size uint32, required bool) types.ParsedRegionTableEntry {
	return types.ParsedRegionTableEntry{
		TypeIdentifier: id,
		DataOffset:     offset,
		DataSize:       size,
		IsRequired:     required,
	}
}

var unknownRegionGUID = [16]byte{0xaa, 0xbb, 0xcc, 0xdd}

func TestRegionTableAcceptsAConformantLayout(t *testing.T) {
	regions := []types.ParsedRegionTableEntry{
		region(types.RegionTypeBAT, 1<<20, 512<<10, true),
		region(types.RegionTypeMetadata, 2<<20, 64<<10, true),
	}
	if err := validateVHDXRegionTable(regions, 4<<20); err != nil {
		t.Fatalf("rejected a conformant region table: %v", err)
	}
}

func TestRegionTableRejectsSubMegabyteAlignment(t *testing.T) {
	// 512 KB is sector-aligned and 512-byte aligned, so the old check passed it.
	// It is not 1 MB aligned, and a region there can reach into the header area.
	regions := []types.ParsedRegionTableEntry{
		region(types.RegionTypeBAT, 512<<10, 4096, true),
		region(types.RegionTypeMetadata, 2<<20, 64<<10, true),
	}
	err := validateVHDXRegionTable(regions, 4<<20)
	if err == nil {
		t.Fatal("accepted a region at 512 KB, which is not a 1 MB multiple")
	}
	if !errors.Is(err, ErrCorruptImage) {
		t.Fatalf("error does not wrap ErrCorruptImage: %v", err)
	}
	if !strings.Contains(err.Error(), "1 MB") {
		t.Fatalf("error does not name the alignment rule: %v", err)
	}
}

func TestRegionTableRejectsAZeroOffset(t *testing.T) {
	// Offset 0 is the file identifier. A region there would overwrite the very
	// bytes used to recognise the format.
	regions := []types.ParsedRegionTableEntry{
		region(types.RegionTypeBAT, 0, 4096, true),
	}
	if err := validateVHDXRegionTable(regions, 4<<20); err == nil {
		t.Fatal("accepted a region at offset 0")
	}
}

func TestRegionTableRejectsOverlap(t *testing.T) {
	// The BAT declares 2 MB starting at 1 MB, so it runs to 3 MB. Metadata at
	// 2 MB sits inside it.
	regions := []types.ParsedRegionTableEntry{
		region(types.RegionTypeBAT, 1<<20, 2<<20, true),
		region(types.RegionTypeMetadata, 2<<20, 64<<10, true),
	}
	err := validateVHDXRegionTable(regions, 8<<20)
	if err == nil {
		t.Fatal("accepted two regions occupying the same bytes")
	}
	if !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("error does not say the regions overlap: %v", err)
	}
}

func TestRegionTableRejectsOverlapWithAnUnknownRegion(t *testing.T) {
	// This library does not know what an unknown region contains, but it does
	// know the region occupies that space and that nothing else may. Excluding
	// unknown regions from the overlap check would miss exactly the case where
	// the library is most likely to be wrong about the layout.
	regions := []types.ParsedRegionTableEntry{
		region(types.RegionTypeBAT, 1<<20, 2<<20, true),
		region(types.RegionTypeMetadata, 4<<20, 64<<10, true),
		region(unknownRegionGUID, 2<<20, 1<<20, false),
	}
	if err := validateVHDXRegionTable(regions, 8<<20); err == nil {
		t.Fatal("accepted an unknown region overlapping the BAT")
	}
}

func TestRegionTableAllowsAdjacentRegions(t *testing.T) {
	// Touching is not overlapping. A region ending exactly where the next
	// begins is the densest conformant layout and must not be refused.
	regions := []types.ParsedRegionTableEntry{
		region(types.RegionTypeBAT, 1<<20, 1<<20, true),
		region(types.RegionTypeMetadata, 2<<20, 1<<20, true),
	}
	if err := validateVHDXRegionTable(regions, 4<<20); err != nil {
		t.Fatalf("rejected two exactly adjacent regions: %v", err)
	}
}

func TestRegionTableRejectsDuplicateRegionTypes(t *testing.T) {
	// Two entries for the same region leave it undefined which one describes
	// the region.
	regions := []types.ParsedRegionTableEntry{
		region(types.RegionTypeBAT, 1<<20, 64<<10, true),
		region(types.RegionTypeBAT, 2<<20, 64<<10, true),
	}
	err := validateVHDXRegionTable(regions, 4<<20)
	if err == nil {
		t.Fatal("accepted a region table listing the BAT twice")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Fatalf("error does not say the region is listed twice: %v", err)
	}
}

func TestRegionTableRejectsARegionPastTheEndOfTheFile(t *testing.T) {
	regions := []types.ParsedRegionTableEntry{
		region(types.RegionTypeBAT, 3<<20, 4<<20, true),
	}
	if err := validateVHDXRegionTable(regions, 4<<20); err == nil {
		t.Fatal("accepted a region extending past the end of the file")
	}
}

func TestRegionTableIgnoresFileSizeWhenUnknown(t *testing.T) {
	// A caller may open a VHDX from a bare io.ReaderAt with no derivable size.
	// VHDX locates every structure from fixed offsets, so that is legitimate,
	// and the bounds check simply has nothing to compare against.
	regions := []types.ParsedRegionTableEntry{
		region(types.RegionTypeBAT, 1<<20, 1<<20, true),
	}
	if err := validateVHDXRegionTable(regions, 0); err != nil {
		t.Fatalf("rejected a region table when the file size was unknown: %v", err)
	}
}

func TestRegionTableAllowsAZeroLengthRegion(t *testing.T) {
	// A zero-length region occupies nothing, so it cannot collide with anything.
	regions := []types.ParsedRegionTableEntry{
		region(types.RegionTypeBAT, 1<<20, 1<<20, true),
		region(unknownRegionGUID, 1<<20, 0, false),
	}
	if err := validateVHDXRegionTable(regions, 4<<20); err != nil {
		t.Fatalf("rejected a zero-length region: %v", err)
	}
}

func TestValidateVHDXRegionsRejectsOverlappingPointers(t *testing.T) {
	// The extracted pointers are re-checked because they can reach openVHDX
	// from the secondary table, which has its own validation path.
	err := validateVHDXRegions(1<<20, 2<<20, 2<<20, 64<<10, 8<<20)
	if err == nil {
		t.Fatal("accepted a BAT pointer overlapping the metadata pointer")
	}
	if !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("error does not say the regions overlap: %v", err)
	}
}

func TestValidateVHDXRegionsRejectsSubMegabyteAlignment(t *testing.T) {
	if err := validateVHDXRegions(512<<10, 4096, 2<<20, 4096, 8<<20); err == nil {
		t.Fatal("accepted a BAT pointer at 512 KB")
	}
}
