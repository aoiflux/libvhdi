// SPDX-License-Identifier: MIT

package types_test

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/aoiflux/libvhdi/types"
)

// VHD does not use the Unix epoch. Every timestamp in a footer or dynamic disk
// header counts seconds from 1 January 2000, so decoding one as a Unix
// timestamp lands it thirty years early. In a library whose output is evidence
// that is not a cosmetic error: an image's creation time is exactly the sort of
// fact a report exists to state.

// maxTimestamp is the last instant the unsigned 32-bit encoding can hold.
var maxTimestamp = time.Date(2136, time.February, 7, 6, 28, 15, 0, time.UTC)

func TestEpochIsTheYear2000(t *testing.T) {
	// The value itself, checked against the date rather than restated as the
	// same magic number the code uses.
	want := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	if !types.Epoch.Equal(want) {
		t.Fatalf("Epoch is %s, want %s", types.Epoch, want)
	}
	if got := types.Time(0xFFFFFFFF); !got.Equal(maxTimestamp) {
		t.Fatalf("Time(0xFFFFFFFF) = %s, want %s", got, maxTimestamp)
	}
}

func TestTimeDecodesFromTheYear2000(t *testing.T) {
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
			got := types.Time(tc.raw)
			if !got.Equal(tc.want) {
				t.Fatalf("Time(%d) = %s, want %s", tc.raw, got, tc.want)
			}
			if got.Location() != time.UTC {
				t.Fatalf("Time(%d) is in %s, want UTC", tc.raw, got.Location())
			}
		})
	}
}

func TestTimeIsNotTheUnixEpoch(t *testing.T) {
	// The regression this guards. Reading the raw value as a Unix timestamp
	// yields a time thirty years earlier, and the two readings differ by
	// exactly the epoch offset.
	const raw = 757382400

	viaVHD := types.Time(raw)
	viaUnix := time.Unix(raw, 0).UTC()

	if viaVHD.Equal(viaUnix) {
		t.Fatal("the VHD epoch and the Unix epoch produced the same time; the offset is not applied")
	}
	if diff := viaVHD.Sub(viaUnix); diff != types.Epoch.Sub(time.Unix(0, 0)) {
		t.Fatalf("the two readings differ by %s, want the epoch offset", diff)
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
	if got := types.Time(0); !got.IsZero() {
		t.Fatalf("Time(0) = %s, want the zero Time", got)
	}
	// The cost: the epoch instant itself cannot be recorded as present.
	if got := types.Timestamp(types.Epoch); got != 0 {
		t.Fatalf("Timestamp(Epoch) = %d, want 0", got)
	}
}

func TestTimestampRoundTripsFrom2000To2136(t *testing.T) {
	cases := []time.Time{
		time.Date(2000, time.January, 1, 0, 0, 1, 0, time.UTC),
		time.Date(2013, time.June, 7, 13, 45, 3, 0, time.UTC),
		time.Date(2024, time.December, 31, 23, 59, 59, 0, time.UTC),
		maxTimestamp,
	}
	for year := 2001; year <= 2136; year++ {
		cases = append(cases, time.Date(year, time.January, 1, 12, 34, 56, 0, time.UTC))
	}

	for _, want := range cases {
		if got := types.Time(types.Timestamp(want)); !got.Equal(want) {
			t.Errorf("round trip of %s gave %s", want, got)
		}
	}
}

func TestTimestampIgnoresZoneAndTruncatesSubseconds(t *testing.T) {
	utc := time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC)
	zoned := utc.In(time.FixedZone("UTC+5:30", 5*3600+1800)).Add(999 * time.Millisecond)
	if got, want := types.Timestamp(zoned), types.Timestamp(utc); got != want {
		t.Fatalf("Timestamp(%s) = %d, want %d", zoned, got, want)
	}
}

func TestTimestampClampsRatherThanWraps(t *testing.T) {
	// The encoding is unsigned and 32 bits wide. Wrapping would turn a time
	// before the epoch into one in 2136, and one after 2136 into one in 2000.
	for _, tc := range []struct {
		name string
		in   time.Time
		want uint32
	}{
		{"zero time", time.Time{}, 0},
		{"before the epoch", time.Date(1999, time.December, 31, 0, 0, 0, 0, time.UTC), 0},
		{"the epoch", types.Epoch, 0},
		{"the last representable second", maxTimestamp, 0xFFFFFFFF},
		{"one second later", maxTimestamp.Add(time.Second), 0xFFFFFFFF},
		{"far later", time.Date(3000, time.January, 1, 0, 0, 0, 0, time.UTC), 0xFFFFFFFF},
	} {
		if got := types.Timestamp(tc.in); got != tc.want {
			t.Errorf("%s: Timestamp(%s) = %#x, want %#x", tc.name, tc.in, got, tc.want)
		}
	}
}

// byteSum is the specification's checksum written independently of the code
// under test.
func byteSum(b []byte) uint32 {
	var s uint32
	for _, c := range b {
		s += uint32(c)
	}
	return ^s
}

