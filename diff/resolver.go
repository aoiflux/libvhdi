// SPDX-License-Identifier: MIT

// Package diff implements differencing (child) disk resolution over a parent chain.
package diff

import (
	"errors"
	"io"

	"github.com/aoiflux/libvhdi/bat"
	"github.com/aoiflux/libvhdi/types"
)

// ErrParentRequired is returned when a read resolves to a parent disk but no
// parent has been attached. Differencing disks fail closed: returning zeroes
// would be indistinguishable from genuine disk contents.
var ErrParentRequired = errors.New("libvhdi: differencing disk requires a parent")

// ErrNoBitmapSource is returned when a block can only be resolved by consulting
// a sector bitmap but the resolver was built without access to the child's
// backing file. Resolvers created through the deprecated NewVHDResolver and
// NewVHDXResolver constructors are in this state.
var ErrNoBitmapSource = errors.New("libvhdi: sector bitmap resolution requires the child's backing reader")

// disposition describes where the bytes of a block live.
type disposition int

const (
	// dispChild: the whole block is present in the child.
	dispChild disposition = iota
	// dispParent: the whole block must be read from the parent.
	dispParent
	// dispZero: the block reads as zeroes and has no parent backing.
	dispZero
	// dispPerSector: the block's sector bitmap decides, sector by sector,
	// which bytes come from the child and which from the parent.
	dispPerSector
)

// Config describes a differencing disk and its resolution inputs.
type Config struct {
	// Source is the child's raw backing reader. It is required to read sector
	// bitmaps; without it, blocks needing per-sector resolution error out.
	Source io.ReaderAt

	// Child is the decoded child block reader (virtual address space).
	Child io.ReaderAt

	// Parent is the next disk in the chain, or nil if none is attached yet.
	// A nil Parent makes parent-backed reads return ErrParentRequired.
	Parent io.ReaderAt

	BlockSize   uint32
	SectorSize  uint32
	VirtualSize uint64

	// Exactly one of VHDBAT / VHDXBAT must be set.
	VHDBAT  *types.VHDBlockAllocationTable
	VHDXBAT *types.VHDXBlockAllocationTable
}

// Resolver resolves reads on a child (differencing) disk against a parent
// chain, at sector granularity.
//
// Both VHD and VHDX represent partially-written blocks with a sector bitmap:
// sectors whose bit is set live in the child, and sectors whose bit is clear
// must be served from the parent. Resolving at whole-block granularity returns
// the child's zero-filled holes instead of the parent's data.
type Resolver struct {
	src    io.ReaderAt
	child  io.ReaderAt
	parent io.ReaderAt

	blockSize   int64
	sectorSize  int64
	virtualSize int64

	vhd  *types.VHDBlockAllocationTable
	vhdx *types.VHDXBlockAllocationTable
}

// New creates a differencing disk resolver.
func New(cfg Config) (*Resolver, error) {
	if cfg.Child == nil {
		return nil, errors.New("libvhdi: child reader must not be nil")
	}
	if (cfg.VHDBAT == nil) == (cfg.VHDXBAT == nil) {
		return nil, errors.New("libvhdi: exactly one of VHDBAT or VHDXBAT must be set")
	}
	if cfg.BlockSize == 0 {
		return nil, errors.New("libvhdi: block size must be non-zero")
	}

	sectorSize := int64(cfg.SectorSize)
	if cfg.VHDBAT != nil {
		// The VHD block bitmap is defined as one bit per 512-byte sector,
		// independent of any reported logical sector size.
		sectorSize = types.DefaultSectorSize
	}
	if sectorSize <= 0 {
		sectorSize = types.DefaultSectorSize
	}

	return &Resolver{
		src:         cfg.Source,
		child:       cfg.Child,
		parent:      cfg.Parent,
		blockSize:   int64(cfg.BlockSize),
		sectorSize:  sectorSize,
		virtualSize: int64(cfg.VirtualSize),
		vhd:         cfg.VHDBAT,
		vhdx:        cfg.VHDXBAT,
	}, nil
}

// NewVHDResolver creates a differencing disk resolver for VHD format.
//
// Deprecated: this constructor cannot resolve partially-written blocks because
// it has no access to the child's backing reader, and so cannot read sector
// bitmaps. Such blocks return ErrNoBitmapSource rather than silently dropping
// parent data. Use New with a populated Config.Source instead.
func NewVHDResolver(child io.ReaderAt, parent io.ReaderAt, childBAT *types.VHDBlockAllocationTable, blockSize uint32, virtualSize uint64) *Resolver {
	r, err := New(Config{
		Child:       child,
		Parent:      parent,
		BlockSize:   blockSize,
		VirtualSize: virtualSize,
		VHDBAT:      childBAT,
	})
	if err != nil {
		return &Resolver{child: child, parent: parent, vhd: childBAT}
	}
	return r
}

