// SPDX-License-Identifier: MIT

package change_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoiflux/libvhdi/change"
	"github.com/aoiflux/libvhdi/vhdimap"
)

const (
	blk = int64(4096)
	vol = "vol-uuid:1234"
)

// r is one changed disk range.
func r(offset, length int64) vhdimap.ByteRange {
	return vhdimap.ByteRange{Offset: offset, Length: length}
}

func diff(t *testing.T, from, to vhdimap.Filesystem, changed []vhdimap.ByteRange, opts *change.DiffOptions) *change.VolumeDiff {
	t.Helper()
	got, err := change.Diff(context.Background(), from, to, changed, opts)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	return got
}

// find returns the single change for a path, failing if there is not exactly
// one.
func find(t *testing.T, d *change.VolumeDiff, path string) change.FileChange {
	t.Helper()
	var hits []change.FileChange
	for _, c := range d.Changes {
		if c.Entry.Path == path || c.Previous.Path == path {
			hits = append(hits, c)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0]
	case 0:
		t.Fatalf("no change reported for %s; got %s", path, summarise(d))
	default:
		t.Fatalf("%d changes reported for %s, want 1", len(hits), path)
	}
	return change.FileChange{}
}

func summarise(d *change.VolumeDiff) string {
	out := ""
	for _, c := range d.Changes {
		p := c.Entry.Path
		if p == "" {
			p = c.Previous.Path
		}
		out += string(c.Kind) + ":" + p + " "
	}
	if out == "" {
		return "(no changes)"
	}
	return out
}

// TestOnlyFilesIntersectingWrittenRangesAreReported is the property the whole
// design rests on: a file untouched by the checkpoint must not appear, however
// many files the volume holds.
func TestOnlyFilesIntersectingWrittenRangesAreReported(t *testing.T) {
	from := newFake("ext4", vol).
		add(11, 1, "/keep.txt", 2, 100*blk, blk).
		add(12, 1, "/touched.txt", 2, 200*blk, blk)
	to := newFake("ext4", vol).
		add(11, 1, "/keep.txt", 2, 100*blk, blk).
		add(12, 1, "/touched.txt", 2, 200*blk, blk)

	d := diff(t, from, to, []vhdimap.ByteRange{r(200*blk, blk)}, nil)

	if len(d.Changes) != 1 {
		t.Fatalf("got %d changes, want 1: %s", len(d.Changes), summarise(d))
	}
	if got := d.Changes[0]; got.Entry.Path != "/touched.txt" || got.Kind != change.KindModified {
		t.Errorf("got %s %s, want modified /touched.txt", got.Kind, got.Entry.Path)
	}
}

func TestAddedFileIsReportedEvenWhereItsRangeWasAlreadyWritten(t *testing.T) {
	from := newFake("ext4", vol)
	to := newFake("ext4", vol).add(20, 1, "/new.bin", 2, 300*blk, 2*blk)

	d := diff(t, from, to, []vhdimap.ByteRange{r(300*blk, 2*blk)}, nil)

	c := find(t, d, "/new.bin")
	if c.Kind != change.KindAdded {
		t.Errorf("kind = %s, want added", c.Kind)
	}
	if !c.Modified {
		t.Error("Modified = false; a newly added file's blocks were written")
	}
}

// TestDeletionIsFoundAlthoughItsBlocksAreFreed is the case a purely
// block-guided search misses: a deleted file has no extents left to intersect,
// so it can only be found as a set difference.
func TestDeletionIsFoundAlthoughItsBlocksAreFreed(t *testing.T) {
	from := newFake("ext4", vol).add(30, 1, "/gone.txt", 2, 400*blk, blk)
	to := newFake("ext4", vol)

	// The written range is the directory block, not the file's former blocks.
	d := diff(t, from, to, []vhdimap.ByteRange{r(2*blk, blk)}, nil)

	c := find(t, d, "/gone.txt")
	if c.Kind != change.KindDeleted {
		t.Errorf("kind = %s, want deleted", c.Kind)
	}
	if c.Entry.ID != (vhdimap.FileID{}) {
		t.Error("a deleted file should carry no newer-state entry")
	}
	if c.Previous.Path != "/gone.txt" {
		t.Errorf("Previous.Path = %q, want /gone.txt", c.Previous.Path)
	}
}

