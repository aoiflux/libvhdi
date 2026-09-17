// SPDX-License-Identifier: MIT

package report

import (
	"context"
	"fmt"

	"github.com/aoiflux/libvhdi/reader"
)

// Extent is one contiguous run of the virtual disk and what backs it.
type Extent struct {
	// VirtualOffset and Length bound the run in the device's address space.
	VirtualOffset int64 `json:"virtual_offset"`
	Length        int64 `json:"length"`

	// Kind is one of "mapped", "zero", "zeroed-by-child" or "unresolved".
	//
	// The distinction between "zero" and "zeroed-by-child" is not cosmetic:
	// both read as zeroes, but the second is a write by a differencing disk
	// that overrode its parent, which is how a deletion appears at block level.
	Kind string `json:"kind"`

	// FileOffset is where the run lives in the backing file, present only for a
	// mapped run.
	FileOffset int64 `json:"file_offset,omitempty"`

	// ChainIndex identifies the backing image: 0 is the disk the report was
	// built from, 1 its parent, and so on. Path names it.
	ChainIndex int    `json:"chain_index"`
	Path       string `json:"path,omitempty"`
}

func convertExtent(e reader.Extent) Extent {
	out := Extent{
		VirtualOffset: e.VirtualOffset,
		Length:        e.Length,
		Kind:          e.Kind.String(),
		ChainIndex:    e.ChainIndex,
		Path:          e.Path,
	}
	if e.Kind == reader.ExtentMapped {
		out.FileOffset = e.FileOffset
	}
	return out
}

func convertExtents(in []reader.Extent) []Extent {
	if len(in) == 0 {
		return nil
	}
	out := make([]Extent, 0, len(in))
	for _, e := range in {
		out = append(out, convertExtent(e))
	}
	return out
}

// AllocationTotals accounts for every byte of the address space.
//
// The four categories are exhaustive and disjoint, so their sum equals the
// virtual size. A document where it does not is one the producer got wrong, and
// stating the total lets a consumer check.
type AllocationTotals struct {
	// Mapped is backed by real bytes in some image in the chain.
	Mapped int64 `json:"mapped"`

	// Zero was never written anywhere in the chain.
	Zero int64 `json:"zero"`

	// ZeroedByChild was explicitly cleared by a differencing disk.
	ZeroedByChild int64 `json:"zeroed_by_child"`

	// Unresolved needs an image that is not attached, so its contents are
	// unknown. It is neither present nor absent, and counting it as either
	// would be a guess.
	Unresolved int64 `json:"unresolved"`

	// Total is the device's virtual size and equals the sum of the four above.
	Total int64 `json:"total"`

	MappedHuman string `json:"mapped_human"`
	TotalHuman  string `json:"total_human"`
}

// AllocationReport describes which parts of a device are backed by data, which
// is what a sparse-aware acquisition needs in order to read only what exists.
type AllocationReport struct {
	// Header is embedded, so its fields appear at the top level of the
	// document rather than nested under a key.
	Header

	Source   Source           `json:"source"`
	Geometry Geometry         `json:"geometry"`
	Totals   AllocationTotals `json:"totals"`

	// Extents tile the whole address space in order, when included.
	//
	// They are omitted by default: a fragmented multi-terabyte device can have
	// millions of them, which makes a document nobody can open. The totals
	// answer most questions on their own.
	Extents []Extent `json:"extents,omitempty"`

	// ExtentCount is the number of extents, reported whether or not they are
	// included -- so a document without them still says how many there were.
	ExtentCount int `json:"extent_count"`

	Warnings []Warning `json:"warnings,omitempty"`
}

// AllocationOptions controls an allocation report.
type AllocationOptions struct {
	Options

	// IncludeExtents puts the full extent list in the document.
	//
	// Off by default because a fragmented large device produces millions of
	// entries. Turn it on when the consumer actually walks them, such as a
	// sparse copier driving its reads from the document.
	IncludeExtents bool

	// MaxExtents caps how many are included, with zero meaning no cap. When the
	// cap truncates the list, ExtentCount still reports the true total, so a
	// consumer can tell a truncated document from a complete one.
	MaxExtents int
}

