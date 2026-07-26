package diff

import (
	"errors"
	"io"

	"github.com/aoiflux/libvhdi/block"
	"github.com/aoiflux/libvhdi/types"
)

// ============================================================================
// Parent chain builder helpers
// ============================================================================

// Chain represents an ordered slice of disk readers forming a parent chain,
// from child (index 0) to oldest ancestor (last index).
type Chain struct {
	links []io.ReaderAt
}

// NewChain creates a new chain with the given child reader.
func NewChain(child io.ReaderAt) *Chain {
	return &Chain{links: []io.ReaderAt{child}}
}

// AddParent appends a parent reader to the end of the chain.
func (c *Chain) AddParent(parent io.ReaderAt) error {
	if parent == nil {
		return errors.New("parent reader must not be nil")
	}
	c.links = append(c.links, parent)
	return nil
}

// Depth returns the number of disks in the chain (child + all parents).
func (c *Chain) Depth() int { return len(c.links) }

// Reader returns the reader at the given position in the chain (0 = child).
func (c *Chain) Reader(index int) (io.ReaderAt, error) {
	if index < 0 || index >= len(c.links) {
		return nil, errors.New("chain index out of range")
	}
	return c.links[index], nil
}

// BuildVHDResolver constructs a layered Resolver for VHD differencing disk
// chains. links[i] is the raw backing reader for disk i and bats[i] its BAT;
// blockSize and virtualSize come from the child's metadata.
//
// Each layer resolves at sector granularity: sectors absent from a child's
// block bitmap are served by the next disk in the chain.
func BuildVHDResolver(links []io.ReaderAt, bats []*types.VHDBlockAllocationTable, blockSize uint32, virtualSize uint64) (*Resolver, error) {
	if len(links) == 0 {
		return nil, errors.New("chain must have at least one link")
	}
	if len(links) != len(bats) {
		return nil, errors.New("links and bats slices must have equal length")
	}
	for i := range bats {
		if bats[i] == nil {
			return nil, errors.New("chain BAT entries must not be nil")
		}
	}

	// The oldest ancestor is a plain block reader with no parent behind it.
	last := len(links) - 1
	var current io.ReaderAt = block.NewVHDBlockReader(links[last], bats[last], types.DefaultSectorSize)

	// Layer each younger disk on top, newest resolved last.
	for i := last - 1; i >= 0; i-- {
		r, err := New(Config{
			Source:      links[i],
			Child:       block.NewVHDBlockReader(links[i], bats[i], types.DefaultSectorSize),
			Parent:      current,
			BlockSize:   blockSize,
			VirtualSize: virtualSize,
			VHDBAT:      bats[i],
		})
		if err != nil {
			return nil, err
		}
		current = r
	}

	resolver, ok := current.(*Resolver)
	if !ok {
		return nil, errors.New("chain must contain at least one differencing disk")
	}
	return resolver, nil
}
