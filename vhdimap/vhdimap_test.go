// SPDX-License-Identifier: MIT

package vhdimap_test

import (
	"context"
	"testing"

	"github.com/aoiflux/libvhdi/vhdimap"
)

// This package declares a contract and implements none of it, so its tests do
// two things: check the range arithmetic every consumer will lean on, and prove
// the interfaces are satisfiable by something that is not a filesystem at all.
// The second is the point of the design -- if only a real parser could
// implement these, the diff engine could not be tested without a disk image.

// ---------------------------------------------------------------------------
// Range arithmetic
// ---------------------------------------------------------------------------

func TestOverlapsIsHalfOpen(t *testing.T) {
	// Ranges are half-open, so touching is not overlapping. Getting this wrong
	// by one byte makes every adjacent file look like a candidate for every
	// change, which quietly destroys the performance the whole approach rests on.
	a := vhdimap.ByteRange{Offset: 0, Length: 100}
	b := vhdimap.ByteRange{Offset: 100, Length: 100}

	if a.Overlaps(b) || b.Overlaps(a) {
		t.Fatal("adjacent ranges report as overlapping")
	}

	c := vhdimap.ByteRange{Offset: 99, Length: 100}
	if !a.Overlaps(c) || !c.Overlaps(a) {
		t.Fatal("ranges sharing one byte do not report as overlapping")
	}
}

func TestIntersect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		a, b   vhdimap.ByteRange
		want   vhdimap.ByteRange
		wantOK bool
	}{
		{
			name:   "partial overlap",
			a:      vhdimap.ByteRange{Offset: 0, Length: 100},
			b:      vhdimap.ByteRange{Offset: 50, Length: 100},
			want:   vhdimap.ByteRange{Offset: 50, Length: 50},
			wantOK: true,
		},
		{
			name:   "contained",
			a:      vhdimap.ByteRange{Offset: 0, Length: 100},
			b:      vhdimap.ByteRange{Offset: 20, Length: 10},
			want:   vhdimap.ByteRange{Offset: 20, Length: 10},
			wantOK: true,
		},
		{
			name:   "identical",
			a:      vhdimap.ByteRange{Offset: 10, Length: 10},
			b:      vhdimap.ByteRange{Offset: 10, Length: 10},
			want:   vhdimap.ByteRange{Offset: 10, Length: 10},
			wantOK: true,
		},
		{
			name: "adjacent",
			a:    vhdimap.ByteRange{Offset: 0, Length: 100},
			b:    vhdimap.ByteRange{Offset: 100, Length: 100},
		},
		{
			name: "disjoint",
			a:    vhdimap.ByteRange{Offset: 0, Length: 10},
			b:    vhdimap.ByteRange{Offset: 500, Length: 10},
		},
		{
			name: "empty range never intersects",
			a:    vhdimap.ByteRange{Offset: 50, Length: 0},
			b:    vhdimap.ByteRange{Offset: 0, Length: 100},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.a.Intersect(tc.b)
			if ok != tc.wantOK {
				t.Fatalf("Intersect ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("Intersect = %+v, want %+v", got, tc.want)
			}

			// Intersection is symmetric, and a diff engine will rely on that
			// when it walks whichever side is shorter.
			mirrored, mirroredOK := tc.b.Intersect(tc.a)
			if mirroredOK != ok || mirrored != got {
				t.Fatalf("Intersect is not symmetric: %+v/%v vs %+v/%v",
					got, ok, mirrored, mirroredOK)
			}
		})
	}
}

func TestEnd(t *testing.T) {
	r := vhdimap.ByteRange{Offset: 4096, Length: 512}
	if got := r.End(); got != 4608 {
		t.Fatalf("End() = %d, want 4608", got)
	}
}

func TestFileIDZero(t *testing.T) {
	// A zero generation is legitimate on a filesystem that has one, so IsZero
	// has to require both halves unset. Treating generation zero as "no
	// identity" would discard every file with a fresh inode.
	if !(vhdimap.FileID{}).IsZero() {
		t.Error("the zero FileID does not report as zero")
	}
	if (vhdimap.FileID{Number: 5, Generation: 0}).IsZero() {
		t.Error("a file with generation 0 reports as having no identity")
	}
	if (vhdimap.FileID{Number: 0, Generation: 7}).IsZero() {
		t.Error("a file with number 0 reports as having no identity")
	}
}

// ---------------------------------------------------------------------------
// The contract is satisfiable without a filesystem
// ---------------------------------------------------------------------------

// fakeFS implements Filesystem over a map. It is what lets a diff engine be
// tested without a disk image: if the interfaces demanded anything only a real
// parser could supply, this type could not exist.
type fakeFS struct {
	files   map[vhdimap.FileID]vhdimap.FileEntry
	extents map[vhdimap.FileID][]vhdimap.ByteRange
}

func (f *fakeFS) Capabilities() vhdimap.Capabilities {
	return vhdimap.Capabilities{Name: "fake", StableIdentity: true, HasGeneration: true}
}

