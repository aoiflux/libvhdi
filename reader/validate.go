// SPDX-License-Identifier: MIT

package reader

import (
	"errors"
	"fmt"

	"github.com/aoiflux/libvhdi/types"
)

// ErrCorruptImage is returned when an image's headers are internally
// inconsistent or describe structures that do not fit the file.
var ErrCorruptImage = errors.New("libvhdi: corrupt or malformed image")

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
func validateVHDFixed(footer *types.ParsedFileFooter, fileSize int64) error {
	if footer.MediaSize == 0 {
		return fmt.Errorf("%w: fixed disk has zero media size", ErrCorruptImage)
	}
	if footer.MediaSize > uint64(maxInt64) {
		return fmt.Errorf("%w: fixed disk media size %d overflows", ErrCorruptImage, footer.MediaSize)
	}

	// The payload occupies everything before the trailing footer.
	if need := int64(footer.MediaSize) + types.VHDFooterSize; fileSize > 0 && need > fileSize {
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

// validateVHDXGeometry checks the metadata that sizes the BAT allocation.
//
// batRegionSize is the size the region table declared for the BAT region, and
// is cross-checked here rather than discarded.
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
