// SPDX-License-Identifier: MIT

package vhdimap_test

import (
	"testing"

	"github.com/aoiflux/libvhdi/vhdimap"
)

func rng(offset, length int64) vhdimap.ByteRange {
	return vhdimap.ByteRange{Offset: offset, Length: length}
}

func equal(a, b []vhdimap.ByteRange) bool {
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

func TestCoalesce(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []vhdimap.ByteRange
		want []vhdimap.ByteRange
	}{
		{"empty", nil, nil},
		{
			"unsorted input is sorted",
			[]vhdimap.ByteRange{rng(200, 10), rng(0, 10)},
			[]vhdimap.ByteRange{rng(0, 10), rng(200, 10)},
		},
		{
			"overlapping ranges merge",
			[]vhdimap.ByteRange{rng(0, 100), rng(50, 100)},
			[]vhdimap.ByteRange{rng(0, 150)},
		},
		{
			"abutting ranges merge",
			[]vhdimap.ByteRange{rng(0, 100), rng(100, 100)},
			[]vhdimap.ByteRange{rng(0, 200)},
		},
		{
			"a range wholly inside another is absorbed",
			[]vhdimap.ByteRange{rng(0, 100), rng(20, 5)},
			[]vhdimap.ByteRange{rng(0, 100)},
		},
		{
			"a one-byte gap is preserved",
			[]vhdimap.ByteRange{rng(0, 100), rng(101, 10)},
			[]vhdimap.ByteRange{rng(0, 100), rng(101, 10)},
		},
		{
			"zero-length ranges are dropped",
			[]vhdimap.ByteRange{rng(0, 0), rng(10, 5), rng(90, 0)},
			[]vhdimap.ByteRange{rng(10, 5)},
		},
		{
			"negative lengths are dropped",
			[]vhdimap.ByteRange{rng(0, -5), rng(10, 5)},
			[]vhdimap.ByteRange{rng(10, 5)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vhdimap.Coalesce(tc.in); !equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCoalesceDoesNotModifyItsInput: the changed-range set is reused across
// every volume on a disk, so coalescing it must not be destructive.
func TestCoalesceDoesNotModifyItsInput(t *testing.T) {
	in := []vhdimap.ByteRange{rng(200, 10), rng(0, 10), rng(5, 10)}
	before := append([]vhdimap.ByteRange(nil), in...)

	vhdimap.Coalesce(in)

	if !equal(in, before) {
		t.Errorf("input became %v, was %v", in, before)
	}
}

func TestIntersects(t *testing.T) {
	set := vhdimap.Coalesce([]vhdimap.ByteRange{rng(100, 100), rng(400, 100)})

	for _, tc := range []struct {
		name string
		r    vhdimap.ByteRange
		want bool
	}{
		{"before everything", rng(0, 100), false},
		{"touching the start edge", rng(0, 101), true},
		{"wholly inside", rng(120, 10), true},
		{"in the gap", rng(200, 200), false},
		{"abutting the end, exclusive", rng(200, 10), false},
		{"spanning both ranges", rng(0, 1000), true},
		{"after everything", rng(500, 100), false},
		{"zero length", rng(120, 0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vhdimap.Intersects(set, tc.r); got != tc.want {
				t.Errorf("Intersects(%v) = %v, want %v", tc.r, got, tc.want)
			}
		})
	}
}

func TestIntersectsOnAnEmptySet(t *testing.T) {
	if vhdimap.Intersects(nil, rng(0, 100)) {
		t.Error("an empty set intersected something")
	}
}

func TestOverlap(t *testing.T) {
	set := vhdimap.Coalesce([]vhdimap.ByteRange{rng(100, 100), rng(400, 100)})

	for _, tc := range []struct {
		name string
		r    vhdimap.ByteRange
		want []vhdimap.ByteRange
	}{
		{"no overlap", rng(0, 50), nil},
		{"clipped at the start", rng(50, 100), []vhdimap.ByteRange{rng(100, 50)}},
		{"clipped at the end", rng(150, 100), []vhdimap.ByteRange{rng(150, 50)}},
		{
			"spanning both, with the gap excluded",
			rng(0, 1000),
			[]vhdimap.ByteRange{rng(100, 100), rng(400, 100)},
		},
		{"entirely within the gap", rng(250, 50), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vhdimap.Overlap(set, tc.r); !equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIntersectsAgreesWithOverlap: the fast predicate and the version that
// keeps the answer must never disagree, since the diff uses one to decide
// whether to call the other.
func TestIntersectsAgreesWithOverlap(t *testing.T) {
	set := vhdimap.Coalesce([]vhdimap.ByteRange{
		rng(0, 10), rng(50, 10), rng(1000, 1), rng(4096, 4096),
	})

	for offset := int64(-5); offset < 9000; offset += 3 {
		for _, length := range []int64{1, 7, 64, 4095} {
			r := rng(offset, length)
			if vhdimap.Intersects(set, r) != (len(vhdimap.Overlap(set, r)) > 0) {
				t.Fatalf("disagreement at %v", r)
			}
		}
	}
}

// TestFileExtentSparseIsAHoleNotAnEmptyExtent. A hole occupies file offsets and
// no disk, and it is reported rather than elided so that the runs after it keep
// their positions -- accumulating lengths instead would shift every offset
// after the first hole.
func TestFileExtentSparseIsAHoleNotAnEmptyExtent(t *testing.T) {
	hole := vhdimap.FileExtent{FileOffset: 0}
	backed := vhdimap.FileExtent{
		ByteRange:  rng(4096, 4096),
		FileOffset: 8192,
	}

	if !hole.Sparse() {
		t.Error("an extent with no disk range is not reported as sparse")
	}
	if backed.Sparse() {
		t.Error("a backed extent is reported as sparse")
	}
	if backed.FileOffset == backed.Offset {
		t.Error("the fixture no longer distinguishes the two axes")
	}
}

func TestByteRangeIntersect(t *testing.T) {
	a := rng(100, 100)

	if got, ok := a.Intersect(rng(150, 100)); !ok || got != rng(150, 50) {
		t.Errorf("overlap = %v (%v), want {150 50}", got, ok)
	}
	if _, ok := a.Intersect(rng(200, 10)); ok {
		t.Error("abutting ranges reported an intersection; End is exclusive")
	}
	if _, ok := a.Intersect(rng(0, 100)); ok {
		t.Error("abutting ranges reported an intersection at the low edge")
	}
}
