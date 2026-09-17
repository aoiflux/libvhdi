// SPDX-License-Identifier: MIT

// Package vhdimap declares the bridge between libvhdi's block-level view of a
// virtual disk and a filesystem's file-level view of it.
//
// libvhdi can say which byte ranges of a disk a checkpoint wrote. Turning that
// into "which files changed" needs something that understands the filesystem on
// the disk, and this package is the contract such a thing has to satisfy.
//
// # Why this is only interfaces
//
// The core libvhdi module has no dependencies and must never acquire one: a
// consumer who wants only VHD/VHDX parsing has to be able to take it without
// absorbing a filesystem library into their module graph. So this package
// declares the shape of a filesystem and implements none, names no particular
// library, and imports nothing but the standard library.
//
// Implement it with libvhdi/change, which adapts the aoiflux filesystem
// libraries, with your own parser, or with something else entirely. libvhdi
// neither knows nor cares.
//
// # Why the optional parts are separate interfaces
//
// Journal and OwnerIndex are separate rather than methods on Filesystem that
// return ErrUnsupported. A partial implementation is then a compile-time fact
// that a type switch can see, rather than a runtime surprise. A parser that
// implements only Filesystem produces a working -- if less precise -- result,
// and one that also implements Journal produces a better one, with no version
// of either lying about what it can do.
package vhdimap

import (
	"context"
	"errors"
	"sort"
	"time"
)

// ErrNotSupported is returned by an implementation that cannot answer a
// particular question about an otherwise readable filesystem.
//
// It is for genuine gaps in a format -- a filesystem that records no creation
// time, say -- and not for absent capabilities, which are expressed by not
// implementing the optional interface at all.
var ErrNotSupported = errors.New("vhdimap: not supported by this filesystem")

// ErrIdentityReused is returned by a lookup whose file number is occupied by a
// different file than the one asked for -- the generation on disk has moved on
// from the generation in the FileID.
//
// It is a distinct error rather than "not found" because the two mean opposite
// things to a change report. Not found says the file is gone; reused says the
// file is gone and something else now sits in its slot, and treating the second
// as the first is what produces a report claiming a file was modified when it
// was deleted and an unrelated file created over it.
var ErrIdentityReused = errors.New("vhdimap: file identity has been reused")

// ByteRange is a contiguous run of bytes, located on the disk.
//
// Offset is measured from the start of the disk, not from the start of the
// volume. A volume inside a partition must therefore add its own base offset
// before reporting, so that what comes back can be intersected directly with
// libvhdi's changed ranges, which are whole-disk absolute.
//
// This is the one rule in this package that is not negotiable, because getting
// it wrong produces a confident wrong answer rather than an error: a
// volume-relative range compared against a whole-disk one still intersects, and
// nothing downstream can tell that the answer is nonsense. Every file appears
// changed, or none does.
//
// Requiring one coordinate space rather than documenting two is what removes
// the mistake. It also matches what a filesystem parser naturally produces: a
// parser given the whole disk and told where its volume starts reports absolute
// offsets already, and every one of the six libraries libvhdi/change adapts
// does exactly that. Capabilities.BaseOffset records the base that was applied,
// for reporting -- not for the caller to add.
type ByteRange struct {
	Offset int64
	Length int64
}

// End returns the first offset past the range.
func (r ByteRange) End() int64 { return r.Offset + r.Length }

// Overlaps reports whether two ranges share any byte.
func (r ByteRange) Overlaps(other ByteRange) bool {
	return r.Offset < other.End() && other.Offset < r.End()
}

// Intersect returns the overlap of two ranges and whether there is one.
func (r ByteRange) Intersect(other ByteRange) (ByteRange, bool) {
	start := max64(r.Offset, other.Offset)
	end := min64(r.End(), other.End())
	if end <= start {
		return ByteRange{}, false
	}
	return ByteRange{Offset: start, Length: end - start}, true
}

// FileExtent is one run of a file's content, located on the disk and placed
// within the file.
//
// Both axes are needed and neither implies the other. The disk range is what
// intersects with libvhdi's changed ranges to decide whether a file changed at
// all; the file offset is what turns that answer into "the first megabyte of
// this file changed" rather than only "this file changed". Every filesystem
// parser has both to hand, because it derived one from the other.
//
// FileOffset is not the sum of the preceding runs' lengths whenever a file has
// holes: a sparse run occupies file offsets while occupying no disk, so it is
// reported as its own extent with a zero-length disk range rather than elided.
// Deriving the position by accumulation would silently shift every offset after
// the first hole.
type FileExtent struct {
	ByteRange

	// FileOffset is where this run begins within the file's own byte space.
	FileOffset int64
}

// Sparse reports whether this extent is a hole: file offsets backed by no disk.
func (e FileExtent) Sparse() bool { return e.Length <= 0 }