func TestRenameIsSeenWhenIdentityIsKeptAndNameChanges(t *testing.T) {
	from := newFake("ext4", vol).add(40, 7, "/old-name.txt", 2, 500*blk, blk)
	to := newFake("ext4", vol).add(40, 7, "/new-name.txt", 2, 500*blk, blk)

	d := diff(t, from, to, []vhdimap.ByteRange{r(2*blk, blk)}, nil)

	c := find(t, d, "/new-name.txt")
	if c.Kind != change.KindRenamed {
		t.Fatalf("kind = %s, want renamed", c.Kind)
	}
	if c.Previous.Path != "/old-name.txt" {
		t.Errorf("Previous.Path = %q, want /old-name.txt", c.Previous.Path)
	}
	if c.Modified {
		t.Error("Modified = true; only the directory entry changed")
	}
}

// TestMoveBetweenDirectoriesIsARename pins the reason FileEntry carries the
// parent's identity rather than its name: a file moved between directories
// keeps its name, and a name comparison cannot see the move.
func TestMoveBetweenDirectoriesIsARename(t *testing.T) {
	from := newFake("ext4", vol).add(41, 3, "/a/report.doc", 100, 600*blk, blk)
	to := newFake("ext4", vol).add(41, 3, "/b/report.doc", 200, 600*blk, blk)

	d := diff(t, from, to, []vhdimap.ByteRange{r(600*blk, blk)}, nil)

	c := find(t, d, "/b/report.doc")
	if c.Kind != change.KindRenamed {
		t.Errorf("kind = %s, want renamed; the file moved directories", c.Kind)
	}
}

// TestReusedFileNumberIsNotReportedAsAModification is what the generation
// counter exists for. Without it, a deleted file whose inode slot is taken by
// an unrelated new file reads as the original file having been edited.
func TestReusedFileNumberIsNotReportedAsAModification(t *testing.T) {
	from := newFake("ext4", vol).add(50, 1, "/original.txt", 2, 700*blk, blk)
	to := newFake("ext4", vol).add(50, 2, "/unrelated.bin", 2, 700*blk, blk)

	d := diff(t, from, to, []vhdimap.ByteRange{r(700*blk, blk)}, nil)

	added := find(t, d, "/unrelated.bin")
	if added.Kind != change.KindAdded {
		t.Errorf("new file: kind = %s, want added", added.Kind)
	}
	deleted := find(t, d, "/original.txt")
	if deleted.Kind != change.KindDeleted {
		t.Errorf("old file: kind = %s, want deleted", deleted.Kind)
	}
	for _, c := range d.Changes {
		if c.Kind == change.KindModified {
			t.Error("a reused file number was reported as a modification")
		}
	}
}

// TestChangedRangesAreFileRelativeAcrossAHole pins the reason FileExtent
// carries its own file offset. Deriving the position by accumulating run
// lengths would place this change at offset 0 instead of past the hole.
func TestChangedRangesAreFileRelativeAcrossAHole(t *testing.T) {
	const holeLen = 8 * blk

	from := newFake("ext4", vol).addSparse(60, 1, "/sparse.bin", holeLen, 800*blk, 2*blk)
	to := newFake("ext4", vol).addSparse(60, 1, "/sparse.bin", holeLen, 800*blk, 2*blk)

	// Write the second block of the backed run: disk 801*blk, which is
	// holeLen+blk inside the file.
	d := diff(t, from, to, []vhdimap.ByteRange{r(801*blk, blk)}, nil)

	c := find(t, d, "/sparse.bin")
	if len(c.Ranges) != 1 {
		t.Fatalf("got %d ranges, want 1: %+v", len(c.Ranges), c.Ranges)
	}
	if want := holeLen + blk; c.Ranges[0].Offset != want {
		t.Errorf("range offset = %d, want %d (the hole occupies file space but no disk)",
			c.Ranges[0].Offset, want)
	}
	if c.Ranges[0].Length != blk {
		t.Errorf("range length = %d, want %d", c.Ranges[0].Length, blk)
	}
}

// TestHoleIsNeverReportedAsChanged: a sparse run occupies no disk, so no write
// can have landed in it.
func TestHoleIsNeverReportedAsChanged(t *testing.T) {
	from := newFake("ext4", vol).addSparse(61, 1, "/sparse.bin", 8*blk, 900*blk, blk)
	to := newFake("ext4", vol).addSparse(61, 1, "/sparse.bin", 8*blk, 900*blk, blk)

	// Offset 0 is inside the hole in file space and belongs to something else
	// entirely on disk.
	d := diff(t, from, to, []vhdimap.ByteRange{r(0, blk)}, nil)

	if len(d.Changes) != 0 {
		t.Errorf("got %s, want no changes: a hole cannot be written to", summarise(d))
	}
}

