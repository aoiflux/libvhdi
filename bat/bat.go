// Package bat provides Block Allocation Table (BAT) parsing and lookup logic.
package bat

// BAT represents a Block Allocation Table for tracking block locations.
type BAT interface {
	// BlockOffset returns the physical file offset for the given block index.
	// Returns -1 if the block is unallocated.
	BlockOffset(index uint32) (int64, error)

	// IsAllocated checks if a block is allocated.
	IsAllocated(index uint32) (bool, error)

	// BlockCount returns the total number of blocks in the BAT.
	BlockCount() uint32
}

// SectorRange represents a contiguous range of allocated sectors [Start, End).
type SectorRange struct {
	Start uint32
	End   uint32
}