// FileID identifies a file within one volume, stably across time.
//
// Number alone is not enough. Every filesystem that reuses file numbers --
// which is all of them -- needs the reuse counter as well, or a number that has
// been recycled for a different file is indistinguishable from the original
// file having been modified. That mistake produces a change report claiming a
// file was edited when it was in fact deleted and a different one created in
// its place.
//
// The pair is spelled generically because each filesystem spells it
// differently: NTFS packs an MFT record number with a sequence number, ext and
// XFS pair an inode number with a generation, and FAT and exFAT have neither
// and must synthesise something. An implementation that can only synthesise an
// identity has to say so -- see Capabilities.StableIdentity.
type FileID struct {
	// Number is the filesystem's own file number.
	Number uint64

	// Generation is the reuse counter. Zero is a legitimate value on
	// filesystems that have one; a filesystem that has none leaves it zero
	// always and reports StableIdentity as false.
	Generation uint64
}

// IsZero reports whether the identity is unset.
func (id FileID) IsZero() bool { return id.Number == 0 && id.Generation == 0 }

// Times holds a file's timestamps. A zero value means the filesystem does not
// record that stamp, or did not record it for this file -- Capabilities
// distinguishes the two.
type Times struct {
	Created  time.Time
	Modified time.Time
	Accessed time.Time
	Changed  time.Time
}

// FileEntry describes one file, directory or named stream.
type FileEntry struct {
	// ID identifies the file, and ParentID the directory holding it.
	//
	// The parent's identity rather than its name is what makes a rename
	// detectable: a file moved between directories keeps its ID and changes its
	// ParentID, which a name comparison cannot see.
	ID       FileID
	ParentID FileID

	// Name is the entry's name within its parent, not a path.
	Name string

	// Path is the full path when the implementation can supply one cheaply. It
	// may be empty; the caller reconstructs it from ParentID if so.
	Path string

	// Size is the file's length in bytes.
	Size int64

	// IsDir reports whether this entry is a directory.
	IsDir bool

	// Deleted marks an entry recovered from a deleted record rather than a live
	// one. Such an entry's contents may already have been overwritten.
	Deleted bool

	// Times holds whichever timestamps the filesystem records.
	Times Times

	// Stream names an alternate data stream, and is empty for the file's main
	// contents. NTFS is the format this exists for: a change confined to an
	// alternate stream is a change to the file, and reporting only the unnamed
	// stream would miss it.
	Stream string
}

// Capabilities describes what a filesystem implementation can actually answer,
// so a caller can distinguish "this format does not record X" from "X was
// absent for this file".
//
// Without this a change report cannot tell the difference between a filesystem
// with no access times and one where every access time happened to be zero, and
// would claim every file's atime changed.
type Capabilities struct {
	// Name identifies the filesystem, such as "ntfs" or "ext4".
	Name string

	// StableIdentity reports whether FileID survives across volume states. When
	// false, the implementation is synthesising an identity from position or
	// name, so rename detection is inference rather than proof and a report
	// should say so.
	StableIdentity bool

	// HasGeneration reports whether the filesystem records a reuse counter. When
	// false, a recycled file number cannot be detected.
	HasGeneration bool

	// RecordsCreated, RecordsAccessed and RecordsChanged report which optional
	// timestamps the format carries at all.
	RecordsCreated  bool
	RecordsAccessed bool
	RecordsChanged  bool

	// HasNamedStreams reports whether the format supports alternate data
	// streams.
	HasNamedStreams bool

	// HasJournal reports whether a journal exists on this volume. It can be
	// false even on a format that supports one, when the particular volume has
	// none.
	HasJournal bool

	// BaseOffset is the volume's offset within the disk the implementation was
	// given.
	//
	// It is reported so that a change document can say where the volume it
	// describes sat, and so that a caller can sanity-check that two states
	// being compared were opened at the same place. It is not an adjustment to
	// apply: ByteRange offsets already include it.
	BaseOffset int64
}

// Filesystem is what a volume parser must provide for file-level change
// detection.
//
// Implement it with the sibling filesystem libraries, with your own parser, or
// with anything else. Every method must be safe to call concurrently on one
// value, or the implementation must document that it is not.
type Filesystem interface {
	// Capabilities describes what this implementation can answer.
	Capabilities() Capabilities

	// VolumeIdentity returns a stable identifier for the volume, used to
	// confirm two checkpoint states describe the same volume before their
	// contents are compared. Comparing two different volumes would report every
	// file as changed.
	VolumeIdentity(ctx context.Context) (string, error)

	// WalkFiles calls fn for every entry on the volume. Returning an error from
	// fn stops the walk and is returned.
	WalkFiles(ctx context.Context, fn func(FileEntry) error) error

	// FileByID returns one entry by identity.
	FileByID(ctx context.Context, id FileID) (FileEntry, error)

	// ExtentsForFile returns the runs backing a file, sorted by position within
	// the file. Their disk offsets are absolute, in the same coordinate space
	// as the ranges libvhdi reports -- see ByteRange.
	//
	// This is the decisive operation. Intersecting these ranges with the ranges
	// libvhdi says a checkpoint wrote yields a small candidate set, so a 1 TB
	// volume is examined in seconds rather than hashed end to end.
	//
	// Content stored inside a metadata record rather than in its own blocks --
	// a resident NTFS attribute, an inline ext inode -- is not reported. Those
	// bytes live in the MFT or the inode table, which every other record in the
	// same block shares, so reporting them would attribute one file's change to
	// all of its neighbours.
	ExtentsForFile(ctx context.Context, id FileID) ([]FileExtent, error)
}

