// SPDX-License-Identifier: MIT

package reader

import (
	"errors"
	"fmt"
	"io"

	"github.com/aoiflux/libvhdi/diff"
	"github.com/aoiflux/libvhdi/types"
)

// ExtentKind describes what backs a range of the virtual disk.
type ExtentKind int

const (
	// ExtentZero: the range has no backing bytes anywhere in the chain and reads
	// as zeroes. An acquisition tool can skip it rather than write zeroes.
	ExtentZero ExtentKind = iota

	// ExtentMapped: the range is backed by a contiguous run of bytes in one of
	// the files in the chain, at FileOffset.
	ExtentMapped

	// ExtentUnresolved: the range resolves to a parent that is not attached, so
	// its contents are unknown. Reading it returns ErrParentRequired.
	ExtentUnresolved

	// ExtentZeroedByChild: a differencing disk explicitly recorded this range as
	// zero, overriding whatever the parent held there.
	//
	// It reads as zeroes exactly like ExtentZero, and the distinction exists for
	// one reason: this is a *write*. The child did something here. ExtentZero
	// means the range was never written anywhere in the chain, which is a
	// different fact and, for change tracking, the opposite one -- a deletion
	// that clears a region records PAYLOAD_BLOCK_ZERO or PAYLOAD_BLOCK_UNMAPPED,
	// and without this kind it is indistinguishable from "the parent was already
	// zero". Change detection would silently under-report deletions.
	//
	// This kind is VHDX-only, and the reason is a limitation of VHD rather than
	// of this library. A VHD differencing disk has no way to say "explicitly
	// zero": its per-block sector bitmap distinguishes only "this sector is
	// mine" from "read this sector from the parent", and there is no block state
	// meaning zero. A region cleared on a VHD chain is therefore recorded as
	// belonging to the parent and reads back as the parent's old contents, not
	// as zeroes -- so on VHD the clearing is not merely invisible to change
	// tracking, it did not happen at the format level. Callers doing
	// deletion-aware change detection need VHDX.
	ExtentZeroedByChild
)

// ReadsAsZero reports whether a range of this kind returns zeroes when read.
//
// Both ExtentZero and ExtentZeroedByChild do. They differ in what they say
// about who put the zeroes there, not in what a read returns.
func (k ExtentKind) ReadsAsZero() bool {
	return k == ExtentZero || k == ExtentZeroedByChild
}

// IsWrite reports whether a disk in the chain actively wrote this range,
// whether with data or with zeroes.
func (k ExtentKind) IsWrite() bool {
	return k == ExtentMapped || k == ExtentZeroedByChild
}

// String implements fmt.Stringer.
func (k ExtentKind) String() string {
	switch k {
	case ExtentZero:
		return "zero"
	case ExtentMapped:
		return "mapped"
	case ExtentUnresolved:
		return "unresolved"
	case ExtentZeroedByChild:
		return "zeroed-by-child"
	default:
		return fmt.Sprintf("ExtentKind(%d)", int(k))
	}
}

// Extent describes a contiguous run of the virtual disk address space and what
// backs it.
type Extent struct {
	// VirtualOffset and Length bound the run in the virtual disk's address
	// space.
	VirtualOffset int64
	Length        int64

	// Kind says what backs the run.
	Kind ExtentKind

	// FileOffset is the byte offset of the run within the backing file. Valid
	// only when Kind is ExtentMapped.
	FileOffset int64

	// ChainIndex identifies the backing disk: 0 is the disk Extents was called
	// on, 1 its parent, and so on. Meaningful when Kind is ExtentMapped.
	ChainIndex int

	// Path is the backing disk's file path when known, for provenance.
	Path string
}

// End returns the first virtual offset past this extent.
func (e Extent) End() int64 { return e.VirtualOffset + e.Length }

