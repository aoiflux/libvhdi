// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// "invalid VHD footer signature" tells an examiner that something failed and
// nothing else. Which copy of the footer? At what offset? Once the library has
// given up, examining the image by hand is the only recourse, and an error that
// names a location is where that starts.
//
// The second thing these tests pin is the distinction between a broken file and
// a missing feature. One says the evidence is damaged; the other says to try a
// different tool. Conflating them sends an examiner looking for damage that is
// not there.

// asStructural extracts the StructuralError from err, failing if there is none.
func asStructural(t *testing.T, err error) *types.StructuralError {
	t.Helper()
	var se *types.StructuralError
	if !errors.As(err, &se) {
		t.Fatalf("error is not a *StructuralError: %v", err)
	}
	return se
}

func TestCorruptFooterErrorNamesFieldAndOffset(t *testing.T) {
	img := buildFixedVHD(4096, nil)
	// Break the checksum without touching anything else, so the failure is
	// unambiguously the checksum and not a knock-on effect.
	img[len(img)-512+64] ^= 0xFF

	fp := NewVHDFooterParser(bytes.NewReader(img))
	_, err := fp.ReadFooterFromEnd(int64(len(img)))
	if err == nil {
		t.Fatal("a corrupted footer checksum was accepted")
	}

	se := asStructural(t, err)
	if se.Field != "Checksum" {
		t.Errorf("Field = %q, want %q", se.Field, "Checksum")
	}
	if want := int64(len(img) - 512); se.Offset != want {
		t.Errorf("Offset = %d, want %d", se.Offset, want)
	}
	if se.Format != types.FileFormatVHD {
		t.Errorf("Format = %v, want VHD", se.Format)
	}
	if !errors.Is(err, ErrCorruptImage) {
		t.Error("error does not satisfy errors.Is(err, ErrCorruptImage)")
	}
	if errors.Is(err, ErrUnsupportedFeature) {
		t.Error("a corrupt image was reported as an unsupported feature")
	}
}

func TestErrorMessageCarriesTheLocation(t *testing.T) {
	img := buildFixedVHD(4096, nil)
	img[len(img)-512+64] ^= 0xFF

	fp := NewVHDFooterParser(bytes.NewReader(img))
	_, err := fp.ReadFooterFromEnd(int64(len(img)))
	if err == nil {
		t.Fatal("a corrupted footer checksum was accepted")
	}

	msg := err.Error()
	for _, want := range []string{"read VHD footer", "VHD", "Checksum", "offset"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not mention %q", msg, want)
		}
	}
}

func TestReservedVHDDiskTypeIsUnsupportedNotCorrupt(t *testing.T) {
	// VHD disk types 0, 1, 5 and 6 are reserved or deprecated by the
	// specification. Such an image is well-formed; this library simply does not
	// implement it, and reporting it as corruption would be a lie about the
	// evidence.
	img := buildFixedVHD(4096, nil)
	f := img[len(img)-512:]
	f[60], f[61], f[62], f[63] = 0, 0, 0, 6 // disk type 6
	refreshFooterChecksum(f)

	fp := NewVHDFooterParser(bytes.NewReader(img))
	_, err := fp.ReadFooterFromEnd(int64(len(img)))
	if err == nil {
		t.Fatal("a reserved VHD disk type was accepted")
	}

	if !errors.Is(err, ErrUnsupportedFeature) {
		t.Errorf("reserved disk type is not reported as an unsupported feature: %v", err)
	}
	if errors.Is(err, ErrCorruptImage) {
		t.Errorf("reserved disk type is reported as corruption: %v", err)
	}

	se := asStructural(t, err)
	if se.Field != "Disk Type" {
		t.Errorf("Field = %q, want %q", se.Field, "Disk Type")
	}
}

// refreshFooterChecksum recomputes the spec checksum over an edited footer.
func refreshFooterChecksum(f []byte) {
	f[64], f[65], f[66], f[67] = 0, 0, 0, 0
	sum := vhdSpecChecksum(f[:vhdFooterLen])
	f[64] = byte(sum >> 24)
	f[65] = byte(sum >> 16)
	f[66] = byte(sum >> 8)
	f[67] = byte(sum)
}

