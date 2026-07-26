// SPDX-License-Identifier: MIT

// Package bat provides Block Allocation Table (BAT) parsing and lookup logic.
package bat

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/aoiflux/libvhdi/types"
)

// VHDBATParser provides functionality to parse VHD Block Allocation Tables.
type VHDBATParser struct {
	reader io.ReaderAt
}

// NewVHDBATParser creates a new VHD BAT parser.
func NewVHDBATParser(r io.ReaderAt) *VHDBATParser {
	return &VHDBATParser{reader: r}
}

// ReadBAT reads the VHD BAT from the specified offset and block count.
func (p *VHDBATParser) ReadBAT(offset int64, blockCount uint32, blockSize uint32) (*types.VHDBlockAllocationTable, error) {
	if blockCount == 0 {
		return nil, errors.New("invalid block count")
	}

	// Calculate sector bitmap size per block (must be sector-aligned, 512 bytes).
	sectorBitmapSize := blockSize / (512 * 8)
	if sectorBitmapSize%512 != 0 {
		sectorBitmapSize = (sectorBitmapSize/512 + 1) * 512
	}

	// Bulk-read all BAT entries in one I/O call.
	rawBuf := make([]byte, int(blockCount)*4)
	if _, err := p.reader.ReadAt(rawBuf, offset); err != nil {
		return nil, err
	}

	bat := &types.VHDBlockAllocationTable{
		NumberOfEntries:  blockCount,
		TableOffset:      offset,
		BlockSize:        blockSize,
		SectorBitmapSize: sectorBitmapSize,
		Entries:          make([]types.BlockAllocationEntry, blockCount),
	}

	for i := uint32(0); i < blockCount; i++ {
		entryValue := binary.BigEndian.Uint32(rawBuf[i*4:])
		isAllocated := entryValue != types.VHDUnallocatedBlockMarker

		var fileOffset int64 = -1
		if isAllocated {
			fileOffset = int64(entryValue)*512 + int64(sectorBitmapSize)
		}

		bat.Entries[i] = types.BlockAllocationEntry{
			IsAllocated: isAllocated,
			FileOffset:  fileOffset,
		}
	}

	return bat, nil
}

// ============================================================================
// VHDX BAT PARSER
// ============================================================================

// VHDXBATParser provides functionality to parse VHDX Block Allocation Tables.
type VHDXBATParser struct {
	reader io.ReaderAt
}

// NewVHDXBATParser creates a new VHDX BAT parser.
func NewVHDXBATParser(r io.ReaderAt) *VHDXBATParser {
	return &VHDXBATParser{reader: r}
}

