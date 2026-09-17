// SPDX-License-Identifier: MIT

package change

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/aoiflux/libvhdi"
	"github.com/aoiflux/libvhdi/report"
	"github.com/aoiflux/libvhdi/vhdimap"
)

// Options controls a whole-disk comparison.
type Options struct {
	// Diff controls each volume's comparison.
	Diff DiffOptions

	// Report controls the document Compare produces. IncludeExtents puts the
	// block-level ranges in it alongside the file-level findings.
	Report report.ChangeOptions

	// SkipVolumes leaves the filesystem layer out entirely, producing the same
	// block-tier document report.Change would. It is here so a caller can fall
	// back without changing code paths when a disk turns out to hold a
	// filesystem no adapter reads.
	SkipVolumes bool
}

// Compare reports which files a checkpoint wrote.
//
// leaf is the newer disk -- normally the leaf of a checkpoint chain -- and
// sinceChainIndex names the older state to compare it against, as a position in
// leaf.Chain(): 1 is the immediate parent, and 0 would be the leaf itself.
//
// The comparison is block-guided. libvhdi says which disk ranges the newer
// disks wrote, those ranges are intersected with the partitions that hold them,
// and only the files whose extents fall inside one are examined. A volume with
// no changed ranges is not opened at all.
//
// The document returned is always valid. When no volume could be read it is a
// block-tier document, which says which ranges changed and makes no claim about
// files -- a complete answer at a lower tier, and the tier field says which it
// is so an absent Volumes cannot be read as "no files changed".
func Compare(ctx context.Context, leaf *libvhdi.Disk, sinceChainIndex int, opts *Options) (*report.ChangeReport, error) {
	if leaf == nil {
		return nil, fmt.Errorf("change: nil disk")
	}
	if opts == nil {
		opts = &Options{}
	}

	ropts := opts.Report
	doc, err := report.Change(ctx, leaf, sinceChainIndex, &ropts)
	if err != nil {
		return nil, err
	}
	if opts.SkipVolumes {
		return doc, nil
	}

	older, closeOlder, err := openOlder(leaf, sinceChainIndex)
	if err != nil {
		// Without the older state there is nothing to compare against. The
		// block-tier document is still correct and still useful, so it is
		// returned with the reason recorded rather than discarded.
		doc.Warnings = append(doc.Warnings, report.Warning{
			Kind:    "change_older_state_unavailable",
			Message: err.Error(),
		})
		return doc, nil
	}
	defer closeOlder()

	changed, err := leaf.ChangedExtents(ctx, sinceChainIndex)
	if err != nil {
		return nil, err
	}
	written := writtenRanges(changed)

	volumes, notes := compareVolumes(ctx, older, leaf, written, opts)
	doc.Volumes = volumes
	for _, n := range notes {
		doc.Warnings = append(doc.Warnings, report.Warning{Kind: "change_volume", Message: n})
	}
	if len(volumes) > 0 {
		doc.Tier = tierOf(volumes)
	}
	return doc, nil
}

// compareVolumes pairs the volumes of two disk states and diffs each pair.
func compareVolumes(ctx context.Context, older, newer *libvhdi.Disk, written []vhdimap.ByteRange, opts *Options) ([]report.ChangedVolume, []string) {
	var notes []string

	newVols, newErrs := OpenVolumes(ctx, newer, int64(newer.Size()))
	for _, err := range newErrs {
		notes = append(notes, "newer state: "+err.Error())
	}
	defer closeAll(newVols)
	if len(newVols) == 0 {
		return nil, notes
	}

	oldVols, oldErrs := OpenVolumes(ctx, older, int64(older.Size()))
	for _, err := range oldErrs {
		notes = append(notes, "older state: "+err.Error())
	}
	defer closeAll(oldVols)

	byBase := make(map[int64]*Volume, len(oldVols))
	for _, v := range oldVols {
		byBase[v.Base] = v
	}

	out := make([]report.ChangedVolume, 0, len(newVols))
	for _, nv := range newVols {
		if err := ctx.Err(); err != nil {
			notes = append(notes, err.Error())
			break
		}

		ov, ok := byBase[nv.Base]
		if !ok {
			// A volume with no counterpart at the same offset is new, or the
			// layout was repartitioned. Either way there is nothing to compare
			// it against, and inventing a pairing with a volume at a different
			// offset would compare two unrelated filesystems.
			notes = append(notes, fmt.Sprintf(
				"%s volume at offset %d has no counterpart in the older state; it was not compared",
				nv.Filesystem.Capabilities().Name, nv.Base))
			continue
		}

		// Only the ranges inside this volume are its business. Handing a volume
		// the whole disk's changed ranges would make every file that happens to
		// sit at the same offset in a different partition look changed.
		span := nv.Span(int64(newer.Size()))
		inVolume := clip(written, span)
		if len(inVolume) == 0 {
			continue
		}

		diff, err := Diff(ctx, ov.Filesystem, nv.Filesystem, inVolume, &opts.Diff)
		if err != nil {
			notes = append(notes, fmt.Sprintf("volume at offset %d: %v", nv.Base, err))
			continue
		}
		out = append(out, toReport(nv, diff))
	}

	return out, notes
}

