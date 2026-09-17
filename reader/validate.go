// SPDX-License-Identifier: MIT

package reader

import (
	"errors"
	"fmt"
	"sort"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// ErrCorruptImage is returned when an image's headers are internally
// inconsistent, fail a checksum, or describe structures that do not fit the
// file.
//
// It is the same sentinel the parsers below the reader wrap, so errors.Is
// answers the question regardless of which layer noticed. A parser that can say
// where the problem is returns a *types.StructuralError wrapping it.
var ErrCorruptImage = types.ErrCorruptImage

// ErrUnsupportedFeature is returned when an image is well-formed but uses
// something this library does not implement, such as a reserved VHD disk type
// or a VHDX metadata item marked required that this parser does not know.
//
// It is deliberately distinct from ErrCorruptImage. One says the file is
// broken; the other says to try a different tool. Conflating them sends an
// examiner looking for damage that is not there.
var ErrUnsupportedFeature = types.ErrUnsupportedFeature

// StructuralError says what was being parsed, where in the image, and what was
// wrong with it. Errors from the parsers are returned as *StructuralError
// wherever the location is known.
type StructuralError = types.StructuralError

// ErrDirtyImage is returned when a VHDX image carries an unreplayed log. Its
// block allocation table and metadata may be stale, so decoded contents cannot
// be trusted. Set Options.AllowDirtyImage to read it anyway.
var ErrDirtyImage = errors.New("libvhdi: image has an unreplayed log and may be stale")

// maxBATBytes caps how large a block allocation table may be before it is
// treated as malformed rather than merely large.
//
// A 64 MiB VHD BAT describes 16.7 million 2 MiB blocks, or 33 TB of virtual
// disk; a VHDX BAT of the same size describes 8.4 million 1 MiB blocks. Both are
// far beyond any real image, so this bounds header-driven allocation without
// constraining legitimate use.
const maxBATBytes = 64 << 20

// validateVHDFixed checks that a fixed disk's payload actually fits its file.
//
// footerBytes is the on-disk length of the footer that was actually read, which
// is 512 for a conformant image and 511 for one written by Virtual PC before the
// format was documented. Assuming 512 would reject every legacy image, since its
// payload runs one byte closer to the end of the file.
func validateVHDFixed(footer *types.ParsedFileFooter, fileSize, footerBytes int64) error {
	if footer.MediaSize == 0 {
		return fmt.Errorf("%w: fixed disk has zero media size", ErrCorruptImage)
	}
	if footer.MediaSize > uint64(maxInt64) {
		return fmt.Errorf("%w: fixed disk media size %d overflows", ErrCorruptImage, footer.MediaSize)
	}

	if footerBytes <= 0 {
		footerBytes = types.VHDFooterSize
	}

	// The payload occupies everything before the trailing footer.
	if need := int64(footer.MediaSize) + footerBytes; fileSize > 0 && need > fileSize {
		return fmt.Errorf("%w: fixed disk claims %d payload bytes but the file holds %d",
			ErrCorruptImage, footer.MediaSize, fileSize)
	}
	return nil
}

// validateVHDDynamic cross-checks the dynamic disk header against the footer and
// the file size.
//
// Every field here comes from the image and is therefore attacker-controlled;
// NumberOfBlocks in particular sizes a heap allocation.
func validateVHDDynamic(footer *types.ParsedFileFooter, h *types.ParsedDynamicDiskHeader, fileSize int64) error {
	if h.BlockSize == 0 {
		return fmt.Errorf("%w: dynamic disk block size is zero", ErrCorruptImage)
	}
	if footer.MediaSize == 0 {
		return fmt.Errorf("%w: dynamic disk has zero media size", ErrCorruptImage)
	}
	if h.NumberOfBlocks == 0 {
		return fmt.Errorf("%w: dynamic disk has zero blocks", ErrCorruptImage)
	}

	// The BAT holds one 4-byte entry per block.
	batBytes := int64(h.NumberOfBlocks) * 4
	if batBytes > maxBATBytes {
		return fmt.Errorf("%w: block allocation table of %d bytes exceeds the %d byte limit (%d blocks)",
			ErrCorruptImage, batBytes, int64(maxBATBytes), h.NumberOfBlocks)
	}

	if h.BlockTableOffset < 0 {
		return fmt.Errorf("%w: negative block table offset", ErrCorruptImage)
	}
	if fileSize > 0 {
		if h.BlockTableOffset > fileSize {
			return fmt.Errorf("%w: block table offset %d is beyond the end of a %d byte file",
				ErrCorruptImage, h.BlockTableOffset, fileSize)
		}
		if h.BlockTableOffset+batBytes > fileSize {
			return fmt.Errorf("%w: block allocation table at %d spans %d bytes, past the end of a %d byte file",
				ErrCorruptImage, h.BlockTableOffset, batBytes, fileSize)
		}
	}

	// The specification requires the table to hold one entry per block of the
	// disk. A table shorter than that would silently truncate the device.
	needed := (footer.MediaSize + uint64(h.BlockSize) - 1) / uint64(h.BlockSize)
	if uint64(h.NumberOfBlocks) < needed {
		return fmt.Errorf("%w: block allocation table holds %d entries but the %d byte disk needs %d",
			ErrCorruptImage, h.NumberOfBlocks, footer.MediaSize, needed)
	}

	return nil
}

// errUnknownRequiredRegion marks the one region-table failure that must never be
// retried against the other copy. Every other malformation means "this table is
// damaged, try the spare"; this one means "this image needs a feature we do not
// have", which the spare says too.
var errUnknownRequiredRegion = fmt.Errorf(
	"%w: VHDX region table contains an unknown required entry", ErrCorruptImage)

// vhdxRegionPointers carries the two regions this library locates out of a
// region table, so reading one table is a single call with a single error.
type vhdxRegionPointers struct {
	batOffset  int64
	batSize    int64
	metaOffset int64
	metaSize   uint32
}

// vhdxRegionAlignment is the alignment MS-VHDX requires of every region's file
// offset.
//
// The first megabyte is reserved for the file identifier, both headers and both
// region tables, so requiring offsets to be positive multiples of 1 MB also
// guarantees no region can be placed on top of them. A 512-byte check, which is
// what this used to be, admits a region starting inside the header area.
const vhdxRegionAlignment = 1 << 20

// validateVHDXRegionTable checks a whole region table for the structural rules
// that govern regions as a set, which no per-pointer check can see.
//
// Two regions that overlap are the dangerous case. Both would parse, both would
// return plausible structures, and whichever was read second would be decoding
// bytes that belong to the other -- so the failure surfaces as wrong data rather
// than as an error. Unknown region types are included deliberately: this library
// does not know what such a region contains, but it does know the region
// occupies that space and that nothing else may.
func validateVHDXRegionTable(regions []types.ParsedRegionTableEntry, fileSize int64) error {
	type span struct {
		name  string
		start int64
		end   int64
	}

	spans := make([]span, 0, len(regions))
	seen := make(map[[16]byte]bool, len(regions))

	for _, r := range regions {
		name := vhdxRegionName(r.TypeIdentifier)

		// The specification allows at most one entry per region type. Two would
		// leave which of them describes the region undefined.
		if seen[r.TypeIdentifier] {
			return fmt.Errorf("%w: region table lists the %s region twice", ErrCorruptImage, name)
		}
		seen[r.TypeIdentifier] = true

		if r.DataOffset <= 0 || r.DataOffset%vhdxRegionAlignment != 0 {
			return fmt.Errorf("%w: %s region offset %d is not a positive multiple of 1 MB",
				ErrCorruptImage, name, r.DataOffset)
		}

		end := r.DataOffset + int64(r.DataSize)
		if end < r.DataOffset {
			return fmt.Errorf("%w: %s region at %d with size %d overflows",
				ErrCorruptImage, name, r.DataOffset, r.DataSize)
		}
		if fileSize > 0 && end > fileSize {
			return fmt.Errorf("%w: %s region at %d spans %d bytes, past the end of a %d byte file",
				ErrCorruptImage, name, r.DataOffset, r.DataSize, fileSize)
		}

		if r.DataSize == 0 {
			// A zero-length region occupies nothing and so cannot overlap.
			continue
		}
		spans = append(spans, span{name: name, start: r.DataOffset, end: end})
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return fmt.Errorf("%w: the %s region at %d overlaps the %s region at %d",
				ErrCorruptImage, spans[i].name, spans[i].start, spans[i-1].name, spans[i-1].start)
		}
	}

	return nil
}

