// SPDX-License-Identifier: MIT

package change

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/aoiflux/libvhdi/vhdimap"
)

// Kind says what happened to a file between two volume states.
type Kind string

const (
	// KindAdded means the file's identity is present in the newer state and
	// absent from the older one.
	KindAdded Kind = "added"

	// KindDeleted means the reverse: present in the older state, gone from the
	// newer one.
	KindDeleted Kind = "deleted"

	// KindModified means the identity is present in both and the newer state
	// wrote bytes the checkpoint reports as changed.
	KindModified Kind = "modified"

	// KindRenamed means the identity is present in both under a different name,
	// a different parent directory, or both. A rename may also be a
	// modification; RenamedAndModified reports that case.
	KindRenamed Kind = "renamed"
)

// Confidence says what a classification rests on.
//
// The three levels are not a quality score. They record which kind of evidence
// was available, so a report can be read for what it proves rather than for
// what it asserts.
type Confidence string

const (
	// ConfidenceProven means a journal recorded the event. Only NTFS's USN
	// journal supplies this today.
	ConfidenceProven Confidence = "proven"

	// ConfidenceIdentified means the filesystem carries a real reuse counter,
	// so the file was followed across the two states by an identity the
	// filesystem itself maintains. A rename found this way is not a guess: the
	// same file is under a different name. What is missing is the event, so the
	// intermediate steps and the time it happened are unknown.
	ConfidenceIdentified Confidence = "identified"

	// ConfidenceInferred means the identity was synthesised, from a position in
	// a directory or from a creation time, because the format records none.
	// FAT, exFAT and HFS+ are in this position. A rename here is an inference
	// that is usually right, and a report must say so.
	ConfidenceInferred Confidence = "inferred"
)

// FileChange is one file the diff attributes a change to.
type FileChange struct {
	// Kind says what happened.
	Kind Kind

	// Entry is the file's state in the newer volume. For a deletion it is the
	// zero value, since the file is no longer there; Previous holds what it was.
	Entry vhdimap.FileEntry

	// Previous is the file's state in the older volume, set for a modification,
	// a rename and a deletion.
	Previous vhdimap.FileEntry

	// Modified reports whether the file's own bytes changed. It is set
	// independently of Kind, so a file that was both renamed and edited is one
	// change rather than two.
	Modified bool

	// Ranges are the byte ranges within the file that changed, as far as the
	// extent maps narrow it. They are file-relative.
	Ranges []vhdimap.ByteRange

	// Confidence says what the classification rests on.
	Confidence Confidence

	// Evidence is set when a journal corroborated the change.
	Evidence *vhdimap.RenameEvidence
}

// VolumeDiff is what changed on one volume between two states.
type VolumeDiff struct {
	// Capabilities describes the newer state's filesystem, and is what a
	// consumer should read before trusting any confidence level in Changes.
	Capabilities vhdimap.Capabilities

	// VolumeIdentity is the volume both states agreed they were.
	VolumeIdentity string

	// Changes are the files that changed, sorted by path.
	Changes []FileChange

	// Notes record why the result is limited. They are written for a reader of
	// the report rather than for a programmer, because an unexplained result is
	// what gets over-read.
	Notes []string
}

// DiffOptions controls a comparison.
type DiffOptions struct {
	// AllowVolumeIdentityMismatch compares two volumes that do not agree on
	// their own identity.
	//
	// The default is to refuse, because comparing two different volumes reports
	// every file as changed and nothing in the output says the comparison was
	// meaningless. Set this only when the mismatch is understood -- a volume
	// reformatted between checkpoints, say -- and expect a note saying so.
	AllowVolumeIdentityMismatch bool

	// MaxFiles caps how many changes are reported. Zero means no cap. When the
	// cap is hit a note records it, so a truncated document cannot be mistaken
	// for a complete one.
	MaxFiles int

	// SkipDirectories leaves directories out of the result.
	//
	// A directory's blocks change whenever a file is added to or removed from
	// it, so directories dominate the raw candidate set and say little that the
	// file entries do not. They are included by default because a change to a
	// directory that contains no changed file is itself worth seeing.
	SkipDirectories bool
}

// ErrVolumeMismatch is returned when two states do not describe the same
// volume.
var ErrVolumeMismatch = errors.New("change: the two states are different volumes")