// NewVHDXResolver creates a differencing disk resolver for VHDX format.
//
// Deprecated: this constructor cannot resolve partially-present blocks because
// it has no access to the child's backing reader, and so cannot read sector
// bitmaps. Such blocks return ErrNoBitmapSource rather than silently dropping
// parent data. Use New with a populated Config.Source instead.
func NewVHDXResolver(child io.ReaderAt, parent io.ReaderAt, childBAT *types.VHDXBlockAllocationTable, blockSize uint32, virtualSize uint64) *Resolver {
	r, err := New(Config{
		Child:       child,
		Parent:      parent,
		BlockSize:   blockSize,
		VirtualSize: virtualSize,
		VHDXBAT:     childBAT,
	})
	if err != nil {
		return &Resolver{child: child, parent: parent, vhdx: childBAT}
	}
	return r
}

// ReadAt implements io.ReaderAt over the child's virtual address space,
// resolving each region to the child, the parent chain, or zeroes.
func (r *Resolver) ReadAt(p []byte, virtualOffset int64) (int, error) {
	if virtualOffset < 0 {
		return 0, errors.New("libvhdi: negative offset")
	}
	if virtualOffset >= r.virtualSize {
		return 0, io.EOF
	}

	// Clamp to the end of the virtual disk. A caller asking for more than the
	// device holds gets the available bytes plus io.EOF, per io.ReaderAt.
	clamped := false
	if rem := r.virtualSize - virtualOffset; int64(len(p)) > rem {
		p = p[:rem]
		clamped = true
	}

	total := 0
	for total < len(p) {
		n, err := r.readSegment(p[total:], virtualOffset+int64(total))
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrUnexpectedEOF
		}
	}
	if clamped {
		return total, io.EOF
	}
	return total, nil
}

// readSegment resolves one contiguous region within a single block.
func (r *Resolver) readSegment(dst []byte, virtualOffset int64) (int, error) {
	blockIndex := uint32(virtualOffset / r.blockSize)
	offsetInBlock := virtualOffset % r.blockSize

	if blockIndex >= r.blockCount() {
		return 0, io.EOF
	}

	// Bound the read to the current block.
	want := int64(len(dst))
	if available := r.blockSize - offsetInBlock; want > available {
		want = available
	}
	dst = dst[:want]

	disp, err := r.classify(blockIndex)
	if err != nil {
		return 0, err
	}

	switch disp {
	case dispChild:
		return r.child.ReadAt(dst, virtualOffset)
	case dispParent:
		return r.readParent(dst, virtualOffset)
	case dispZero:
		clear(dst)
		return len(dst), nil
	default:
		return r.readPerSector(dst, blockIndex, offsetInBlock, virtualOffset)
	}
}

// readPerSector walks the block's sector bitmap, reading runs of sectors from
// whichever disk backs them. Sectors present in the child come from the child;
// absent sectors come from the parent.
func (r *Resolver) readPerSector(dst []byte, blockIndex uint32, offsetInBlock, virtualOffset int64) (int, error) {
	firstSector := offsetInBlock / r.sectorSize
	lastSector := (offsetInBlock + int64(len(dst)) - 1) / r.sectorSize

	// Read the bitmap bytes covering this sector range once. Testing each
	// sector with its own one-byte read turned a single block read into
	// thousands of I/O operations.
	window, err := r.loadBitmap(blockIndex, firstSector, lastSector)
	if err != nil {
		return 0, err
	}

	written := 0
	for s := firstSector; s <= lastSector; {
		present := window.present(r.bitPos(blockIndex, s))

		// Extend the run while the backing disk stays the same, so a contiguous
		// stretch is served by one read rather than one read per sector.
		run := s + 1
		for run <= lastSector && window.present(r.bitPos(blockIndex, run)) == present {
			run++
		}

		// Translate the sector run into a byte range within dst.
		start := s*r.sectorSize - offsetInBlock
		if start < 0 {
			start = 0
		}
		end := run*r.sectorSize - offsetInBlock
		if end > int64(len(dst)) {
			end = int64(len(dst))
		}

		chunk := dst[start:end]
		at := virtualOffset + start

		var n int
		if present {
			n, err = r.child.ReadAt(chunk, at)
		} else {
			n, err = r.readParent(chunk, at)
		}
		written += n
		if err != nil {
			return written, err
		}

		s = run
	}

	return written, nil
}