// vhdxRegionName labels a region for an error message. An unknown GUID is
// rendered in full, since that is the only thing that identifies it.
func vhdxRegionName(id [16]byte) string {
	switch id {
	case types.RegionTypeBAT:
		return "BAT"
	case types.RegionTypeMetadata:
		return "metadata"
	default:
		return "unknown (" + binaryutil.GUIDToString(id) + ")"
	}
}

// validateVHDXRegions checks the region table's pointers before anything
// dereferences them, so a bad offset is reported as a malformed image rather than
// surfacing as an I/O error from deep inside a parser.
//
// This repeats the bounds and alignment checks validateVHDXRegionTable already
// made, because the two pointers can also reach here having been extracted from
// the secondary table, and because a caller must not be able to introduce an
// unchecked offset by reaching openVHDX another way.
func validateVHDXRegions(batOffset, batRegionSize, metaOffset int64, metaRegionSize uint32, fileSize int64) error {
	type region struct {
		name   string
		offset int64
		size   int64
	}
	for _, r := range []region{
		{"BAT", batOffset, batRegionSize},
		{"metadata", metaOffset, int64(metaRegionSize)},
	} {
		if r.offset <= 0 || r.offset%vhdxRegionAlignment != 0 {
			return fmt.Errorf("%w: %s region offset %d is not a positive multiple of 1 MB",
				ErrCorruptImage, r.name, r.offset)
		}
		if r.size < 0 {
			return fmt.Errorf("%w: %s region has negative size %d", ErrCorruptImage, r.name, r.size)
		}
		if fileSize > 0 && r.offset+r.size > fileSize {
			return fmt.Errorf("%w: %s region at %d spans %d bytes, past the end of a %d byte file",
				ErrCorruptImage, r.name, r.offset, r.size, fileSize)
		}
	}

	if batRegionSize > 0 && int64(metaRegionSize) > 0 {
		if batOffset < metaOffset+int64(metaRegionSize) && metaOffset < batOffset+batRegionSize {
			return fmt.Errorf("%w: the BAT region at %d overlaps the metadata region at %d",
				ErrCorruptImage, batOffset, metaOffset)
		}
	}
	return nil
}