// Extents maps a range of the virtual disk to the runs that back it.
//
// This is the sparse-aware view of the disk: an acquisition or carving tool can
// walk the extents, read only what is mapped, and skip zero runs instead of
// reading and writing megabytes of zeroes. For a differencing chain each mapped
// extent also identifies which disk in the chain supplies it, which
// VirtualToFileOffset cannot express.
//
// Adjacent runs are merged when they share a kind and, for mapped runs, continue
// contiguously in the same file. The returned extents tile the requested range
// exactly, clamped to the disk size, and are ordered by VirtualOffset.
//
// A range resolving to a parent that has not been attached yields
// ExtentUnresolved rather than an error, so a caller can discover which parts of
// a chain it is missing.
func (d *VirtualDisk) Extents(virtualOffset, length int64) ([]Extent, error) {
	if virtualOffset < 0 {
		return nil, errors.New("virtual offset must be >= 0")
	}
	if length < 0 {
		return nil, errors.New("length must be >= 0")
	}
	if uint64(virtualOffset) >= d.virtualSize {
		return nil, io.EOF
	}
	if length == 0 {
		return nil, nil
	}

	// Clamp to the end of the device.
	if remaining := int64(d.virtualSize) - virtualOffset; length > remaining {
		length = remaining
	}

	raw, err := d.rawExtents(virtualOffset, length, 0)
	if err != nil {
		return nil, err
	}
	return mergeExtents(raw), nil
}

// AllExtents maps the whole disk. It is shorthand for Extents(0, Size()).
func (d *VirtualDisk) AllExtents() ([]Extent, error) {
	if d.virtualSize == 0 {
		return nil, nil
	}
	return d.Extents(0, int64(d.virtualSize))
}

// MappedBytes returns how many bytes of the disk are backed by real data,
// which for a sparse image is what an acquisition actually has to read.
func (d *VirtualDisk) MappedBytes() (int64, error) {
	extents, err := d.AllExtents()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range extents {
		if e.Kind == ExtentMapped {
			total += e.Length
		}
	}
	return total, nil
}

// rawExtents produces unmerged extents for a range, recursing into the parent
// chain where the child does not supply the bytes.
//
// chainIndex is this disk's position in the chain being walked.
func (d *VirtualDisk) rawExtents(virtualOffset, length int64, chainIndex int) ([]Extent, error) {
	// A fixed VHD maps the whole device directly onto the front of the file.
	if d.format == types.FileFormatVHD && d.diskType == types.DiskTypeFixed {
		return []Extent{{
			VirtualOffset: virtualOffset,
			Length:        length,
			Kind:          ExtentMapped,
			FileOffset:    virtualOffset,
			ChainIndex:    chainIndex,
			Path:          d.path,
		}}, nil
	}

	blockSize := int64(d.blockSize)
	if blockSize <= 0 {
		return nil, errors.New("libvhdi: block size is zero")
	}

	var out []Extent
	end := virtualOffset + length

	for at := virtualOffset; at < end; {
		blockIndex := uint32(at / blockSize)
		offsetInBlock := at % blockSize

		// Bound this step to the end of the block or the requested range.
		span := blockSize - offsetInBlock
		if at+span > end {
			span = end - at
		}

		var (
			sub []Extent
			err error
		)
		if d.resolver != nil {
			sub, err = d.differencingExtents(blockIndex, at, offsetInBlock, span, chainIndex)
		} else {
			sub, err = d.plainBlockExtent(blockIndex, at, offsetInBlock, span, chainIndex)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)

		at += span
	}

	return out, nil
}

// plainBlockExtent classifies one block of a fixed, dynamic or non-differencing
// VHDX disk.
func (d *VirtualDisk) plainBlockExtent(blockIndex uint32, at, offsetInBlock, span int64, chainIndex int) ([]Extent, error) {
	// A PARTIALLY_PRESENT VHDX block is written per sector, so a single verdict
	// for the whole span describes only its first sector. The differencing path
	// already splits per sector via the resolver; this one has to do it too, or
	// the unwritten sectors of a partly-written block are reported as mapped.
	if d.format == types.FileFormatVHDX && d.childBATvhdx != nil &&
		blockIndex < d.childBATvhdx.NumberOfEntries &&
		d.childBATvhdx.Entries[blockIndex].BlockState == types.BlockStatePartiallyAllocated {
		return d.partialBlockExtents(blockIndex, at, offsetInBlock, span, chainIndex)
	}

	fileOffset, mapped, err := d.blockFileOffset(blockIndex, offsetInBlock)
	if err != nil {
		return nil, err
	}

	e := Extent{
		VirtualOffset: at,
		Length:        span,
		ChainIndex:    chainIndex,
		Path:          d.path,
	}
	if mapped {
		e.Kind = ExtentMapped
		e.FileOffset = fileOffset
	} else {
		e.Kind = ExtentZero
	}
	return []Extent{e}, nil
}