// Diff reports which files changed between two states of one volume.
//
// changed is the set of disk ranges libvhdi says were written -- whole-disk
// absolute, the same coordinate space the filesystems report in. The comparison
// is guided by those ranges rather than by inventorying both volumes: a file is
// examined only when its extents intersect something that was written, which is
// what makes a terabyte volume tractable.
//
// from is the older state and to the newer one. Both may be nil-free; neither
// is written to.
func Diff(ctx context.Context, from, to vhdimap.Filesystem, changed []vhdimap.ByteRange, opts *DiffOptions) (*VolumeDiff, error) {
	if from == nil || to == nil {
		return nil, fmt.Errorf("change: both states are required")
	}
	if opts == nil {
		opts = &DiffOptions{}
	}

	caps := to.Capabilities()
	out := &VolumeDiff{Capabilities: caps}

	if err := checkIdentity(ctx, from, to, opts, out); err != nil {
		return nil, err
	}

	// Coalescing first is what makes the per-file test a binary search rather
	// than a scan over every changed range.
	written := vhdimap.Coalesce(changed)
	if len(written) == 0 {
		out.Notes = append(out.Notes, "the checkpoint wrote nothing to this volume's range of the disk")
		return out, nil
	}

	oldEntries, err := inventory(ctx, from)
	if err != nil {
		return nil, fmt.Errorf("change: older state: %w", err)
	}
	newEntries, err := inventory(ctx, to)
	if err != nil {
		return nil, fmt.Errorf("change: newer state: %w", err)
	}

	base := confidenceFor(caps)
	changes := make([]FileChange, 0, 16)

	// Files present in the newer state: added, modified or renamed. Only those
	// whose extents intersect a written range are examined, which is the whole
	// point of being block-guided.
	for id, entry := range newEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir && opts.SkipDirectories {
			continue
		}

		ranges, touched, err := changedWithin(ctx, to, id, written)
		if err != nil {
			// A file whose map cannot be read is not evidence that it did not
			// change. It is noted and skipped, rather than silently counted as
			// unchanged.
			out.Notes = append(out.Notes, fmt.Sprintf("extent map unreadable for %s: %v", describe(entry), err))
			continue
		}

		prev, existed := oldEntries[id]
		renamed := existed && (prev.Name != entry.Name || prev.ParentID != entry.ParentID)

		switch {
		case !existed:
			changes = append(changes, FileChange{
				Kind: KindAdded, Entry: entry, Modified: touched,
				Ranges: ranges, Confidence: base,
			})
		case renamed:
			changes = append(changes, FileChange{
				Kind: KindRenamed, Entry: entry, Previous: prev, Modified: touched,
				Ranges: ranges, Confidence: base,
			})
		case touched:
			changes = append(changes, FileChange{
				Kind: KindModified, Entry: entry, Previous: prev, Modified: true,
				Ranges: ranges, Confidence: base,
			})
		}
	}

	// Files that were in the older state and are not in the newer one. This is
	// a set difference rather than a block test, because a deletion frees the
	// file's blocks: there is nothing left to intersect, and a deletion found
	// only by looking for writes is a deletion missed.
	for id, prev := range oldEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if prev.IsDir && opts.SkipDirectories {
			continue
		}
		if _, ok := newEntries[id]; ok {
			continue
		}
		changes = append(changes, FileChange{
			Kind: KindDeleted, Previous: prev, Confidence: base,
		})
	}

	corroborate(ctx, from, to, changes, out)

	sort.SliceStable(changes, func(i, j int) bool {
		a, b := sortKey(changes[i]), sortKey(changes[j])
		if a != b {
			return a < b
		}
		return changes[i].Kind < changes[j].Kind
	})

	if opts.MaxFiles > 0 && len(changes) > opts.MaxFiles {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"truncated to %d of %d changes by MaxFiles; this document does not list every change",
			opts.MaxFiles, len(changes)))
		changes = changes[:opts.MaxFiles]
	}

	out.Changes = changes
	addCapabilityNotes(caps, out)
	return out, nil
}

// checkIdentity refuses to compare two volumes that say they are different
// ones, and records what it could and could not confirm.
func checkIdentity(ctx context.Context, from, to vhdimap.Filesystem, opts *DiffOptions, out *VolumeDiff) error {
	oldID, oldErr := from.VolumeIdentity(ctx)
	newID, newErr := to.VolumeIdentity(ctx)
	out.VolumeIdentity = newID

	if oldErr != nil || newErr != nil {
		out.Notes = append(out.Notes,
			"volume identity could not be read on both states, so the two were not confirmed to be the same volume")
		return nil
	}
	if oldID == newID {
		return nil
	}
	if !opts.AllowVolumeIdentityMismatch {
		return fmt.Errorf("%w: %q then %q", ErrVolumeMismatch, oldID, newID)
	}
	out.Notes = append(out.Notes, fmt.Sprintf(
		"the two states report different volume identities (%q then %q); "+
			"the comparison was run anyway and every file may appear changed", oldID, newID))
	return nil
}

