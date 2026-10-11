// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"testing"
	"time"

	"github.com/aoiflux/libvhdi/types"
)

// The types package exports the VHD footer codecs for tools and tests that
// produce footers of their own. These tests hold the codecs and this package's
// parser to each other: a footer one writes, the other must read back exactly.

// codecFooter builds a fixed-disk footer the way an external producer would,
// using nothing but the exported codecs.
func codecFooter(original, current uint64, created time.Time) *types.FileFooter {
	f := &types.FileFooter{
		Features:           types.FeatureReserved,
		FormatVersion:      types.FormatVersion1,
		NextOffset:         types.FixedDataOffset,
		ModificationTime:   types.Timestamp(created),
		CreatorApplication: 0x66736167, // "fsag"
		CreatorVersion:     0x00010000,
		CreatorOS:          types.CreatorOSWindows,
		OriginalSize:       original,
		CurrentSize:        current,
		DiskGeometry:       types.PackGeometry(types.CHSGeometry(current / types.DefaultSectorSize)),
		DiskType:           uint32(types.DiskTypeFixed),
		Identifier:         [16]byte{0xF5, 0xA6, 0xE0, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D},
	}
	copy(f.Signature[:], types.VHDFooterSignature)
	f.Checksum = types.FooterChecksum(f)
	return f
}

func TestFooterChecksumAgreesWithVerifyFooterChecksum(t *testing.T) {
	f := codecFooter(1<<20, 1<<20, time.Date(2026, time.October, 11, 0, 0, 0, 0, time.UTC))
	p := NewVHDFooterParser(nil)

	if !p.VerifyFooterChecksum(f) {
		t.Fatal("VerifyFooterChecksum rejected a footer sealed with FooterChecksum")
	}

	// Flipping any single byte outside the checksum field must break it.
	raw, _ := f.MarshalBinary()
	for i := range raw {
		if i >= 64 && i < 68 {
			continue
		}
		mutated := bytes.Clone(raw)
		mutated[i] ^= 0x01
		g, err := decodeFooter(mutated)
		if err != nil {
			t.Fatalf("decodeFooter: %v", err)
		}
		if p.VerifyFooterChecksum(g) {
			t.Fatalf("VerifyFooterChecksum accepted the footer with byte %d flipped", i)
		}
	}
}

func TestMarshalBinaryRoundTripsThroughTheParser(t *testing.T) {
	f := codecFooter(1<<20, 4<<20, time.Date(2024, time.June, 1, 12, 0, 0, 0, time.UTC))
	f.SavedState = 1
	f.Reserved[100] = 0x5A
	f.Checksum = types.FooterChecksum(f)

	raw, err := f.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}

	// Struct to bytes to struct.
	got, err := decodeFooter(raw)
	if err != nil {
		t.Fatalf("decodeFooter: %v", err)
	}
	if *got != *f {
		t.Fatalf("decodeFooter(MarshalBinary(f)) differs from f:\n got %+v\nwant %+v", *got, *f)
	}

	// Bytes to struct to bytes, starting from bytes the codecs did not write.
	fixture := buildFixedVHD(4096, nil)
	tail := fixture[len(fixture)-types.VHDFooterSize:]
	decoded, err := decodeFooter(tail)
	if err != nil {
		t.Fatalf("decodeFooter(fixture): %v", err)
	}
	again, _ := decoded.MarshalBinary()
	if !bytes.Equal(again, tail) {
		t.Fatal("MarshalBinary(decodeFooter(fixture footer)) is not byte-for-byte the fixture footer")
	}

	// And the full parser reads every field the codecs wrote.
	parsed, err := NewVHDFooterParser(bytes.NewReader(raw)).ReadFooterAt(0)
	if err != nil {
		t.Fatalf("ReadFooterAt rejected a codec-built footer: %v", err)
	}
	c, h, s := types.UnpackGeometry(f.DiskGeometry)
	if parsed.MediaSize != f.CurrentSize || parsed.OriginalSize != f.OriginalSize ||
		parsed.DiskType != types.DiskTypeFixed || parsed.Checksum != f.Checksum ||
		parsed.Identifier != f.Identifier || !parsed.ModTime.Equal(types.Time(f.ModificationTime)) ||
		parsed.Features != f.Features || parsed.CreatorOS != "Wi2k" || parsed.CreatorApp != "fsag" ||
		parsed.Cylinders != c || parsed.Heads != h || parsed.SectorsPerTrack != s || !parsed.SavedState {
		t.Fatalf("parsed footer does not match what was written: %+v", *parsed)
	}
}

func TestCodecBuiltFixedVHDOpens(t *testing.T) {
	// The recipe an external producer follows: payload, then the footer the
	// codecs marshal. OpenVHD must accept it and report what was written.
	const size = 1 << 20
	created := time.Date(2026, time.October, 11, 8, 0, 0, 0, time.UTC)

	f := codecFooter(size, size, created)
	footer, _ := f.MarshalBinary()
	img := append(bytes.Repeat([]byte{0x42}, size), footer...)

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	if d.DiskType() != types.DiskTypeFixed {
		t.Errorf("DiskType() = %s, want fixed", d.DiskType())
	}
	if d.Size() != size {
		t.Errorf("Size() = %d, want %d", d.Size(), size)
	}

	g := d.Geometry()
	wc, wh, ws := types.CHSGeometry(size / 512)
	if g.Cylinders != wc || g.Heads != wh || g.SectorsPerTrack != ws {
		t.Errorf("Geometry() CHS = %d/%d/%d, want %d/%d/%d", g.Cylinders, g.Heads, g.SectorsPerTrack, wc, wh, ws)
	}

	p := d.Provenance()
	if !p.Created.Equal(created) {
		t.Errorf("Provenance().Created = %s, want %s", p.Created, created)
	}
	if p.CreatorOS != "Wi2k" || p.OriginalSize != size || p.Features != types.FeatureReserved {
		t.Errorf("Provenance() = %+v", p)
	}

	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, size-512); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, bytes.Repeat([]byte{0x42}, 512)) {
		t.Fatal("ReadAt did not return the payload")
	}
}

func TestExpandedVHDReadsAtItsCurrentSize(t *testing.T) {
	// The footer records Original Size at offset 40 and Current Size at offset
	// 48. Reading them the other way round reports an expanded disk at its old
	// size and cuts reads off there. Both sizes are written here by offset, not
	// through any fixture that shares the parser's view of the layout.
	const (
		original = 1 << 20
		current  = 2 << 20
	)

	f := codecFooter(original, current, time.Time{})
	footer, _ := f.MarshalBinary()
	img := append(make([]byte, current), footer...)
	img[current-1] = 0x77

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	if d.Size() != current {
		t.Fatalf("Size() = %d, want the current size %d (the original size is %d)", d.Size(), current, original)
	}
	if got := d.Provenance().OriginalSize; got != original {
		t.Fatalf("Provenance().OriginalSize = %d, want %d", got, original)
	}

	buf := make([]byte, 1)
	if _, err := d.ReadAt(buf, current-1); err != nil || buf[0] != 0x77 {
		t.Fatalf("ReadAt(last byte) = %#x, %v; want 0x77", buf[0], err)
	}
}
