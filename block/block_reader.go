// Package block provides block-level reading and sector I/O for virtual disks.
package block

import (
	"errors"
	"io"

	"github.com/aoiflux/libvhdi/bat"
	"github.com/aoiflux/libvhdi/types"
)

// VHDBlockReader reads block and sector data from VHD dynamic disks.
type VHDBlockReader struct {
	reader          io.ReaderAt
	bat             *types.VHDBlockAllocationTable
	blockSize       uint32
	sectorSize      uint32
	sectorsPerBlock uint32
}

// NewVHDBlockReader creates a VHD block reader using a parsed BAT.
func NewVHDBlockReader(r io.ReaderAt, b *types.VHDBlockAllocationTable, sectorSize uint32) *VHDBlockReader {
	if sectorSize == 0 {
		sectorSize = types.DefaultSectorSize
	}
	sectorsPerBlock := b.BlockSize / sectorSize
	return &VHDBlockReader{
		reader:          r,
		bat:             b,
		blockSize:       b.BlockSize,
		sectorSize:      sectorSize,
		sectorsPerBlock: sectorsPerBlock,
	}
}

// ReadAt implements io.ReaderAt — reads from the virtual disk address space.
func (br *VHDBlockReader) ReadAt(p []byte, virtualOffset int64) (int, error) {
	total := 0
	for total < len(p) {
		n, err := br.readSegment(p[total:], virtualOffset+int64(total))
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			// No progress and no error would spin forever.
			return total, io.ErrUnexpectedEOF
		}
	}
	return total, nil
}

// readSegment reads as many bytes as possible starting at virtualOffset, bounded
// by the end of the block that contains virtualOffset.
func (br *VHDBlockReader) readSegment(dst []byte, virtualOffset int64) (int, error) {
	// A zero block size would divide by zero below. It can only arrive from a
	// malformed BAT, so report it rather than crashing the caller.
	if br.blockSize == 0 {
		return 0, errors.New("libvhdi: VHD block size is zero")
	}
	blockIndex := uint32(virtualOffset / int64(br.blockSize))
	offsetInBlock := virtualOffset % int64(br.blockSize)

	if blockIndex >= br.bat.NumberOfEntries {
		return 0, io.EOF
	}

	// How many bytes are available within this block
	available := int64(br.blockSize) - offsetInBlock
	want := int64(len(dst))
	if want > available {
		want = available
	}

	fileOffset, err := bat.BlockOffsetVHD(br.bat, blockIndex)
	if err != nil {
		return 0, err
	}

	if fileOffset < 0 {
		// Unallocated block — return zeroes
		clear(dst[:want])
		return int(want), nil
	}

	// fileOffset already accounts for the sector bitmap; add offset within block
	physicalOffset := fileOffset + offsetInBlock
	n, err := br.reader.ReadAt(dst[:want], physicalOffset)
	return n, err
}

// ReadBlock reads the full data of the block at blockIndex (excluding sector bitmap).
func (br *VHDBlockReader) ReadBlock(blockIndex uint32) ([]byte, error) {
	fileOffset, err := bat.BlockOffsetVHD(br.bat, blockIndex)
	if err != nil {
		return nil, err
	}

	if fileOffset < 0 {
		// Return zeroed block
		return make([]byte, br.blockSize), nil
	}

	data := make([]byte, br.blockSize)
	_, err = br.reader.ReadAt(data, fileOffset)
	return data, err
}

// ReadSectorBitmap reads the raw sector bitmap for a given block.
func (br *VHDBlockReader) ReadSectorBitmap(blockIndex uint32) (*bat.SectorBitmap, error) {
	if blockIndex >= br.bat.NumberOfEntries {
		return nil, errors.New("block index out of range")
	}

	entry := br.bat.Entries[blockIndex]
	if !entry.IsAllocated {
		return nil, errors.New("block is not allocated")
	}

	// The sector bitmap sits immediately before the block data.
	// bat.BlockOffsetVHD already adds sectorBitmapSize, so subtract it.
	bitmapOffset := entry.FileOffset - int64(br.bat.SectorBitmapSize)
	return bat.ReadSectorBitmap(br.reader, bitmapOffset, br.bat.SectorBitmapSize)
}