// Allocation builds a report describing which parts of a device hold data.
func Allocation(ctx context.Context, d *reader.VirtualDisk, opts *AllocationOptions) (*AllocationReport, error) {
	if d == nil {
		return nil, fmt.Errorf("report: nil disk")
	}

	var base *Options
	if opts != nil {
		base = &opts.Options
	}

	src, err := sourceOf(d, base)
	if err != nil {
		return nil, err
	}

	extents, err := d.AllExtentsContext(ctx)
	if err != nil {
		return nil, err
	}

	r := &AllocationReport{
		Header:      newHeader(KindAllocation),
		Source:      src,
		Geometry:    convertGeometry(d.Geometry()),
		ExtentCount: len(extents),
		Warnings:    convertWarnings(d.Warnings()),
	}

	for _, e := range extents {
		switch e.Kind {
		case reader.ExtentMapped:
			r.Totals.Mapped += e.Length
		case reader.ExtentZero:
			r.Totals.Zero += e.Length
		case reader.ExtentZeroedByChild:
			r.Totals.ZeroedByChild += e.Length
		case reader.ExtentUnresolved:
			r.Totals.Unresolved += e.Length
		}
	}
	r.Totals.Total = int64(d.Size())
	r.Totals.MappedHuman = humanBytes(r.Totals.Mapped)
	r.Totals.TotalHuman = humanBytes(r.Totals.Total)

	if opts != nil && opts.IncludeExtents {
		if opts.MaxExtents > 0 && len(extents) > opts.MaxExtents {
			extents = extents[:opts.MaxExtents]
		}
		r.Extents = convertExtents(extents)
	}

	return r, nil
}

// ============================================================================
// Change
// ============================================================================

// ChangeTier says how a change report was produced, and therefore what it can
// be trusted to say.
type ChangeTier string

const (
	// TierBlock means only block-level detection ran: the document says which
	// byte ranges changed and nothing about files.
	TierBlock ChangeTier = "block"

	// TierFile means a filesystem parser attributed the changed ranges to
	// files, but renames are inferred rather than proven.
	TierFile ChangeTier = "file"

	// TierJournal means a filesystem journal corroborated the renames, which is
	// the only high-fidelity source for them.
	TierJournal ChangeTier = "journal"
)

// ChangedFile is one file a change report attributes a change to.
//
// These types live in the core module on purpose. The schema belongs in one
// place so its versioning stays coherent, and it means a pipeline that reads
// and validates change reports never imports a filesystem library -- only the
// code that produces them does.
type ChangedFile struct {
	// Path is the file's full path on the volume.
	Path string `json:"path"`

	// Change is "added", "deleted", "modified" or "renamed".
	Change string `json:"change"`

	// PreviousPath is set for a rename.
	PreviousPath string `json:"previous_path,omitempty"`

	// FileNumber and Generation are the filesystem's identity for the file.
	// Generation is the reuse counter; without it a recycled file number is
	// indistinguishable from the original file being modified.
	FileNumber uint64 `json:"file_number"`
	Generation uint64 `json:"generation"`

	// Stream names an alternate data stream, empty for the file's main
	// contents.
	Stream string `json:"stream,omitempty"`

	// Size is the file's length after the change.
	Size int64 `json:"size"`

	// ChangedRanges are the byte ranges within the file that changed, when the
	// producer can narrow it that far.
	ChangedRanges []FileRange `json:"changed_ranges,omitempty"`

	// Confidence is "proven" or "inferred". A rename detected without a journal
	// is inference, and a report that does not say so overstates itself.
	Confidence string `json:"confidence,omitempty"`
}

// FileRange is a byte range within a file.
type FileRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// ChangedVolume groups the changes found on one volume of a disk.
type ChangedVolume struct {
	// Filesystem names the format, such as "ntfs" or "ext4".
	Filesystem string `json:"filesystem"`

	// VolumeIdentity is the volume's own identifier, which is what confirms two
	// checkpoint states describe the same volume. Comparing two different
	// volumes would report every file as changed.
	VolumeIdentity string `json:"volume_identity,omitempty"`

	// BaseOffset is the volume's offset within the disk. Changed ranges from
	// libvhdi are whole-disk absolute and a filesystem's are volume-relative,
	// so this is what relates them -- and getting it wrong produces a confident
	// wrong answer rather than an error.
	BaseOffset int64 `json:"base_offset"`

	// Files are the changed files.
	Files []ChangedFile `json:"files"`

	// Notes record why the volume's result is limited, such as a filesystem
	// with no stable file identity making rename detection inferential.
	Notes []string `json:"notes,omitempty"`
}