// partialBlockExtents classifies a span of a PARTIALLY_PRESENT VHDX block one
// sector at a time, so written and unwritten sectors are reported separately.
//
// Adjacent runs of the same kind are coalesced by the caller, so a block whose
// sectors are all present still yields a single extent. This walks sector by
// sector rather than reading the bitmap once, which is acceptable because the
// specification confines state 7 to differencing files -- and a differencing
// disk takes the resolver path instead, which loads the bitmap window once.
// Reaching this code at all means the image is unusual.
func (d *VirtualDisk) partialBlockExtents(blockIndex uint32, at, offsetInBlock, span int64, chainIndex int) ([]Extent, error) {
	sectorSize := int64(d.sectorSize)
	if sectorSize <= 0 {
		return nil, errors.New("libvhdi: sector size is zero")
	}

	var out []Extent
	for pos := int64(0); pos < span; {
		off := offsetInBlock + pos

		// Bound this step to the end of the sector it starts in.
		step := sectorSize - (off % sectorSize)
		if pos+step > span {
			step = span - pos
		}

		fileOffset, mapped, err := d.blockFileOffset(blockIndex, off)
		if err != nil {
			return nil, err
		}

		e := Extent{
			VirtualOffset: at + pos,
			Length:        step,
			ChainIndex:    chainIndex,
			Path:          d.path,
		}
		if mapped {
			e.Kind = ExtentMapped
			e.FileOffset = fileOffset
		} else {
			// This disk is not differencing, so there is nothing beneath it to
			// override: an unwritten sector is a genuine hole rather than a
			// deliberate zero, and ExtentZero is the honest kind.
			e.Kind = ExtentZero
		}
		out = append(out, e)

		pos += step
	}
	return out, nil
}

// differencingExtents classifies one block of a differencing disk, descending
// into the parent chain for the parts this disk does not supply.
func (d *VirtualDisk) differencingExtents(blockIndex uint32, at, offsetInBlock, span int64, chainIndex int) ([]Extent, error) {
	kind, err := d.resolver.BlockKind(blockIndex)
	if err != nil {
		return nil, err
	}

	switch kind {
	case diff.BlockZero:
		// The child recorded this block as zero or unmapped, which on a
		// differencing disk is an override rather than an absence: the parent
		// may well hold data here and the child has said to ignore it. That is
		// a write, and reporting it as a plain hole would lose the only record
		// that the region was cleared.
		return []Extent{{
			VirtualOffset: at,
			Length:        span,
			Kind:          ExtentZeroedByChild,
			ChainIndex:    chainIndex,
			Path:          d.path,
		}}, nil

	case diff.BlockFromChild:
		fileOffset, mapped, err := d.blockFileOffset(blockIndex, offsetInBlock)
		if err != nil {
			return nil, err
		}
		if !mapped {
			return d.parentExtents(at, span, chainIndex)
		}
		return []Extent{{
			VirtualOffset: at,
			Length:        span,
			Kind:          ExtentMapped,
			FileOffset:    fileOffset,
			ChainIndex:    chainIndex,
			Path:          d.path,
		}}, nil

	case diff.BlockFromParent:
		return d.parentExtents(at, span, chainIndex)
	}

	// BlockPerSector: the sector bitmap decides, so split the range into runs
	// and resolve each against this disk or the parent.
	sectorSize := int64(d.sectorSize)
	if d.format == types.FileFormatVHD {
		// A VHD block bitmap always describes 512-byte sectors.
		sectorSize = types.DefaultSectorSize
	}
	if sectorSize <= 0 {
		return nil, errors.New("libvhdi: sector size is zero")
	}

	firstSector := offsetInBlock / sectorSize
	lastSector := (offsetInBlock + span - 1) / sectorSize

	runs, err := d.resolver.SectorRuns(blockIndex, firstSector, lastSector)
	if err != nil {
		return nil, err
	}

	blockStart := at - offsetInBlock
	var out []Extent

	for _, run := range runs {
		// Intersect the sector run with the requested range.
		runStart := max(blockStart+run.First*sectorSize, at)
		runEnd := min(blockStart+(run.Last+1)*sectorSize, at+span)
		if runEnd <= runStart {
			continue
		}

		if !run.InChild {
			sub, err := d.parentExtents(runStart, runEnd-runStart, chainIndex)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
			continue
		}

		fileOffset, mapped, err := d.blockFileOffset(blockIndex, runStart-blockStart)
		if err != nil {
			return nil, err
		}
		e := Extent{
			VirtualOffset: runStart,
			Length:        runEnd - runStart,
			ChainIndex:    chainIndex,
			Path:          d.path,
		}
		if mapped {
			e.Kind = ExtentMapped
			e.FileOffset = fileOffset
		} else {
			// The sector bitmap claims this sector for the child, yet the block
			// has no bytes behind it. The child asserted ownership, so the
			// parent's contents here are overridden -- this is the child's zero,
			// not an untouched hole.
			e.Kind = ExtentZeroedByChild
		}
		out = append(out, e)
	}

	return out, nil
}