// BlockCount returns the total number of blocks.
func (br *VHDBlockReader) BlockCount() uint32 { return br.bat.NumberOfEntries }

// BlockSize returns the size in bytes of each block.
func (br *VHDBlockReader) BlockSize() uint32 { return br.blockSize }

// ============================================================================
// VHDX Block Reader
// ============================================================================

// VHDXBlockReader reads block and sector data from VHDX disks.
type VHDXBlockReader struct {
	reader          io.ReaderAt
	bat             *types.VHDXBlockAllocationTable
	blockSize       uint32
	sectorSize      uint32
	sectorsPerBlock uint32
}

// NewVHDXBlockReader creates a VHDX block reader using a parsed BAT.
func NewVHDXBlockReader(r io.ReaderAt, b *types.VHDXBlockAllocationTable, sectorSize uint32) *VHDXBlockReader {
	if sectorSize == 0 {
		sectorSize = types.DefaultSectorSize
	}
	return &VHDXBlockReader{
		reader:          r,
		bat:             b,
		blockSize:       b.BlockSize,
		sectorSize:      sectorSize,
		sectorsPerBlock: b.BlockSize / sectorSize,
	}
}

// ReadAt implements io.ReaderAt — reads from the virtual disk address space.
func (br *VHDXBlockReader) ReadAt(p []byte, virtualOffset int64) (int, error) {
	total := 0
	for total < len(p) {
		n, err := br.readSegment(p[total:], virtualOffset+int64(total))
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			// No progress and no error would spin forever.
			return total, io.ErrUnexpectedEOF
		}
	}
	return total, nil
}

func (br *VHDXBlockReader) readSegment(dst []byte, virtualOffset int64) (int, error) {
	if br.blockSize == 0 {
		return 0, errors.New("libvhdi: VHDX block size is zero")
	}
	if br.sectorSize == 0 {
		return 0, errors.New("libvhdi: VHDX sector size is zero")
	}
	blockIndex := uint32(virtualOffset / int64(br.blockSize))
	offsetInBlock := virtualOffset % int64(br.blockSize)

	if blockIndex >= br.bat.NumberOfEntries {
		return 0, io.EOF
	}

	available := int64(br.blockSize) - offsetInBlock
	want := int64(len(dst))
	if want > available {
		want = available
	}

	entry := br.bat.Entries[blockIndex]

	if !entry.IsAllocated {
		// Unallocated / zero block — return zeroes
		clear(dst[:want])
		return int(want), nil
	}

	physicalOffset := entry.FileOffset + offsetInBlock

	if entry.BlockState == types.BlockStatePartiallyAllocated {
		// For partially-present blocks, the sector bitmap records which sectors
		// have been written. Unwritten sectors must be returned as zero.
		return br.readPartialBlock(dst[:want], entry.FileOffset, blockIndex, offsetInBlock)
	}

	n, err := br.reader.ReadAt(dst[:want], physicalOffset)
	return n, err
}