// ============================================================================
// Classification, for extent mapping
// ============================================================================

// BlockKind describes where a block's bytes live.
type BlockKind int

const (
	// BlockFromChild: the whole block is present in this disk.
	BlockFromChild BlockKind = iota

	// BlockFromParent: the whole block must be read from the parent chain.
	BlockFromParent

	// BlockZero: the block reads as zeroes and is not parent-backed.
	BlockZero

	// BlockPerSector: the block's sector bitmap decides sector by sector, so
	// SectorRuns is needed to resolve it.
	BlockPerSector
)

// BlockKind reports where the given block's bytes live, without reading them.
func (r *Resolver) BlockKind(blockIndex uint32) (BlockKind, error) {
	disp, err := r.classify(blockIndex)
	if err != nil {
		return BlockZero, err
	}
	switch disp {
	case dispChild:
		return BlockFromChild, nil
	case dispParent:
		return BlockFromParent, nil
	case dispPerSector:
		return BlockPerSector, nil
	default:
		return BlockZero, nil
	}
}

// SectorRun is a contiguous run of sectors within one block that share a
// backing. Sector indices are inclusive and relative to the block.
type SectorRun struct {
	First, Last int64

	// InChild is true when the run is present in this disk, false when it must
	// come from the parent chain.
	InChild bool
}

// SectorRuns groups the sectors in [firstSector, lastSector] of a
// BlockPerSector block into runs sharing a backing.
//
// The bitmap bytes covering the range are read once, so this costs one I/O
// regardless of how many sectors the range spans.
func (r *Resolver) SectorRuns(blockIndex uint32, firstSector, lastSector int64) ([]SectorRun, error) {
	if lastSector < firstSector {
		return nil, errors.New("libvhdi: empty sector range")
	}

	window, err := r.loadBitmap(blockIndex, firstSector, lastSector)
	if err != nil {
		return nil, err
	}

	var runs []SectorRun
	for s := firstSector; s <= lastSector; {
		present := window.present(r.bitPos(blockIndex, s))
		run := s + 1
		for run <= lastSector && window.present(r.bitPos(blockIndex, run)) == present {
			run++
		}
		runs = append(runs, SectorRun{First: s, Last: run - 1, InChild: present})
		s = run
	}
	return runs, nil
}

// BlockCount returns the number of blocks the child's BAT describes.
func (r *Resolver) BlockCount() uint32 { return r.blockCount() }

// ============================================================================
// Sector bitmaps
// ============================================================================

// bitmapWindow holds the bytes of a sector bitmap covering some range of
// sectors, so a range of sectors can be tested without further I/O.
//
// The two formats disagree on bit order: a VHD block bitmap is
// most-significant-bit first, while a VHDX sector bitmap is
// least-significant-bit first.
type bitmapWindow struct {
	bytes     []byte
	firstByte int64
	msbFirst  bool

	// absent marks a block with no bitmap at all, where nothing is present in
	// the child.
	absent bool
}

func (w *bitmapWindow) present(bitPos int64) bool {
	if w.absent || bitPos < 0 {
		return false
	}
	i := bitPos/8 - w.firstByte
	if i < 0 || i >= int64(len(w.bytes)) {
		return false
	}
	b := w.bytes[i]
	if w.msbFirst {
		return (b>>(7-uint(bitPos%8)))&1 == 1
	}
	return (b>>uint(bitPos%8))&1 == 1
}

// bitPos maps a sector within a block to its bit position in the relevant
// bitmap. VHD keeps one bitmap per block, so the position is just the sector
// index; VHDX keeps one per chunk of blocks, so the block's offset within its
// chunk contributes too.
func (r *Resolver) bitPos(blockIndex uint32, sectorInBlock int64) int64 {
	if r.vhd != nil {
		return sectorInBlock
	}
	chunkRatio := int64(r.vhdx.ChunkRatio)
	if chunkRatio <= 0 {
		return -1
	}
	sectorsPerBlock := r.blockSize / r.sectorSize
	return (int64(blockIndex)%chunkRatio)*sectorsPerBlock + sectorInBlock
}