// inventory walks a state and keys every entry by identity.
//
// The walk reads names and identities, not extent maps, so it costs one pass
// over the filesystem's own tables. The extent maps are read per candidate
// afterwards, which is the expensive part and is why it is not done here.
func inventory(ctx context.Context, fs vhdimap.Filesystem) (map[vhdimap.FileID]vhdimap.FileEntry, error) {
	out := make(map[vhdimap.FileID]vhdimap.FileEntry)
	err := fs.WalkFiles(ctx, func(e vhdimap.FileEntry) error {
		if e.ID.IsZero() {
			// Nothing downstream could refer to an entry with no identity, and
			// keying several of them together under the zero ID would merge
			// unrelated files.
			return nil
		}
		if e.Deleted {
			// A record recovered from a deleted entry describes the past, not
			// the state being compared. Counting it as present would make every
			// deletion look like it had not happened.
			return nil
		}
		out[e.ID] = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// changedWithin returns which parts of a file fall inside the written ranges,
// as file-relative ranges, and whether any do.
func changedWithin(ctx context.Context, fs vhdimap.Filesystem, id vhdimap.FileID, written []vhdimap.ByteRange) ([]vhdimap.ByteRange, bool, error) {
	extents, err := fs.ExtentsForFile(ctx, id)
	if err != nil {
		return nil, false, err
	}

	var out []vhdimap.ByteRange
	for _, e := range extents {
		if e.Sparse() {
			// A hole occupies no disk, so nothing can have been written to it.
			continue
		}
		for _, hit := range vhdimap.Overlap(written, e.ByteRange) {
			// The overlap is a disk range; subtracting the extent's own disk
			// offset and adding its position in the file moves it into the
			// file's coordinate space.
			out = append(out, vhdimap.ByteRange{
				Offset: e.FileOffset + (hit.Offset - e.Offset),
				Length: hit.Length,
			})
		}
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	return vhdimap.Coalesce(out), true, nil
}

// corroborate upgrades renames the journal actually recorded, and adds the ones
// identity alone did not see.
func corroborate(ctx context.Context, from, to vhdimap.Filesystem, changes []FileChange, out *VolumeDiff) {
	journal, ok := to.(vhdimap.Journal)
	if !ok {
		return
	}
	oldJournal, ok := from.(vhdimap.Journal)
	if !ok {
		return
	}

	since, err := oldJournal.VolumeState(ctx)
	if err != nil {
		out.Notes = append(out.Notes, fmt.Sprintf("journal position unreadable on the older state: %v", err))
		return
	}
	until, err := journal.VolumeState(ctx)
	if err != nil {
		out.Notes = append(out.Notes, fmt.Sprintf("journal position unreadable on the newer state: %v", err))
		return
	}

	renames, err := journal.RenamesBetween(ctx, since, until)
	if err != nil {
		out.Notes = append(out.Notes, fmt.Sprintf("journal unreadable: %v", err))
		return
	}
	if len(renames) == 0 {
		return
	}

	byID := make(map[vhdimap.FileID]vhdimap.RenameEvidence, len(renames))
	for _, r := range renames {
		byID[r.ID] = r
	}
	for i := range changes {
		id := changes[i].Entry.ID
		if id.IsZero() {
			id = changes[i].Previous.ID
		}
		ev, ok := byID[id]
		if !ok {
			continue
		}
		// The journal saw the event. That promotes a rename from "the same file
		// is under a different name" to "the rename was recorded happening",
		// and it also explains a file that identity read as merely modified --
		// a file renamed back to its original name within the window looks
		// unchanged by name and is not.
		evidence := ev
		changes[i].Evidence = &evidence
		changes[i].Confidence = ConfidenceProven
		if changes[i].Kind == KindModified {
			changes[i].Kind = KindRenamed
		}
	}
}

// confidenceFor picks the level the filesystem's own identity supports.
func confidenceFor(caps vhdimap.Capabilities) Confidence {
	if caps.StableIdentity && caps.HasGeneration {
		return ConfidenceIdentified
	}
	return ConfidenceInferred
}

func addCapabilityNotes(caps vhdimap.Capabilities, out *VolumeDiff) {
	if !caps.StableIdentity {
		out.Notes = append(out.Notes, caps.Name+
			" records no file identity of its own, so identity here is synthesised: "+
			"a rename is an inference, and a record slot reused by a different file "+
			"can read as a modification")
	} else if !caps.HasGeneration {
		out.Notes = append(out.Notes, caps.Name+
			" records a file number but no reuse counter, so a recycled number "+
			"cannot be distinguished from the original file being modified")
	}
	if caps.HasNamedStreams {
		out.Notes = append(out.Notes,
			"a change confined to an alternate data stream is reported against the file that owns it")
	}
}

func sortKey(c FileChange) string {
	if c.Entry.Path != "" {
		return c.Entry.Path
	}
	if c.Previous.Path != "" {
		return c.Previous.Path
	}
	if c.Entry.Name != "" {
		return c.Entry.Name
	}
	return c.Previous.Name
}

func describe(e vhdimap.FileEntry) string {
	if e.Path != "" {
		return e.Path
	}
	if e.Name != "" {
		return e.Name
	}
	return fmt.Sprintf("file %d.%d", e.ID.Number, e.ID.Generation)
}