// ChangeReport describes what a checkpoint wrote.
//
// At the block tier Volumes is absent entirely: the document says which byte
// ranges changed and makes no claim about files. That is a complete and useful
// document on its own, and the tier field says which it is so a consumer cannot
// mistake an absent Volumes for "no files changed".
type ChangeReport struct {
	// Header is embedded, so its fields appear at the top level of the
	// document rather than nested under a key.
	Header

	Source Source `json:"source"`

	// Tier says how the report was produced and therefore what it can be
	// trusted to say.
	Tier ChangeTier `json:"tier"`

	// From and To identify the two states compared. From is the checkpoint
	// changes are measured since; To is the disk the report was built from.
	From ChangeEndpoint `json:"from"`
	To   ChangeEndpoint `json:"to"`

	// Totals account for the changed bytes.
	Totals ChangeTotals `json:"totals"`

	// Extents are the changed byte ranges, included on request.
	Extents     []Extent `json:"extents,omitempty"`
	ExtentCount int      `json:"extent_count"`

	// Volumes is present only at the file or journal tier. Its absence means
	// no filesystem analysis was run, not that no files changed.
	Volumes []ChangedVolume `json:"volumes,omitempty"`

	Warnings []Warning `json:"warnings,omitempty"`
}

// ChangeEndpoint identifies one end of a comparison.
type ChangeEndpoint struct {
	Path       string `json:"path,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	ChainIndex int    `json:"chain_index"`
}

// ChangeTotals accounts for the bytes a comparison found.
type ChangeTotals struct {
	// Written is the bytes a disk in range wrote with data.
	Written int64 `json:"written"`

	// Cleared is the bytes a disk in range explicitly zeroed, which is how a
	// deletion shows up. VHD cannot express this, so it is always zero on a VHD
	// chain -- and that is a limitation of the format, not of the analysis.
	Cleared int64 `json:"cleared"`

	// Unresolved is the bytes whose state could not be determined because an
	// image in the chain is missing. Neither changed nor unchanged.
	Unresolved int64 `json:"unresolved"`

	// Total is the device's virtual size, for proportion.
	Total int64 `json:"total"`

	WrittenHuman string `json:"written_human"`
	TotalHuman   string `json:"total_human"`
}

// ChangeOptions controls a change report.
type ChangeOptions struct {
	Options

	// IncludeExtents puts the changed byte ranges in the document.
	IncludeExtents bool

	// MaxExtents caps how many are included; zero means no cap.
	MaxExtents int
}

// Change builds a block-level change report: which ranges of the device were
// written by the disks nearer the leaf than sinceChainIndex.
//
// The result is a complete document at the block tier. The change module fills
// in Volumes to produce a file-level one; this package defines the schema for
// both so that reading a change report never requires a filesystem library.
func Change(ctx context.Context, d *reader.VirtualDisk, sinceChainIndex int, opts *ChangeOptions) (*ChangeReport, error) {
	if d == nil {
		return nil, fmt.Errorf("report: nil disk")
	}

	var base *Options
	if opts != nil {
		base = &opts.Options
	}

	src, err := sourceOf(d, base)
	if err != nil {
		return nil, err
	}

	extents, err := d.ChangedExtents(ctx, sinceChainIndex)
	if err != nil {
		return nil, err
	}

	r := &ChangeReport{
		Header:      newHeader(KindChange),
		Source:      src,
		Tier:        TierBlock,
		ExtentCount: len(extents),
		Warnings:    convertWarnings(d.Warnings()),
	}

	chain := d.Chain()
	if len(chain) > 0 {
		r.To = endpointOf(chain[0])
	}
	if sinceChainIndex < len(chain) {
		r.From = endpointOf(chain[sinceChainIndex])
	} else {
		// The index names a disk beyond the attached chain, which is legitimate
		// -- it means "since before everything we have" -- but there is no file
		// to name.
		r.From = ChangeEndpoint{ChainIndex: sinceChainIndex}
	}

	for _, e := range extents {
		switch e.Kind {
		case reader.ExtentMapped:
			r.Totals.Written += e.Length
		case reader.ExtentZeroedByChild:
			r.Totals.Cleared += e.Length
		case reader.ExtentUnresolved:
			r.Totals.Unresolved += e.Length
		}
	}
	r.Totals.Total = int64(d.Size())
	r.Totals.WrittenHuman = humanBytes(r.Totals.Written)
	r.Totals.TotalHuman = humanBytes(r.Totals.Total)

	if opts != nil && opts.IncludeExtents {
		if opts.MaxExtents > 0 && len(extents) > opts.MaxExtents {
			extents = extents[:opts.MaxExtents]
		}
		r.Extents = convertExtents(extents)
	}

	return r, nil
}

func endpointOf(e reader.ChainEntry) ChangeEndpoint {
	out := ChangeEndpoint{Path: e.Path, ChainIndex: e.Index}
	if e.Identifier != ([16]byte{}) {
		out.Identifier = reader.GUIDString(e.Identifier)
	}
	return out
}
