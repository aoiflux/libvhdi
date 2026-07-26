// SPDX-License-Identifier: MIT

// Package block provides block-level reading and decompression logic for virtual disks.
package block

// Block represents a single block in a virtual disk.
type Block struct {
	Index  uint32 // Block index
	Offset int64  // Physical offset in file
	Size   uint32 // Block size in bytes
	State  uint32 // Block state
}

// BlockReader provides interface for reading blocks from virtual disks.
type BlockReader interface {
	// ReadBlock reads a single block by index.
	ReadBlock(index uint32) ([]byte, error)

	// ReadSector reads a single sector (512 bytes) at the given index.
	ReadSector(index uint32) ([]byte, error)

	// BlockCount returns the total number of blocks.
	BlockCount() uint32

	// BlockSize returns the size of each block in bytes.
	BlockSize() uint32
}