// openOlder opens the image at a chain position as a disk of its own.
//
// The chain entry's path is what makes this possible: opening that image gives
// the device as it stood at that checkpoint, with its own parents resolved
// behind it.
func openOlder(leaf *libvhdi.Disk, sinceChainIndex int) (*libvhdi.Disk, func(), error) {
	chain := leaf.Chain()
	if sinceChainIndex <= 0 || sinceChainIndex >= len(chain) {
		return nil, nil, fmt.Errorf("chain index %d is outside the attached chain of %d images",
			sinceChainIndex, len(chain))
	}
	path := chain[sinceChainIndex].Path
	if path == "" {
		return nil, nil, fmt.Errorf("chain entry %d records no path, so the older state cannot be opened", sinceChainIndex)
	}

	disk, err := libvhdi.OpenFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", filepath.Base(path), err)
	}
	return disk, func() { disk.Close() }, nil
}

// writtenRanges keeps the extents that represent a write.
//
// A cleared range counts: zeroing a region is how a deletion shows up, and
// filtering it out under-reports deletions. An unresolved range does not, since
// the image that would say what happened is missing and calling it written
// would be a guess in the direction that invents changes.
func writtenRanges(extents []libvhdi.Extent) []vhdimap.ByteRange {
	out := make([]vhdimap.ByteRange, 0, len(extents))
	for _, e := range extents {
		if !e.Kind.IsWrite() {
			continue
		}
		out = append(out, vhdimap.ByteRange{Offset: e.VirtualOffset, Length: e.Length})
	}
	return vhdimap.Coalesce(out)
}

// clip narrows a set of disk ranges to those inside one volume.
func clip(ranges []vhdimap.ByteRange, span vhdimap.ByteRange) []vhdimap.ByteRange {
	var out []vhdimap.ByteRange
	for _, r := range ranges {
		if got, ok := r.Intersect(span); ok {
			out = append(out, got)
		}
	}
	return out
}

func toReport(v *Volume, diff *VolumeDiff) report.ChangedVolume {
	out := report.ChangedVolume{
		Filesystem:     diff.Capabilities.Name,
		VolumeIdentity: diff.VolumeIdentity,
		BaseOffset:     v.Base,
		Notes:          diff.Notes,
		Files:          make([]report.ChangedFile, 0, len(diff.Changes)),
	}
	if v.PartitionName != "" {
		out.Notes = append(out.Notes, "partition: "+v.PartitionName)
	}

	for _, c := range diff.Changes {
		entry := c.Entry
		if entry.ID.IsZero() {
			entry = c.Previous
		}
		f := report.ChangedFile{
			Path:       entry.Path,
			Change:     string(c.Kind),
			FileNumber: entry.ID.Number,
			Generation: entry.ID.Generation,
			Stream:     entry.Stream,
			Size:       entry.Size,
			Confidence: string(c.Confidence),
		}
		if f.Path == "" {
			f.Path = entry.Name
		}
		if c.Kind == KindRenamed {
			f.PreviousPath = c.Previous.Path
			if f.PreviousPath == "" {
				f.PreviousPath = c.Previous.Name
			}
		}
		for _, r := range c.Ranges {
			f.ChangedRanges = append(f.ChangedRanges, report.FileRange{
				Offset: r.Offset,
				Length: r.Length,
			})
		}
		out.Files = append(out.Files, f)
	}
	return out
}

// tierOf reports the highest tier any volume reached.
//
// A document is labelled by the strongest evidence behind any of its findings,
// and the per-file Confidence says which findings those are. Labelling the
// whole document by its weakest volume would understate a journal-corroborated
// result on one volume because another had no journal.
func tierOf(volumes []report.ChangedVolume) report.ChangeTier {
	tier := report.TierFile
	for _, v := range volumes {
		for _, f := range v.Files {
			if f.Confidence == string(ConfidenceProven) {
				return report.TierJournal
			}
		}
	}
	return tier
}

func closeAll(vols []*Volume) {
	for _, v := range vols {
		v.Close()
	}
}