// readPartialBlock handles state-7 (partially present) VHDX blocks by consulting
// the sector bitmap. Sectors not set in the bitmap are returned as zeroes.
// blockFileOffset is the physical file offset of the start of the block data.
func (br *VHDXBlockReader) readPartialBlock(dst []byte, blockFileOffset int64, blockIndex uint32, offsetInBlock int64) (int, error) {
	chunkRatio := uint64(br.bat.ChunkRatio)
	if chunkRatio == 0 {
		return 0, errors.New("libvhdi: VHDX chunk ratio is zero")
	}
	chunkIndex := uint64(blockIndex) / chunkRatio
	blockInChunk := uint64(blockIndex) % chunkRatio
	sectorsPerBlock := uint64(br.blockSize) / uint64(br.sectorSize)

	// First sector within the chunk that belongs to this block.
	firstSectorInChunk := blockInChunk * sectorsPerBlock
	// First and last sector (exclusive) within the block we're reading.
	startSector := uint64(offsetInBlock) / uint64(br.sectorSize)
	endSector := (uint64(offsetInBlock) + uint64(len(dst)) + uint64(br.sectorSize) - 1) / uint64(br.sectorSize)

	// Absolute bit positions in the chunk bitmap.
	firstBit := firstSectorInChunk + startSector
	lastBit := firstSectorInChunk + endSector - 1

	// Locate the sector bitmap for this chunk.
	var bitmapOffset int64 = -1
	if int(chunkIndex) < len(br.bat.SectorBitmapOffsets) {
		bitmapOffset = br.bat.SectorBitmapOffsets[chunkIndex]
	}

	if bitmapOffset < 0 {
		// No bitmap present — treat entire region as zeroes.
		clear(dst)
		return len(dst), nil
	}

	// Read only the bytes of the bitmap we need.
	firstByte := firstBit / 8
	lastByte := lastBit / 8
	bitmapSliceLen := lastByte - firstByte + 1
	bitmapSlice := make([]byte, bitmapSliceLen)
	if _, err := br.reader.ReadAt(bitmapSlice, bitmapOffset+int64(firstByte)); err != nil {
		return 0, err
	}

	// Process sector by sector, interleaving bitmap-checked reads with zero fills.
	written := 0
	for s := startSector; s < endSector; s++ {
		sectorStart := int64(s)*int64(br.sectorSize) - offsetInBlock
		if sectorStart < 0 {
			sectorStart = 0
		}
		sectorEnd := int64(s+1)*int64(br.sectorSize) - offsetInBlock
		if sectorEnd > int64(len(dst)) {
			sectorEnd = int64(len(dst))
		}
		chunk := dst[sectorStart:sectorEnd]

		// Check bitmap bit for this sector.
		bitPos := firstSectorInChunk + s
		byteOff := bitPos/8 - firstByte
		bitOff := bitPos % 8
		allocated := (bitmapSlice[byteOff]>>bitOff)&1 == 1

		if allocated {
			if _, err := br.reader.ReadAt(chunk, blockFileOffset+int64(s)*int64(br.sectorSize)); err != nil {
				return written, err
			}
		} else {
			clear(chunk)
		}
		written += len(chunk)
	}

	return written, nil
}

// ReadBlock reads the full data of the block at blockIndex.
func (br *VHDXBlockReader) ReadBlock(blockIndex uint32) ([]byte, error) {
	fileOffset, err := bat.BlockOffsetVHDX(br.bat, blockIndex)
	if err != nil {
		return nil, err
	}

	if fileOffset < 0 {
		return make([]byte, br.blockSize), nil
	}

	data := make([]byte, br.blockSize)
	_, err = br.reader.ReadAt(data, fileOffset)
	return data, err
}

// BlockDescriptor returns the descriptor for a block including its state and sector ranges.
func (br *VHDXBlockReader) BlockDescriptor(blockIndex uint32) (*types.BlockDescriptor, error) {
	if blockIndex >= br.bat.NumberOfEntries {
		return nil, errors.New("block index out of range")
	}

	entry := br.bat.Entries[blockIndex]
	desc := &types.BlockDescriptor{
		FileOffset: entry.FileOffset,
		BlockState: entry.BlockState,
	}

	return desc, nil
}

// BlockCount returns the total number of blocks.
func (br *VHDXBlockReader) BlockCount() uint32 { return br.bat.NumberOfEntries }

// BlockSize returns the size in bytes of each block.
func (br *VHDXBlockReader) BlockSize() uint32 { return br.blockSize }