// OwnerIndex is an optional interface for a parser that can answer reverse
// lookups natively: given a byte range, which files occupy it.
//
// Nothing implements this today, and nothing needs to: a caller can build the
// same index from WalkFiles and ExtentsForFile. It exists so that a parser
// which can do better is not forced to pretend it cannot.
type OwnerIndex interface {
	OwnersOfRange(ctx context.Context, r ByteRange) ([]FileID, error)
}

// RenameEvidence is a rename observed in a journal.
type RenameEvidence struct {
	ID        FileID
	OldName   string
	NewName   string
	OldParent FileID
	NewParent FileID
	When      time.Time
}

// Journal is an optional interface for a filesystem carrying a change log.
//
// A journal is the only high-fidelity source of renames. Without one, a rename
// is inferred from a file keeping its identity while changing its name or
// parent, which is usually right and occasionally not. A report built on
// inference should say which it is, and this interface's presence is how a
// caller knows.
type Journal interface {
	// RenamesBetween returns the renames recorded between two volume states,
	// identified by whatever opaque marker VolumeState produced.
	RenamesBetween(ctx context.Context, from, to string) ([]RenameEvidence, error)

	// VolumeState returns an opaque marker for the volume's current position in
	// its journal, to be passed to RenamesBetween later.
	VolumeState(ctx context.Context) (string, error)
}

// ChangedBlockSource is what libvhdi itself provides to the diff: the ranges of
// a disk that were written.
//
// It is declared here rather than taken as a *libvhdi.Disk so that the engine
// can be tested against an in-memory fake, and so that a caller with changed
// ranges from another source -- a storage array's own change tracking, say --
// can use the same machinery.
type ChangedBlockSource interface {
	ChangedRanges(ctx context.Context) ([]ByteRange, error)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Coalesce sorts ranges by offset and merges those that overlap or abut.
//
// The result is the same set of bytes described in the fewest ranges, which is
// what makes intersecting two range sets cheap: both sides can then be walked
// once in step rather than compared pairwise. Zero-length ranges are dropped,
// since they describe nothing and would otherwise survive as noise in a report.
//
// The input is not modified.
func Coalesce(rs []ByteRange) []ByteRange {
	if len(rs) == 0 {
		return nil
	}

	in := make([]ByteRange, 0, len(rs))
	for _, r := range rs {
		if r.Length > 0 {
			in = append(in, r)
		}
	}
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].Offset != in[j].Offset {
			return in[i].Offset < in[j].Offset
		}
		return in[i].Length < in[j].Length
	})

	out := []ByteRange{in[0]}
	for _, r := range in[1:] {
		last := &out[len(out)-1]
		if r.Offset <= last.End() {
			if end := r.End(); end > last.End() {
				last.Length = end - last.Offset
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// Intersects reports whether r overlaps any range in the sorted, coalesced set
// rs, which is what Coalesce returns.
//
// It binary-searches, so testing one file's extents against a volume's changed
// ranges costs a logarithm rather than a scan. That is the difference between
// examining a million-file volume in seconds and not being able to.
func Intersects(rs []ByteRange, r ByteRange) bool {
	if r.Length <= 0 || len(rs) == 0 {
		return false
	}
	// The first range that could overlap is the last one starting at or before
	// r's end; searching for the first starting past it and stepping back one
	// finds it.
	i := sort.Search(len(rs), func(i int) bool { return rs[i].Offset >= r.End() })
	if i > 0 && rs[i-1].End() > r.Offset {
		return true
	}
	return false
}

// Overlap returns the parts of r that fall inside the sorted, coalesced set rs.
//
// It is Intersects with the answer kept, and it is what lets a report say which
// bytes of a file changed rather than only that the file did.
func Overlap(rs []ByteRange, r ByteRange) []ByteRange {
	if r.Length <= 0 || len(rs) == 0 {
		return nil
	}
	i := sort.Search(len(rs), func(i int) bool { return rs[i].End() > r.Offset })
	var out []ByteRange
	for ; i < len(rs) && rs[i].Offset < r.End(); i++ {
		if got, ok := rs[i].Intersect(r); ok {
			out = append(out, got)
		}
	}
	return out
}