func TestComparingTwoDifferentVolumesIsRefused(t *testing.T) {
	from := newFake("ext4", "vol-uuid:aaaa").add(70, 1, "/f", 2, blk, blk)
	to := newFake("ext4", "vol-uuid:bbbb").add(70, 1, "/f", 2, blk, blk)

	_, err := change.Diff(context.Background(), from, to, []vhdimap.ByteRange{r(blk, blk)}, nil)
	if !errors.Is(err, change.ErrVolumeMismatch) {
		t.Fatalf("err = %v, want ErrVolumeMismatch", err)
	}
}

func TestVolumeMismatchCanBeOverriddenAndIsThenRecorded(t *testing.T) {
	from := newFake("ext4", "vol-uuid:aaaa").add(71, 1, "/f", 2, blk, blk)
	to := newFake("ext4", "vol-uuid:bbbb").add(71, 1, "/f", 2, blk, blk)

	d := diff(t, from, to, []vhdimap.ByteRange{r(blk, blk)},
		&change.DiffOptions{AllowVolumeIdentityMismatch: true})

	if len(d.Notes) == 0 {
		t.Fatal("no note recorded; an overridden mismatch must be visible in the result")
	}
}

// TestConfidenceFollowsTheFilesystemsOwnIdentity: a format that synthesises
// identity must not have its renames reported as though they were proven.
func TestConfidenceFollowsTheFilesystemsOwnIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stable   bool
		hasGen   bool
		want     change.Confidence
		wantNote bool
	}{
		{"ext with generation", true, true, change.ConfidenceIdentified, false},
		{"number but no reuse counter", true, false, change.ConfidenceInferred, true},
		{"synthesised identity", false, false, change.ConfidenceInferred, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from := newFake("fs", vol).add(80, 1, "/a.txt", 2, blk, blk)
			to := newFake("fs", vol).add(80, 1, "/b.txt", 2, blk, blk)
			from.caps.StableIdentity, from.caps.HasGeneration = tc.stable, tc.hasGen
			to.caps.StableIdentity, to.caps.HasGeneration = tc.stable, tc.hasGen

			d := diff(t, from, to, []vhdimap.ByteRange{r(blk, blk)}, nil)

			c := find(t, d, "/b.txt")
			if c.Confidence != tc.want {
				t.Errorf("confidence = %s, want %s", c.Confidence, tc.want)
			}
			if tc.wantNote && len(d.Notes) == 0 {
				t.Error("no note explaining why identity is weak on this format")
			}
		})
	}
}

// TestJournalPromotesARenameToProven exercises the corroboration path: the
// classification is the same, and what changes is what the result claims.
func TestJournalPromotesARenameToProven(t *testing.T) {
	id := vhdimap.FileID{Number: 90, Generation: 4}
	from := &journalFake{
		fakeFS: newFake("ntfs", vol).add(90, 4, "/before.txt", 5, blk, blk),
		state:  "usn:100",
	}
	to := &journalFake{
		fakeFS: newFake("ntfs", vol).add(90, 4, "/after.txt", 5, blk, blk),
		state:  "usn:200",
		renames: []vhdimap.RenameEvidence{{
			ID: id, OldName: "before.txt", NewName: "after.txt",
		}},
	}

	d := diff(t, from, to, []vhdimap.ByteRange{r(blk, blk)}, nil)

	c := find(t, d, "/after.txt")
	if c.Kind != change.KindRenamed {
		t.Fatalf("kind = %s, want renamed", c.Kind)
	}
	if c.Confidence != change.ConfidenceProven {
		t.Errorf("confidence = %s, want proven; the journal recorded the rename", c.Confidence)
	}
	if c.Evidence == nil {
		t.Fatal("Evidence is nil; a promoted rename must carry the record that promoted it")
	}
	if c.Evidence.OldName != "before.txt" {
		t.Errorf("Evidence.OldName = %q, want before.txt", c.Evidence.OldName)
	}
}

