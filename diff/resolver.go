// Package diff implements differencing (child) disk resolution over a parent chain.
package diff

import (
	"errors"
	"io"

	"github.com/aoiflux/libvhdi/bat"
	"github.com/aoiflux/libvhdi/block"
	"github.com/aoiflux/libvhdi/types"
)

// Resolver resolves reads on a child (differencing) disk, falling through to the
// parent when a block is unallocated in the child.
type Resolver struct {
	child       io.ReaderAt
	parent      io.ReaderAt // nil if there is no parent
	childBAT    *resolverBAT
	blockSize   int64
	virtualSize int64
}

// resolverBAT is a minimal interface over either a VHD or VHDX BAT.
type resolverBAT struct {
	isVHD bool
	vhd   *types.VHDBlockAllocationTable
	vhdx  *types.VHDXBlockAllocationTable
}

func (rb *resolverBAT) blockCount() uint32 {
	if rb.isVHD {
		return bat.BlockCountVHD(rb.vhd)
	}
	return bat.BlockCountVHDX(rb.vhdx)
}

func (rb *resolverBAT) isAllocated(index uint32) (bool, error) {
	if rb.isVHD {
		return bat.IsAllocatedVHD(rb.vhd, index)
	}
	return bat.IsAllocatedVHDX(rb.vhdx, index)
}

// NewVHDResolver creates a differencing disk resolver for VHD format.
func NewVHDResolver(child io.ReaderAt, parent io.ReaderAt, childBAT *types.VHDBlockAllocationTable, blockSize uint32, virtualSize uint64) *Resolver {
	rb := &resolverBAT{isVHD: true, vhd: childBAT}
	return &Resolver{
		child:       child,
		parent:      parent,
		childBAT:    rb,
		blockSize:   int64(blockSize),
		virtualSize: int64(virtualSize),
	}
}

// NewVHDXResolver creates a differencing disk resolver for VHDX format.
func NewVHDXResolver(child io.ReaderAt, parent io.ReaderAt, childBAT *types.VHDXBlockAllocationTable, blockSize uint32, virtualSize uint64) *Resolver {
	rb := &resolverBAT{isVHD: false, vhdx: childBAT}
	return &Resolver{
		child:       child,
		parent:      parent,
		childBAT:    rb,
		blockSize:   int64(blockSize),
		virtualSize: int64(virtualSize),
	}
}

// ReadAt implements io.ReaderAt. Reads are first attempted in the child disk;
// unallocated blocks are transparently satisfied by the parent disk.
func (r *Resolver) ReadAt(p []byte, virtualOffset int64) (int, error) {
	if virtualOffset >= r.virtualSize {
		return 0, io.EOF
	}

	total := 0
	for total < len(p) {
		n, err := r.readSegment(p[total:], virtualOffset+int64(total))
		total += n
		if err != nil {
			return total, err
		}
		if virtualOffset+int64(total) >= r.virtualSize {
			return total, io.EOF
		}
	}
	return total, nil
}

// readSegment handles one contiguous region (within a single block).
func (r *Resolver) readSegment(dst []byte, virtualOffset int64) (int, error) {
	blockIndex := uint32(virtualOffset / r.blockSize)
	offsetInBlock := virtualOffset % r.blockSize

	if blockIndex >= r.childBAT.blockCount() {
		return 0, io.EOF
	}

	// Bound the read to the current block
	available := r.blockSize - offsetInBlock
	want := int64(len(dst))
	if want > available {
		want = available
	}

	allocated, err := r.childBAT.isAllocated(blockIndex)
	if err != nil {
		return 0, err
	}

	var src io.ReaderAt
	if allocated {
		src = r.child
	} else {
		if r.parent == nil {
			// No parent — return zeroes (empty/unallocated)
			for i := int64(0); i < want; i++ {
				dst[i] = 0
			}
			return int(want), nil
		}
		src = r.parent
	}

	n, err := src.ReadAt(dst[:want], virtualOffset)
	return n, err
}

// VirtualSize returns the total size of the differencing disk in bytes.
func (r *Resolver) VirtualSize() int64 { return r.virtualSize }

// HasParent reports whether this resolver has a parent disk.
func (r *Resolver) HasParent() bool { return r.parent != nil }

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

// BuildVHDResolver constructs a layered Resolver for VHD differencing disk chains.
// bats[i] is the BAT for links[i]; blockSize and virtualSize are from the child metadata.
func BuildVHDResolver(links []io.ReaderAt, bats []*types.VHDBlockAllocationTable, blockSize uint32, virtualSize uint64) (*Resolver, error) {
	if len(links) == 0 {
		return nil, errors.New("chain must have at least one link")
	}
	if len(links) != len(bats) {
		return nil, errors.New("links and bats slices must have equal length")
	}

	// Build from the deepest ancestor upward.
	// The oldest ancestor has no parent (parent = nil).
	blockReaders := make([]io.ReaderAt, len(links))
	for i := range links {
		var childBAT *types.VHDBlockAllocationTable
		if bats[i] != nil {
			childBAT = bats[i]
		}
		br := block.NewVHDBlockReader(links[i], childBAT, types.DefaultSectorSize)
		blockReaders[i] = br
	}

	// Innermost resolver (oldest ancestor with no parent)
	var current io.ReaderAt = blockReaders[len(links)-1]
	for i := len(links) - 2; i >= 0; i-- {
		current = NewVHDResolver(blockReaders[i], current, bats[i], blockSize, virtualSize)
	}

	// The result is the child-most resolver
	return current.(*Resolver), nil
}