// loadBitmap reads the bitmap bytes covering [firstSector, lastSector] of a
// block.
func (r *Resolver) loadBitmap(blockIndex uint32, firstSector, lastSector int64) (*bitmapWindow, error) {
	if r.src == nil {
		return nil, ErrNoBitmapSource
	}

	bitmapOffset, msbFirst, err := r.bitmapLocation(blockIndex)
	if err != nil {
		return nil, err
	}
	if bitmapOffset < 0 {
		return &bitmapWindow{absent: true}, nil
	}

	firstBit := r.bitPos(blockIndex, firstSector)
	lastBit := r.bitPos(blockIndex, lastSector)
	if firstBit < 0 || lastBit < firstBit {
		return nil, errors.New("libvhdi: invalid sector bitmap range")
	}

	firstByte := firstBit / 8
	byteCount := lastBit/8 - firstByte + 1
	if r.vhd != nil && firstByte+byteCount > int64(r.vhd.SectorBitmapSize) {
		return nil, errors.New("libvhdi: VHD sector range extends beyond its block bitmap")
	}

	buf := make([]byte, byteCount)
	if _, err := r.src.ReadAt(buf, bitmapOffset+firstByte); err != nil {
		return nil, err
	}

	return &bitmapWindow{bytes: buf, firstByte: firstByte, msbFirst: msbFirst}, nil
}

// bitmapLocation returns the file offset of the bitmap backing a block, or -1
// when the block has none.
func (r *Resolver) bitmapLocation(blockIndex uint32) (offset int64, msbFirst bool, err error) {
	if r.vhd != nil {
		if blockIndex >= r.vhd.NumberOfEntries {
			return 0, false, errors.New("libvhdi: block index out of range")
		}
		// The VHD block bitmap sits immediately before the block data, and
		// bat.BlockOffsetVHD already skips past it.
		entry := r.vhd.Entries[blockIndex]
		bitmapOffset := entry.FileOffset - int64(r.vhd.SectorBitmapSize)
		if bitmapOffset < 0 {
			return 0, false, errors.New("libvhdi: VHD sector bitmap offset is negative")
		}
		return bitmapOffset, true, nil
	}

	chunkRatio := int64(r.vhdx.ChunkRatio)
	if chunkRatio <= 0 {
		return 0, false, errors.New("libvhdi: invalid VHDX chunk ratio")
	}
	chunkIndex := int64(blockIndex) / chunkRatio
	if chunkIndex >= int64(len(r.vhdx.SectorBitmapOffsets)) {
		return -1, false, nil
	}
	return r.vhdx.SectorBitmapOffsets[chunkIndex], false, nil
}

// readParent serves a range from the parent chain, failing closed when no
// parent is attached.
func (r *Resolver) readParent(dst []byte, virtualOffset int64) (int, error) {
	if r.parent == nil {
		return 0, ErrParentRequired
	}
	return r.parent.ReadAt(dst, virtualOffset)
}

// classify decides where a block's bytes live.
func (r *Resolver) classify(blockIndex uint32) (disposition, error) {
	if r.vhd != nil {
		allocated, err := bat.IsAllocatedVHD(r.vhd, blockIndex)
		if err != nil {
			return dispZero, err
		}
		if !allocated {
			// No block in the child: the parent owns this range entirely.
			return dispParent, nil
		}
		// An allocated block in a differencing VHD is still sector-sparse.
		return dispPerSector, nil
	}

	if blockIndex >= r.vhdx.NumberOfEntries {
		return dispZero, errors.New("libvhdi: block index out of range")
	}

	switch r.vhdx.Entries[blockIndex].BlockState {
	case types.BlockStateFullyAllocated:
		return dispChild, nil
	case types.BlockStatePartiallyAllocated:
		// PAYLOAD_BLOCK_PARTIALLY_PRESENT occurs only in differencing files:
		// bits clear in the sector bitmap must be read from the parent.
		return dispPerSector, nil
	case types.BlockStateNone:
		// PAYLOAD_BLOCK_NOT_PRESENT: the parent owns this range.
		return dispParent, nil
	default:
		// UNDEFINED, ZERO and UNMAPPED all read as zeroes and are not
		// parent-backed.
		return dispZero, nil
	}
}

func (r *Resolver) blockCount() uint32 {
	if r.vhd != nil {
		return bat.BlockCountVHD(r.vhd)
	}
	return bat.BlockCountVHDX(r.vhdx)
}

// VirtualSize returns the total size of the differencing disk in bytes.
func (r *Resolver) VirtualSize() int64 { return r.virtualSize }

// HasParent reports whether this resolver has a parent disk attached.
func (r *Resolver) HasParent() bool { return r.parent != nil }

// SetParent attaches or replaces the parent disk.
func (r *Resolver) SetParent(parent io.ReaderAt) { r.parent = parent }