// TestWithoutAJournalTheSameRenameIsOnlyIdentified is the control for the test
// above: identical inputs minus the journal must produce a weaker claim.
func TestWithoutAJournalTheSameRenameIsOnlyIdentified(t *testing.T) {
	from := newFake("ntfs", vol).add(91, 4, "/before.txt", 5, blk, blk)
	to := newFake("ntfs", vol).add(91, 4, "/after.txt", 5, blk, blk)

	d := diff(t, from, to, []vhdimap.ByteRange{r(blk, blk)}, nil)

	c := find(t, d, "/after.txt")
	if c.Confidence != change.ConfidenceIdentified {
		t.Errorf("confidence = %s, want identified", c.Confidence)
	}
	if c.Evidence != nil {
		t.Error("Evidence is set although no journal was consulted")
	}
}

// TestAnUnreadableExtentMapIsNotedRatherThanTreatedAsUnchanged: failing to read
// a file's map is not evidence that the file did not change, and silence would
// be indistinguishable from it.
func TestAnUnreadableExtentMapIsNotedRatherThanTreatedAsUnchanged(t *testing.T) {
	id := vhdimap.FileID{Number: 100, Generation: 1}
	from := newFake("ext4", vol).add(100, 1, "/broken.bin", 2, blk, blk)
	to := newFake("ext4", vol).add(100, 1, "/broken.bin", 2, blk, blk)
	to.extentErr = map[vhdimap.FileID]error{id: errors.New("extent tree is corrupt")}

	d := diff(t, from, to, []vhdimap.ByteRange{r(blk, blk)}, nil)

	var found bool
	for _, n := range d.Notes {
		if contains(n, "/broken.bin") && contains(n, "corrupt") {
			found = true
		}
	}
	if !found {
		t.Errorf("no note about the unreadable map; notes were %q", d.Notes)
	}
}

func TestNothingWrittenReportsNoChangesAndSaysSo(t *testing.T) {
	from := newFake("ext4", vol).add(110, 1, "/f", 2, blk, blk)
	to := newFake("ext4", vol).add(110, 1, "/f", 2, blk, blk)

	d := diff(t, from, to, nil, nil)

	if len(d.Changes) != 0 {
		t.Errorf("got %s, want no changes", summarise(d))
	}
	if len(d.Notes) == 0 {
		t.Error("no note explaining the empty result")
	}
}

func TestSkipDirectoriesLeavesDirectoriesOut(t *testing.T) {
	from := newFake("ext4", vol).add(120, 1, "/dir", 2, blk, blk).markDir(120, 1)
	to := newFake("ext4", vol).add(120, 1, "/dir", 2, blk, blk).markDir(120, 1)

	if d := diff(t, from, to, []vhdimap.ByteRange{r(blk, blk)}, nil); len(d.Changes) != 1 {
		t.Fatalf("directories should be reported by default; got %s", summarise(d))
	}
	d := diff(t, from, to, []vhdimap.ByteRange{r(blk, blk)},
		&change.DiffOptions{SkipDirectories: true})
	if len(d.Changes) != 0 {
		t.Errorf("got %s, want no changes", summarise(d))
	}
}

func TestMaxFilesTruncatesAndSaysThatItDid(t *testing.T) {
	from, to := newFake("ext4", vol), newFake("ext4", vol)
	for i := 0; i < 10; i++ {
		n := uint64(200 + i)
		off := int64(1000+i) * blk
		from.add(n, 1, "/f"+string(rune('a'+i)), 2, off, blk)
		to.add(n, 2, "/f"+string(rune('a'+i)), 2, off, blk)
	}

	d := diff(t, from, to, []vhdimap.ByteRange{r(1000*blk, 100*blk)},
		&change.DiffOptions{MaxFiles: 3})

	if len(d.Changes) != 3 {
		t.Errorf("got %d changes, want 3", len(d.Changes))
	}
	var noted bool
	for _, n := range d.Notes {
		if contains(n, "truncated") {
			noted = true
		}
	}
	if !noted {
		t.Error("a truncated document must say so, or it reads as a complete one")
	}
}

func TestCancellationStopsTheDiff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	from := newFake("ext4", vol).add(300, 1, "/f", 2, blk, blk)
	to := newFake("ext4", vol).add(300, 1, "/f", 2, blk, blk)

	if _, err := change.Diff(ctx, from, to, []vhdimap.ByteRange{r(blk, blk)}, nil); err == nil {
		t.Fatal("Diff returned nil error on a cancelled context")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
