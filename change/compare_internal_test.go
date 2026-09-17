// SPDX-License-Identifier: MIT

package change

import (
	"testing"

	"github.com/aoiflux/libvhdi"
	"github.com/aoiflux/libvhdi/report"
	"github.com/aoiflux/libvhdi/vhdimap"
)

func br(offset, length int64) vhdimap.ByteRange {
	return vhdimap.ByteRange{Offset: offset, Length: length}
}

func sameRanges(a, b []vhdimap.ByteRange) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAClearedRangeCountsAsWritten. Zeroing a region is how a deletion shows up
// on a VHDX chain, so filtering on "mapped" alone under-reports deletions --
// which is the whole reason ExtentZeroedByChild exists.
func TestAClearedRangeCountsAsWritten(t *testing.T) {
	got := writtenRanges([]libvhdi.Extent{
		{VirtualOffset: 0, Length: 4096, Kind: libvhdi.ExtentMapped},
		{VirtualOffset: 8192, Length: 4096, Kind: libvhdi.ExtentZeroedByChild},
	})

	want := []vhdimap.ByteRange{br(0, 4096), br(8192, 4096)}
	if !sameRanges(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestAnUnresolvedRangeIsNotTreatedAsWritten. The image that would say what
// happened is missing, so calling it written invents changes; calling it
// unchanged hides them. It is neither, and it is left out of the file-level
// pass while the block-level totals still account for it.
func TestAnUnresolvedRangeIsNotTreatedAsWritten(t *testing.T) {
	got := writtenRanges([]libvhdi.Extent{
		{VirtualOffset: 0, Length: 4096, Kind: libvhdi.ExtentUnresolved},
	})

	if len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
}

// TestAParentsUnwrittenZeroesAreNotAChange: ExtentZero means nothing ever wrote
// there, as distinct from a child explicitly clearing it.
func TestAParentsUnwrittenZeroesAreNotAChange(t *testing.T) {
	got := writtenRanges([]libvhdi.Extent{
		{VirtualOffset: 0, Length: 4096, Kind: libvhdi.ExtentZero},
	})

	if len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
}

func TestWrittenRangesAreCoalesced(t *testing.T) {
	got := writtenRanges([]libvhdi.Extent{
		{VirtualOffset: 4096, Length: 4096, Kind: libvhdi.ExtentMapped},
		{VirtualOffset: 0, Length: 4096, Kind: libvhdi.ExtentMapped},
	})

	if want := []vhdimap.ByteRange{br(0, 8192)}; !sameRanges(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestClipKeepsOnlyWhatFallsInsideTheVolume is what stops a change in one
// partition being attributed to a file at the same offset in another.
func TestClipKeepsOnlyWhatFallsInsideTheVolume(t *testing.T) {
	const mib = int64(1) << 20
	span := br(mib, mib) // a volume covering [1 MiB, 2 MiB)

	got := clip([]vhdimap.ByteRange{
		br(0, 4096),          // before the volume
		br(mib, 4096),        // at its first byte
		br(mib+4096, 4096),   // inside
		br(2*mib, 4096),      // just past its last byte
		br(mib-2048, 4096),   // straddling the start
		br(2*mib-2048, 4096), // straddling the end
	}, span)

	want := []vhdimap.ByteRange{
		br(mib, 4096),
		br(mib+4096, 4096),
		br(mib, 2048),
		br(2*mib-2048, 2048),
	}
	if !sameRanges(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTierReflectsTheStrongestEvidence. Labelling a document by its weakest
// volume would understate a journal-corroborated NTFS result because another
// volume on the same disk had no journal.
func TestTierReflectsTheStrongestEvidence(t *testing.T) {
	inferredOnly := []report.ChangedVolume{
		{Filesystem: "vfat", Files: []report.ChangedFile{{Confidence: string(ConfidenceInferred)}}},
	}
	if got := tierOf(inferredOnly); got != report.TierFile {
		t.Errorf("tier = %q, want %q", got, report.TierFile)
	}

	mixed := []report.ChangedVolume{
		{Filesystem: "vfat", Files: []report.ChangedFile{{Confidence: string(ConfidenceInferred)}}},
		{Filesystem: "ntfs", Files: []report.ChangedFile{{Confidence: string(ConfidenceProven)}}},
	}
	if got := tierOf(mixed); got != report.TierJournal {
		t.Errorf("tier = %q, want %q; one proven finding raises the document", got, report.TierJournal)
	}
}

// TestADeletedFilesReportEntryNamesTheFileItLost. The newer-state entry is empty
// for a deletion, so the report has to fall back to the older one -- otherwise
// every deletion is reported against an empty path.
func TestADeletedFilesReportEntryNamesTheFileItLost(t *testing.T) {
	v := &Volume{Base: 1 << 20, PartitionName: "Basic data partition"}
	diff := &VolumeDiff{
		Capabilities: vhdimap.Capabilities{Name: "ntfs"},
		Changes: []FileChange{{
			Kind: KindDeleted,
			Previous: vhdimap.FileEntry{
				ID:   vhdimap.FileID{Number: 42, Generation: 3},
				Path: "/Users/someone/secret.txt",
				Size: 1024,
			},
			Confidence: ConfidenceIdentified,
		}},
	}

	got := toReport(v, diff)

	if len(got.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(got.Files))
	}
	f := got.Files[0]
	if f.Path != "/Users/someone/secret.txt" {
		t.Errorf("Path = %q, want the deleted file's path", f.Path)
	}
	if f.FileNumber != 42 || f.Generation != 3 {
		t.Errorf("identity = %d.%d, want 42.3", f.FileNumber, f.Generation)
	}
	if f.Size != 1024 {
		t.Errorf("Size = %d, want 1024", f.Size)
	}
	if got.BaseOffset != 1<<20 {
		t.Errorf("BaseOffset = %d, want %d", got.BaseOffset, 1<<20)
	}
}

// TestARenameCarriesBothPaths: a rename with no previous path is a rename the
// reader cannot act on.
func TestARenameCarriesBothPaths(t *testing.T) {
	diff := &VolumeDiff{
		Capabilities: vhdimap.Capabilities{Name: "ext4"},
		Changes: []FileChange{{
			Kind:       KindRenamed,
			Entry:      vhdimap.FileEntry{ID: vhdimap.FileID{Number: 7, Generation: 1}, Path: "/after"},
			Previous:   vhdimap.FileEntry{ID: vhdimap.FileID{Number: 7, Generation: 1}, Path: "/before"},
			Confidence: ConfidenceIdentified,
		}},
	}

	got := toReport(&Volume{}, diff)

	f := got.Files[0]
	if f.Path != "/after" || f.PreviousPath != "/before" {
		t.Errorf("got %q (was %q), want /after (was /before)", f.Path, f.PreviousPath)
	}
	if f.Confidence != string(ConfidenceIdentified) {
		t.Errorf("Confidence = %q; dropping it would let an inference read as proof", f.Confidence)
	}
}