func TestChecksumIsTheOnesComplementByteSum(t *testing.T) {
	for _, b := range [][]byte{
		nil,
		{0x00},
		{0xFF, 0xFF},
		bytes.Repeat([]byte{0xA5}, 512),
		[]byte("conectix"),
	} {
		if got, want := types.Checksum(b), byteSum(b); got != want {
			t.Errorf("Checksum(% x...) = %#08x, want %#08x", b[:min(len(b), 4)], got, want)
		}
	}
	if got := types.Checksum(nil); got != 0xFFFFFFFF {
		t.Fatalf("Checksum(nil) = %#08x, want 0xFFFFFFFF", got)
	}
}

// sampleFooter fills every field with a distinct value, so a field written to
// the wrong offset cannot go unnoticed.
func sampleFooter() *types.FileFooter {
	f := &types.FileFooter{
		Features:           types.FeatureReserved,
		FormatVersion:      types.FormatVersion1,
		NextOffset:         types.FixedDataOffset,
		ModificationTime:   types.Timestamp(time.Date(2026, time.October, 11, 9, 30, 0, 0, time.UTC)),
		CreatorApplication: 0x66736167, // "fsag"
		CreatorVersion:     0x00040000,
		CreatorOS:          types.CreatorOSWindows,
		OriginalSize:       512 << 20,
		CurrentSize:        1 << 30,
		DiskGeometry:       types.PackGeometry(2080, 16, 63),
		DiskType:           uint32(types.DiskTypeFixed),
		Identifier:         [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SavedState:         1,
	}
	copy(f.Signature[:], types.VHDFooterSignature)
	f.Reserved[0] = 0xEE
	f.Reserved[426] = 0xDD
	return f
}

func TestMarshalBinaryWritesEveryFieldAtItsSpecOffset(t *testing.T) {
	f := sampleFooter()
	f.Checksum = 0xCAFEBABE

	b, err := f.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(b) != types.VHDFooterSize {
		t.Fatalf("MarshalBinary returned %d bytes, want %d", len(b), types.VHDFooterSize)
	}

	// Offsets from the specification's Hard Disk Footer Format table, not
	// from the code.
	be := binary.BigEndian
	for _, c := range []struct {
		field  string
		got    uint64
		want   uint64
		offset int
	}{
		{"Features", uint64(be.Uint32(b[8:])), uint64(f.Features), 8},
		{"File Format Version", uint64(be.Uint32(b[12:])), 0x00010000, 12},
		{"Data Offset", be.Uint64(b[16:]), 0xFFFFFFFFFFFFFFFF, 16},
		{"Time Stamp", uint64(be.Uint32(b[24:])), uint64(f.ModificationTime), 24},
		{"Creator Application", uint64(be.Uint32(b[28:])), 0x66736167, 28},
		{"Creator Version", uint64(be.Uint32(b[32:])), 0x00040000, 32},
		{"Creator Host OS", uint64(be.Uint32(b[36:])), 0x5769326B, 36},
		{"Original Size", be.Uint64(b[40:]), 512 << 20, 40},
		{"Current Size", be.Uint64(b[48:]), 1 << 30, 48},
		{"Disk Geometry", uint64(be.Uint32(b[56:])), 2080<<16 | 16<<8 | 63, 56},
		{"Disk Type", uint64(be.Uint32(b[60:])), 2, 60},
		{"Checksum", uint64(be.Uint32(b[64:])), 0xCAFEBABE, 64},
		{"Saved State", uint64(b[84]), 1, 84},
		{"Reserved[0]", uint64(b[85]), 0xEE, 85},
		{"Reserved[426]", uint64(b[511]), 0xDD, 511},
	} {
		if c.got != c.want {
			t.Errorf("%s at offset %d = %#x, want %#x", c.field, c.offset, c.got, c.want)
		}
	}
	if string(b[0:8]) != "conectix" {
		t.Errorf("Cookie = %q, want %q", b[0:8], "conectix")
	}
	if !bytes.Equal(b[68:84], f.Identifier[:]) {
		t.Errorf("Unique Id = % x, want % x", b[68:84], f.Identifier)
	}
	if string(b[36:40]) != "Wi2k" {
		t.Errorf("CreatorOSWindows encodes as %q, want %q", b[36:40], "Wi2k")
	}
}

func TestFooterChecksumIgnoresTheStoredChecksum(t *testing.T) {
	f := sampleFooter()

	f.Checksum = 0
	zeroed, _ := f.MarshalBinary()
	want := byteSum(zeroed)

	for _, stored := range []uint32{0, 1, 0xFFFFFFFF, want} {
		f.Checksum = stored
		if got := types.FooterChecksum(f); got != want {
			t.Errorf("with Checksum %#x stored, FooterChecksum = %#08x, want %#08x", stored, got, want)
		}
		if f.Checksum != stored {
			t.Fatalf("FooterChecksum modified its argument's Checksum to %#x", f.Checksum)
		}
	}
}

func TestFooterChecksumSealsTheMarshalledBytes(t *testing.T) {
	// With the checksum stored, the byte sum of the whole structure excluding
	// the checksum field complements to the stored value: the property a
	// reader verifies.
	f := sampleFooter()
	f.Checksum = types.FooterChecksum(f)
	b, _ := f.MarshalBinary()

	stored := binary.BigEndian.Uint32(b[64:68])
	clear(b[64:68])
	if got := byteSum(b); got != stored {
		t.Fatalf("stored checksum %#08x, recomputed %#08x", stored, got)
	}
}