func TestBadSignatureIsNotConfusedWithCorruption(t *testing.T) {
	// A file that is not a VHD at all still reports as corrupt, because from
	// the library's side those are the same observation: the bytes where a
	// footer should be are not a footer. What must not happen is a bare error
	// with no location.
	img := make([]byte, 8192)
	fp := NewVHDFooterParser(bytes.NewReader(img))
	_, err := fp.ReadFooterFromEnd(int64(len(img)))
	if err == nil {
		t.Fatal("an empty file was accepted as a VHD")
	}

	se := asStructural(t, err)
	if se.Field != "Cookie" {
		t.Errorf("Field = %q, want %q", se.Field, "Cookie")
	}
	if se.Offset != int64(len(img)-512) {
		t.Errorf("Offset = %d, want %d", se.Offset, len(img)-512)
	}
}

func TestVHDXHeaderErrorsAreStructural(t *testing.T) {
	img := buildVHDX(vhdxParams{
		virtualDiskSize: 2 * 1024 * 1024,
		blockSize:       1024 * 1024,
		sectorSize:      512,
	})
	// Corrupt both headers' checksums.
	img[types.VHDXFirstHeaderOffset+4] ^= 0xFF
	img[types.VHDXSecondHeaderOffset+4] ^= 0xFF

	_, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err == nil {
		t.Fatal("a VHDX with two corrupt headers was accepted")
	}
	if !errors.Is(err, ErrCorruptImage) {
		t.Errorf("error does not satisfy errors.Is(err, ErrCorruptImage): %v", err)
	}
	// Both header offsets should appear, since the caller needs to know both
	// copies were tried rather than one.
	msg := err.Error()
	for _, want := range []string{"primary", "secondary"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message does not mention the %s header: %v", want, err)
		}
	}
}

func TestStructuralErrorRendersWithoutAnOffset(t *testing.T) {
	// Some failures are about two fields disagreeing rather than about a byte.
	// NoOffset must not render as "at offset -1".
	err := types.Corruptf("check something", types.FileFormatVHDX, types.NoOffset, "Field",
		"the fields disagree")
	msg := err.Error()
	if strings.Contains(msg, "-1") || strings.Contains(msg, "at offset") {
		t.Fatalf("NoOffset rendered as an offset: %q", msg)
	}
	if !strings.Contains(msg, "Field") || !strings.Contains(msg, "check something") {
		t.Fatalf("message lost its context: %q", msg)
	}
}

func TestStructuralErrorUnwrapsToItsSentinel(t *testing.T) {
	corrupt := types.Corruptf("op", types.FileFormatVHD, 0, "f", "detail")
	if !errors.Is(corrupt, types.ErrCorruptImage) {
		t.Error("Corruptf does not wrap ErrCorruptImage")
	}
	if errors.Is(corrupt, types.ErrUnsupportedFeature) {
		t.Error("Corruptf wraps ErrUnsupportedFeature")
	}

	unsupported := types.Unsupportedf("op", types.FileFormatVHD, 0, "f", "detail")
	if !errors.Is(unsupported, types.ErrUnsupportedFeature) {
		t.Error("Unsupportedf does not wrap ErrUnsupportedFeature")
	}
	if errors.Is(unsupported, types.ErrCorruptImage) {
		t.Error("Unsupportedf wraps ErrCorruptImage")
	}
}

func TestSentinelsAreSharedAcrossPackages(t *testing.T) {
	// The reader and the parsers below it must agree on the sentinel, or
	// errors.Is answers differently depending on which layer noticed.
	if ErrCorruptImage != types.ErrCorruptImage {
		t.Error("reader.ErrCorruptImage is a different value from types.ErrCorruptImage")
	}
	if ErrUnsupportedFeature != types.ErrUnsupportedFeature {
		t.Error("reader.ErrUnsupportedFeature is a different value from types.ErrUnsupportedFeature")
	}
}

func TestCorruptMetadataErrorIsStructural(t *testing.T) {
	// The metadata parser lives below the reader and cannot import it, which is
	// why the sentinels are in types. This checks the wiring end to end.
	img := buildVHDX(vhdxParams{
		virtualDiskSize: 2 * 1024 * 1024,
		blockSize:       1024 * 1024,
		sectorSize:      512,
	})
	// Break the metadata table signature.
	copy(img[vhdxMetaRegionOff:vhdxMetaRegionOff+8], []byte("notmeta!"))

	_, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err == nil {
		t.Fatal("a VHDX with a corrupt metadata table was accepted")
	}
	if !errors.Is(err, ErrCorruptImage) {
		t.Errorf("metadata error does not satisfy errors.Is(err, ErrCorruptImage): %v", err)
	}

	se := asStructural(t, err)
	if se.Offset != vhdxMetaRegionOff {
		t.Errorf("Offset = %d, want the metadata region at %d", se.Offset, vhdxMetaRegionOff)
	}
}