// ReadBAT reads the VHDX BAT from the specified offset and block count.
// For VHDX, the block size should be obtained from metadata (typically 1MB or 32MB).
// sectorSize is the logical sector size (typically 512 or 4096); pass 0 to default to 512.
//
// The VHDX BAT interleaves sector bitmap entries: for every chunkRatio data block
// entries there is one sector bitmap entry:
//
//	[data_0..data_(chunkRatio-1)] [sbitmap_0] [data_chunkRatio..] [sbitmap_1] ...
//
// where chunkRatio = (2^23 × logical_sector_size) / block_size.
func (p *VHDXBATParser) ReadBAT(offset int64, blockCount uint32, blockSize uint32, sectorSize uint32) (*types.VHDXBlockAllocationTable, error) {
	if blockCount == 0 {
		return nil, errors.New("invalid block count")
	}
	if blockSize == 0 {
		return nil, errors.New("invalid block size")
	}
	if sectorSize == 0 {
		sectorSize = 512
	}

	// chunk_ratio = (2^23 × logical_sector_size) / block_size
	// This is the number of data block entries between consecutive sector bitmap entries.
	chunkRatio := (uint64(1<<23) * uint64(sectorSize)) / uint64(blockSize)
	if chunkRatio == 0 {
		return nil, errors.New("computed chunk ratio is zero: block size too large")
	}

	// Sector bitmap covers chunkRatio blocks each with blockSize/sectorSize sectors.
	// Bitmap size = (chunkRatio * blockSize / sectorSize) / 8 bytes.
	sectorBitmapSize := chunkRatio * uint64(blockSize) / uint64(sectorSize) / 8

	// Total BAT entries = data blocks + sector bitmap entries.
	// One sector bitmap entry is appended after every chunkRatio data blocks.
	sectorBitmapCount := (uint64(blockCount) + chunkRatio - 1) / chunkRatio
	totalEntries := uint64(blockCount) + sectorBitmapCount

	// Bulk-read all raw BAT entries in one I/O call.
	rawBuf := make([]byte, totalEntries*8)
	if _, err := p.reader.ReadAt(rawBuf, offset); err != nil {
		return nil, err
	}
	rawEntries := make([]uint64, totalEntries)
	for i := uint64(0); i < totalEntries; i++ {
		rawEntries[i] = binary.LittleEndian.Uint64(rawBuf[i*8:])
	}

	bat := &types.VHDXBlockAllocationTable{
		NumberOfEntries:     blockCount,
		TableOffset:         offset,
		BlockSize:           blockSize,
		ChunkRatio:          uint32(chunkRatio),
		SectorSize:          sectorSize,
		SectorBitmapSize:    uint32(sectorBitmapSize),
		SectorBitmapOffsets: make([]int64, sectorBitmapCount),
		Entries:             make([]types.BlockAllocationEntry, blockCount),
	}

	// Parse sector bitmap BAT entries.
	for c := uint64(0); c < sectorBitmapCount; c++ {
		// The sector bitmap entry for chunk c is at raw index c*(chunkRatio+1)+chunkRatio.
		sbBatIndex := c*(chunkRatio+1) + chunkRatio
		if sbBatIndex >= totalEntries {
			bat.SectorBitmapOffsets[c] = -1
			continue
		}
		sbEntry := rawEntries[sbBatIndex]
		sbState := sbEntry & 0x7
		if sbState == 6 { // SECTOR_BITMAP_BLOCK_PRESENT
			offsetBits := (sbEntry >> 20) & 0x00FFFFFFFFFFFFFF
			bat.SectorBitmapOffsets[c] = int64(offsetBits) * (1024 * 1024)
		} else {
			bat.SectorBitmapOffsets[c] = -1
		}
	}

	for i := uint32(0); i < blockCount; i++ {
		// BAT index for data block i, accounting for interleaved bitmap entries:
		//   batIndex = i + i/chunkRatio
		batIndex := uint64(i) + uint64(i)/chunkRatio
		entryValue := rawEntries[batIndex]

		// Extract block state (bits 0-2)
		blockState := types.BlockState(entryValue & 0x7)

		// Check if block has physical data (states 6=FullyPresent, 7=PartiallyPresent)
		isAllocated := blockState == types.BlockStateFullyAllocated || blockState == types.BlockStatePartiallyAllocated

		var fileOffset int64 = -1
		if isAllocated {
			// Extract file offset (bits 20-63); stored in units of 1 MB.
			offsetBits := (entryValue >> 20) & 0x00FFFFFFFFFFFFFF
			fileOffset = int64(offsetBits) * (1024 * 1024)
		}

		bat.Entries[i] = types.BlockAllocationEntry{
			IsAllocated: isAllocated,
			FileOffset:  fileOffset,
			BlockState:  blockState,
		}
	}

	return bat, nil
}

// ============================================================================
// BAT LOOKUP OPERATIONS
// ============================================================================

// BlockOffset returns the physical file offset for a block in a VHD BAT.
func BlockOffsetVHD(bat *types.VHDBlockAllocationTable, index uint32) (int64, error) {
	if index >= bat.NumberOfEntries {
		return -1, errors.New("block index out of range")
	}

	entry := bat.Entries[index]
	if !entry.IsAllocated {
		return -1, nil
	}

	return entry.FileOffset, nil
}

// IsAllocatedVHD checks if a block is allocated in a VHD BAT.
func IsAllocatedVHD(bat *types.VHDBlockAllocationTable, index uint32) (bool, error) {
	if index >= bat.NumberOfEntries {
		return false, errors.New("block index out of range")
	}

	return bat.Entries[index].IsAllocated, nil
}

