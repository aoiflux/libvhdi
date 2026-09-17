// SPDX-License-Identifier: MIT

package reader

import (
	"testing"
	"time"
)

// VHD does not use the Unix epoch. Every timestamp in a footer or dynamic disk
// header counts seconds from 1 January 2000, so decoding one as a Unix
// timestamp lands it thirty years early. In a library whose output is evidence
// that is not a cosmetic error: an image's creation time is exactly the sort of
// fact a report exists to state.

func TestVHDEpochIsTheYear2000(t *testing.T) {
	// The constant itself, checked against the date rather than restated as the
	// same magic number the code uses.
	want := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	if got := time.Unix(vhdEpochUnix, 0).UTC(); !got.Equal(want) {
		t.Fatalf("vhdEpochUnix is %s, want %s", got, want)
	}
}

func TestVHDTimestampDecodesFromTheYear2000(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  uint32
		want time.Time
	}{
		{"one second after the epoch", 1, time.Date(2000, time.January, 1, 0, 0, 1, 0, time.UTC)},
		{"a day after the epoch", 86400, time.Date(2000, time.January, 2, 0, 0, 0, 0, time.UTC)},
		{"a realistic creation time", 757382400, time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := vhdTimestamp(tc.raw)
			if !got.Equal(tc.want) {
				t.Fatalf("vhdTimestamp(%d) = %s, want %s", tc.raw, got, tc.want)
			}
		})
	}
}

func TestVHDTimestampIsNotTheUnixEpoch(t *testing.T) {
	// The regression this guards. Reading the raw value as a Unix timestamp
	// yields a time thirty years earlier, and the two readings differ by
	// exactly the epoch offset.
	const raw = 757382400

	viaVHD := vhdTimestamp(raw)
	viaUnix := time.Unix(raw, 0).UTC()

	if viaVHD.Equal(viaUnix) {
		t.Fatal("the VHD epoch and the Unix epoch produced the same time; the offset is not applied")
	}
	if diff := viaVHD.Sub(viaUnix); diff != time.Duration(vhdEpochUnix)*time.Second {
		t.Fatalf("the two readings differ by %s, want %d seconds", diff, vhdEpochUnix)
	}
	if viaUnix.Year() != 1994 || viaVHD.Year() != 2024 {
		t.Fatalf("expected 1994 under the Unix reading and 2024 under the VHD one; got %d and %d",
			viaUnix.Year(), viaVHD.Year())
	}
}

func TestZeroTimestampIsAbsentRatherThanTheEpoch(t *testing.T) {
	// A zero field means the producer recorded no timestamp -- which is what a
	// non-differencing disk's parent modification time always is. Rendering
	// that as midnight on 1 January 2000 would invent a fact, and a report
	// stating a precise creation time for an image that never carried one is
	// worse than one stating none.
	if got := vhdTimestamp(0); !got.IsZero() {
		t.Fatalf("vhdTimestamp(0) = %s, want the zero Time", got)
	}
}

func TestVHDTimestampRawRoundTrips(t *testing.T) {
	for _, want := range []time.Time{
		time.Date(2000, time.January, 1, 0, 0, 1, 0, time.UTC),
		time.Date(2013, time.June, 7, 13, 45, 3, 0, time.UTC),
		time.Date(2024, time.December, 31, 23, 59, 59, 0, time.UTC),
	} {
		if got := vhdTimestamp(vhdTimestampRaw(want)); !got.Equal(want) {
			t.Errorf("round trip of %s gave %s", want, got)
		}
	}
}

func TestVHDTimestampRawClampsTimesBeforeTheEpoch(t *testing.T) {
	// The encoding is unsigned, so it cannot represent anything earlier than
	// its epoch. Wrapping would produce a timestamp in the year 2136.
	if got := vhdTimestampRaw(time.Date(1999, time.December, 31, 0, 0, 0, 0, time.UTC)); got != 0 {
		t.Fatalf("vhdTimestampRaw(1999) = %d, want 0", got)
	}
	if got := vhdTimestampRaw(time.Time{}); got != 0 {
		t.Fatalf("vhdTimestampRaw(zero) = %d, want 0", got)
	}
}