func (f *fakeFS) VolumeIdentity(context.Context) (string, error) { return "fake-volume", nil }

func (f *fakeFS) WalkFiles(ctx context.Context, fn func(vhdimap.FileEntry) error) error {
	for _, e := range f.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeFS) FileByID(_ context.Context, id vhdimap.FileID) (vhdimap.FileEntry, error) {
	e, ok := f.files[id]
	if !ok {
		return vhdimap.FileEntry{}, vhdimap.ErrNotSupported
	}
	return e, nil
}

func (f *fakeFS) ExtentsForFile(_ context.Context, id vhdimap.FileID) ([]vhdimap.ByteRange, error) {
	return f.extents[id], nil
}

// journalFS additionally implements Journal, which is how a caller learns at
// compile time that renames can be proven rather than inferred.
type journalFS struct{ fakeFS }

func (j *journalFS) VolumeState(context.Context) (string, error) { return "state-1", nil }

func (j *journalFS) RenamesBetween(context.Context, string, string) ([]vhdimap.RenameEvidence, error) {
	return nil, nil
}

// fakeSource stands in for libvhdi's changed ranges, so the diff engine can be
// exercised with no virtual disk at all.
type fakeSource struct{ ranges []vhdimap.ByteRange }

func (s *fakeSource) ChangedRanges(context.Context) ([]vhdimap.ByteRange, error) {
	return s.ranges, nil
}

var (
	_ vhdimap.Filesystem         = (*fakeFS)(nil)
	_ vhdimap.Filesystem         = (*journalFS)(nil)
	_ vhdimap.Journal            = (*journalFS)(nil)
	_ vhdimap.ChangedBlockSource = (*fakeSource)(nil)
)

func TestOptionalCapabilitiesAreVisibleAtCompileTime(t *testing.T) {
	// The reason Journal is a separate interface rather than a method returning
	// ErrNotSupported: a type assertion answers the question, so a partial
	// implementation is a fact the caller can see rather than a runtime surprise.
	var plain vhdimap.Filesystem = &fakeFS{}
	if _, ok := plain.(vhdimap.Journal); ok {
		t.Error("a filesystem with no journal satisfies Journal")
	}

	var withJournal vhdimap.Filesystem = &journalFS{}
	if _, ok := withJournal.(vhdimap.Journal); !ok {
		t.Error("a filesystem with a journal does not satisfy Journal")
	}

	// Nothing implements OwnerIndex today, and nothing needs to -- a caller
	// builds the same index from WalkFiles and ExtentsForFile. It exists so a
	// parser that can do better is not forced to pretend it cannot.
	if _, ok := withJournal.(vhdimap.OwnerIndex); ok {
		t.Error("the fake unexpectedly satisfies OwnerIndex")
	}
}

func TestTheCandidateSetIsTheWholePoint(t *testing.T) {
	// A miniature of the real algorithm: intersect the ranges a checkpoint
	// wrote with each file's extents, and examine only the files that overlap.
	// Everything else is untouched by construction.
	a := vhdimap.FileID{Number: 1, Generation: 1}
	b := vhdimap.FileID{Number: 2, Generation: 1}
	c := vhdimap.FileID{Number: 3, Generation: 1}

	fs := &fakeFS{
		files: map[vhdimap.FileID]vhdimap.FileEntry{
			a: {ID: a, Name: "changed.txt"},
			b: {ID: b, Name: "untouched.txt"},
			c: {ID: c, Name: "fragmented.bin"},
		},
		extents: map[vhdimap.FileID][]vhdimap.ByteRange{
			a: {{Offset: 0, Length: 4096}},
			b: {{Offset: 1 << 20, Length: 4096}},
			// A fragmented file where only the second fragment was written.
			c: {{Offset: 2 << 20, Length: 4096}, {Offset: 8192, Length: 4096}},
		},
	}

	source := &fakeSource{ranges: []vhdimap.ByteRange{
		{Offset: 0, Length: 16384},
	}}

	ctx := context.Background()
	changed, err := source.ChangedRanges(ctx)
	if err != nil {
		t.Fatalf("ChangedRanges: %v", err)
	}

	candidates := map[vhdimap.FileID]bool{}
	err = fs.WalkFiles(ctx, func(e vhdimap.FileEntry) error {
		runs, err := fs.ExtentsForFile(ctx, e.ID)
		if err != nil {
			return err
		}
		for _, run := range runs {
			for _, ch := range changed {
				if run.Overlaps(ch) {
					candidates[e.ID] = true
					return nil
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}

	if !candidates[a] {
		t.Error("the changed file is not in the candidate set")
	}
	if !candidates[c] {
		t.Error("a file whose second fragment was written is not in the candidate set")
	}
	if candidates[b] {
		t.Error("an untouched file is in the candidate set")
	}
	if len(candidates) != 2 {
		t.Fatalf("candidate set has %d files, want 2", len(candidates))
	}
}