// validateVHDXGeometry checks the metadata that sizes the BAT allocation.
func validateVHDXGeometry(m *types.MetadataValues, batOffset, batRegionSize, fileSize int64) error {
	// A zero block size means the required File Parameters metadata item was
	// absent: parseFileParameters rejects any non-zero value below 1 MiB.
	if m.BlockSize == 0 {
		return fmt.Errorf("%w: required File Parameters metadata item is missing", ErrCorruptImage)
	}
	if m.VirtualDiskSize == 0 {
		return fmt.Errorf("%w: required Virtual Disk Size metadata item is missing or zero", ErrCorruptImage)
	}
	if m.LogicalSectorSize == 0 {
		return fmt.Errorf("%w: logical sector size is zero", ErrCorruptImage)
	}
	if m.VirtualDiskSize%uint64(m.LogicalSectorSize) != 0 {
		return fmt.Errorf("%w: virtual disk size %d is not a multiple of the %d byte logical sector",
			ErrCorruptImage, m.VirtualDiskSize, m.LogicalSectorSize)
	}

	blockCount := (m.VirtualDiskSize + uint64(m.BlockSize) - 1) / uint64(m.BlockSize)
	if blockCount == 0 {
		return fmt.Errorf("%w: virtual disk spans zero blocks", ErrCorruptImage)
	}
	if blockCount > uint64(^uint32(0)) {
		return fmt.Errorf("%w: virtual disk size %d spans %d blocks, more than the format allows",
			ErrCorruptImage, m.VirtualDiskSize, blockCount)
	}

	// chunkRatio is the number of payload entries between sector bitmap entries.
	chunkRatio := (uint64(1) << 23) * uint64(m.LogicalSectorSize) / uint64(m.BlockSize)
	if chunkRatio == 0 {
		return fmt.Errorf("%w: computed chunk ratio is zero for block size %d and sector size %d",
			ErrCorruptImage, m.BlockSize, m.LogicalSectorSize)
	}

	// Total entries = payload entries plus the interleaved sector bitmap ones.
	totalEntries := blockCount + (blockCount+chunkRatio-1)/chunkRatio
	batBytes := totalEntries * 8
	if batBytes > uint64(maxBATBytes) {
		return fmt.Errorf("%w: block allocation table of %d bytes exceeds the %d byte limit (%d blocks)",
			ErrCorruptImage, batBytes, int64(maxBATBytes), blockCount)
	}

	if batOffset < 0 {
		return fmt.Errorf("%w: negative BAT region offset", ErrCorruptImage)
	}
	if batRegionSize > 0 && int64(batBytes) > batRegionSize {
		return fmt.Errorf("%w: block allocation table needs %d bytes but its region declares %d",
			ErrCorruptImage, batBytes, batRegionSize)
	}
	if fileSize > 0 && batOffset+int64(batBytes) > fileSize {
		return fmt.Errorf("%w: block allocation table at %d spans %d bytes, past the end of a %d byte file",
			ErrCorruptImage, batOffset, batBytes, fileSize)
	}

	return nil
}

const maxInt64 = int64(^uint64(0) >> 1)