// BlockCountVHD returns the total number of blocks in a VHD BAT.
func BlockCountVHD(bat *types.VHDBlockAllocationTable) uint32 {
	return bat.NumberOfEntries
}

// BlockOffsetVHDX returns the physical file offset for a block in a VHDX BAT.
func BlockOffsetVHDX(bat *types.VHDXBlockAllocationTable, index uint32) (int64, error) {
	if index >= bat.NumberOfEntries {
		return -1, errors.New("block index out of range")
	}

	entry := bat.Entries[index]
	if !entry.IsAllocated {
		return -1, nil
	}

	return entry.FileOffset, nil
}

// IsAllocatedVHDX checks if a block is allocated in a VHDX BAT.
func IsAllocatedVHDX(bat *types.VHDXBlockAllocationTable, index uint32) (bool, error) {
	if index >= bat.NumberOfEntries {
		return false, errors.New("block index out of range")
	}

	return bat.Entries[index].IsAllocated, nil
}

// BlockCountVHDX returns the total number of blocks in a VHDX BAT.
func BlockCountVHDX(bat *types.VHDXBlockAllocationTable) uint32 {
	return bat.NumberOfEntries
}

// ============================================================================
// SECTOR BITMAP UTILITIES
// ============================================================================

// SectorBitmap provides methods to work with VHD block bitmaps.
//
// A VHD block bitmap holds one bit per 512-byte sector of the block, stored
// most-significant-bit first: the first sector of the block is bit 7 of byte 0.
// This is the ordering used by the VHD specification, the reference libvhdi C
// implementation and qemu's vpc driver.
//
// VHDX sector bitmaps use the opposite (least-significant-bit first) ordering
// and are handled separately; do not use this type for them.
type SectorBitmap struct {
	data []byte
}

// NewSectorBitmap creates a new sector bitmap from raw data.
func NewSectorBitmap(data []byte) *SectorBitmap {
	b := make([]byte, len(data))
	copy(b, data)
	return &SectorBitmap{data: b}
}

// ReadSectorBitmap reads a sector bitmap from file.
func ReadSectorBitmap(reader io.ReaderAt, offset int64, bitmapSize uint32) (*SectorBitmap, error) {
	data := make([]byte, bitmapSize)
	if _, err := reader.ReadAt(data, offset); err != nil {
		return nil, err
	}
	return &SectorBitmap{data: data}, nil
}

// IsSectorAllocated checks if a sector (within a block) is allocated.
//
// Bits are read most-significant-bit first, per the VHD specification.
func (sb *SectorBitmap) IsSectorAllocated(sectorIndex uint32) bool {
	byteIndex := sectorIndex / 8
	bitIndex := sectorIndex % 8

	if byteIndex >= uint32(len(sb.data)) {
		return false
	}

	byte_ := sb.data[byteIndex]
	return (byte_ & (1 << (7 - bitIndex))) != 0
}

// GetAllocatedSectorRanges returns a list of contiguous allocated sector ranges.
func (sb *SectorBitmap) GetAllocatedSectorRanges() []types.SectorRangeDescriptor {
	var ranges []types.SectorRangeDescriptor

	totalSectors := uint32(len(sb.data)) * 8
	inRange := false
	var rangeStart uint32

	for i := uint32(0); i < totalSectors; i++ {
		isAllocated := sb.IsSectorAllocated(i)

		if isAllocated && !inRange {
			// Start of a new range
			rangeStart = i
			inRange = true
		} else if !isAllocated && inRange {
			// End of a range
			ranges = append(ranges, types.SectorRangeDescriptor{
				StartOffset: uint64(rangeStart) * types.DefaultSectorSize,
				EndOffset:   uint64(i) * types.DefaultSectorSize,
			})
			inRange = false
		}
	}

	// Handle final range if it extends to the end
	if inRange {
		ranges = append(ranges, types.SectorRangeDescriptor{
			StartOffset: uint64(rangeStart) * types.DefaultSectorSize,
			EndOffset:   uint64(totalSectors) * types.DefaultSectorSize,
		})
	}

	return ranges
}