// parentExtents delegates a range to the parent chain, or reports it unresolved
// when no parent is attached.
func (d *VirtualDisk) parentExtents(at, span int64, chainIndex int) ([]Extent, error) {
	if d.parent == nil {
		return []Extent{{
			VirtualOffset: at,
			Length:        span,
			Kind:          ExtentUnresolved,
			ChainIndex:    chainIndex + 1,
		}}, nil
	}
	return d.parent.rawExtents(at, span, chainIndex+1)
}

// blockFileOffset resolves a byte within a block to its offset in this disk's
// file, reporting mapped=false for a block with no backing bytes.
func (d *VirtualDisk) blockFileOffset(blockIndex uint32, offsetInBlock int64) (int64, bool, error) {
	switch d.format {
	case types.FileFormatVHD:
		if d.childBATvhd == nil {
			return 0, false, errors.New("VHD BAT not available")
		}
		if blockIndex >= d.childBATvhd.NumberOfEntries {
			return 0, false, io.EOF
		}
		entry := d.childBATvhd.Entries[blockIndex]
		if !entry.IsAllocated || entry.FileOffset < 0 {
			return 0, false, nil
		}
		return entry.FileOffset + offsetInBlock, true, nil

	case types.FileFormatVHDX:
		if d.childBATvhdx == nil {
			return 0, false, errors.New("VHDX BAT not available")
		}
		if blockIndex >= d.childBATvhdx.NumberOfEntries {
			return 0, false, io.EOF
		}
		entry := d.childBATvhdx.Entries[blockIndex]
		if !entry.IsAllocated || entry.FileOffset < 0 {
			return 0, false, nil
		}
		// PARTIALLY_PRESENT blocks are allocated but only partly written, so
		// the sector bitmap decides. Reporting the whole block as mapped would
		// hand a sparse-aware imaging tool file offsets for sectors that hold
		// nothing, and it would copy them as if they were data.
		if entry.BlockState == types.BlockStatePartiallyAllocated {
			present, err := d.vhdxSectorPresent(blockIndex, offsetInBlock)
			if err != nil || !present {
				return 0, false, err
			}
		}
		return entry.FileOffset + offsetInBlock, true, nil

	default:
		return 0, false, errors.New("unknown disk format")
	}
}

// mergeExtents joins adjacent extents that describe one continuous run, so the
// result is as coarse as the underlying layout allows.
func mergeExtents(in []Extent) []Extent {
	if len(in) == 0 {
		return nil
	}

	out := make([]Extent, 0, len(in))
	cur := normalizeExtent(in[0])

	for _, next := range in[1:] {
		next = normalizeExtent(next)
		if canMerge(cur, next) {
			cur.Length += next.Length
			continue
		}
		out = append(out, cur)
		cur = next
	}
	return append(out, cur)
}

// normalizeExtent clears fields that carry no meaning for a kind, so a zero run
// does not appear to have provenance it does not have.
func normalizeExtent(e Extent) Extent {
	if e.Kind == ExtentZero {
		e.FileOffset = 0
		e.ChainIndex = 0
		e.Path = ""
	}
	if e.Kind == ExtentUnresolved {
		e.FileOffset = 0
		e.Path = ""
	}
	return e
}

// canMerge reports whether b continues a directly.
//
// Mapped runs must also stay contiguous within the same file, or the merged
// extent's FileOffset would describe bytes that are not there.
func canMerge(a, b Extent) bool {
	if a.End() != b.VirtualOffset || a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case ExtentMapped:
		return a.ChainIndex == b.ChainIndex &&
			a.Path == b.Path &&
			a.FileOffset+a.Length == b.FileOffset
	case ExtentUnresolved:
		// Keep runs waiting on different parents distinct, so a caller can see
		// which link is missing.
		return a.ChainIndex == b.ChainIndex
	default: // ExtentZero
		return true
	}
}
