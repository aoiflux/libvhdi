// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// A VHD keeps its footer in the final sector, and dynamic and differencing
// disks mirror it at offset 0 for exactly one reason: so that an image whose
// tail is damaged stays readable. Before these tests the library never read the
// mirror, so a single bad sector at the end of an otherwise intact dynamic VHD
// made it unopenable.
//
// Recovery must also stay visible. An image opened from a fallback copy is a
// damaged image, and a report that presents it as an intact read is worse than
// one that refuses to open it at all.

// destroyTrailingFooter overwrites the final 512 bytes, simulating the damaged
// tail these recovery paths exist for.
func destroyTrailingFooter(img []byte) {
	tail := img[len(img)-vhdFooterLen:]
	for i := range tail {
		tail[i] = 0xDB
	}
}

func TestIntactVHDReportsTheTrailingFooter(t *testing.T) {
	img := buildDynamicVHD(types.DiskTypeDynamic, 2*1024*1024, 1024*1024, []vhdBlock{
		{allocated: true, presentSectors: []int{0}, data: repeatByte(0xA1, 1024*1024)},
		{allocated: false},
	}, "", [16]byte{})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	if got := d.FooterSource(); got != FooterSourceTrailing {
		t.Fatalf("FooterSource() = %v, want %v", got, FooterSourceTrailing)
	}
	if d.FooterRecovered() {
		t.Fatal("FooterRecovered() is true for an intact image")
	}
}

func TestDynamicVHDRecoversFromTheMirrorFooter(t *testing.T) {
	const (
		mediaSize = 2 * 1024 * 1024
		blockSize = 1024 * 1024
	)
	payload := repeatByte(0xA1, blockSize)

	img := buildDynamicVHD(types.DiskTypeDynamic, mediaSize, blockSize, []vhdBlock{
		{allocated: true, presentSectors: []int{0}, data: payload},
		{allocated: false},
	}, "", [16]byte{})

	// Prove the image is good before it is damaged, so a failure below is
	// attributable to the damage and not to the fixture.
	if _, err := OpenVHD(bytes.NewReader(img), int64(len(img))); err != nil {
		t.Fatalf("fixture does not open before damage: %v", err)
	}

	destroyTrailingFooter(img)

	// The strict path must still fail. If it did not, the recovery path below
	// would pass without ever being exercised.
	fp := NewVHDFooterParser(bytes.NewReader(img))
	if _, err := fp.ReadFooterFromEnd(int64(len(img))); err == nil {
		t.Fatal("ReadFooterFromEnd succeeded on a destroyed trailing footer; this test proves nothing")
	}

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD did not recover from the mirror footer: %v", err)
	}
	defer d.Close()

	if got := d.FooterSource(); got != FooterSourceMirror {
		t.Fatalf("FooterSource() = %v, want %v", got, FooterSourceMirror)
	}
	if !d.FooterRecovered() {
		t.Fatal("FooterRecovered() is false for an image opened from the mirror")
	}
	if got := d.Size(); got != mediaSize {
		t.Fatalf("Size() = %d, want %d", got, mediaSize)
	}

	// Recovery is only worth anything if the data comes back. The damage is
	// confined to the final sector, which is footer, not payload.
	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt after recovery: %v", err)
	}
	if !bytes.Equal(buf, payload[:512]) {
		t.Fatalf("recovered disk returned %x..., want %x...", buf[:8], payload[:8])
	}
}

func TestDifferencingVHDRecoversFromTheMirrorFooter(t *testing.T) {
	// A differencing disk mirrors its footer too, and it is the one most likely
	// to be captured mid-write and so to have a damaged tail.
	img := buildDynamicVHD(types.DiskTypeDifferential, 1024*1024, 1024*1024, []vhdBlock{
		{allocated: true, presentSectors: []int{0}, data: repeatByte(0xB2, 1024*1024)},
	}, "parent.vhd", [16]byte{0x77})

	destroyTrailingFooter(img)

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD did not recover from the mirror footer: %v", err)
	}
	defer d.Close()

	if got := d.FooterSource(); got != FooterSourceMirror {
		t.Fatalf("FooterSource() = %v, want %v", got, FooterSourceMirror)
	}
	if !d.IsDifferencing() {
		t.Fatal("recovered disk lost its differencing disk type")
	}
	if got := d.ParentFilename(); got != "parent.vhd" {
		t.Fatalf("ParentFilename() = %q after recovery, want %q", got, "parent.vhd")
	}
}

func TestFixedVHDIsNotRecoveredFromOffsetZero(t *testing.T) {
	// A fixed disk has no mirror footer: offset 0 is payload. If the recovery
	// path accepted a footer there, a fixed image whose payload merely begins
	// with footer-shaped bytes would be decoded against a structure that does
	// not describe it -- and the result would look plausible.
	//
	// This fixture is the adversarial case: a valid fixed-disk footer sitting at
	// offset 0 as ordinary user data.
	const mediaSize = 4096
	decoy := buildFixedVHD(mediaSize, nil)
	footerBytes := append([]byte(nil), decoy[mediaSize:]...)

	img := buildFixedVHD(mediaSize, func(payload []byte) {
		copy(payload, footerBytes)
	})

	// Confirm the decoy really is a parseable footer, or the test is vacuous.
	fp := NewVHDFooterParser(bytes.NewReader(img))
	if _, err := fp.ReadFooterAt(0); err != nil {
		t.Fatalf("decoy at offset 0 does not parse as a footer (%v); test proves nothing", err)
	}

	destroyTrailingFooter(img)

	if _, err := OpenVHD(bytes.NewReader(img), int64(len(img))); err == nil {
		t.Fatal("OpenVHD accepted a fixed disk's payload as a mirror footer")
	}
}

func TestLegacy511ByteFooterOpens(t *testing.T) {
	// Microsoft Virtual PC wrote a 511-byte footer before the format was
	// documented: the 512-byte structure with its final reserved byte omitted.
	// Reserved bytes are defined to be zero, so the truncation is lossless and
	// the checksum still verifies once the missing byte is treated as zero.
	const mediaSize = 4096
	payload := repeatByte(0x5A, mediaSize)

	full := buildFixedVHD(mediaSize, func(p []byte) { copy(p, payload) })

	// The footer's final byte is reserved and therefore zero in a conformant
	// image. If it were not, dropping it would change the checksum and this
	// fixture would be testing something else.
	if last := full[len(full)-1]; last != 0 {
		t.Fatalf("footer's final byte is %#x, not the reserved zero the legacy truncation assumes", last)
	}

	img := full[:len(full)-1]

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD rejected a 511-byte legacy footer: %v", err)
	}
	defer d.Close()

	if got := d.FooterSource(); got != FooterSourceTrailingLegacy {
		t.Fatalf("FooterSource() = %v, want %v", got, FooterSourceTrailingLegacy)
	}
	if !d.FooterRecovered() {
		t.Fatal("FooterRecovered() is false for a legacy footer")
	}
	if got := d.Size(); got != mediaSize {
		t.Fatalf("Size() = %d, want %d", got, mediaSize)
	}

	// validateVHDFixed must have used the 511-byte footer length. Assuming 512
	// would make the payload appear one byte longer than the file allows and
	// reject every legacy image.
	buf := make([]byte, mediaSize)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt on a legacy image: %v", err)
	}
	if !bytes.Equal(buf, payload) {
		t.Fatal("legacy image returned the wrong payload")
	}
}

func TestNoFooterAnywhereNamesEveryLocationTried(t *testing.T) {
	// When recovery fails the caller deserves to know it was not a single
	// missed read. The error has to account for every location.
	img := make([]byte, 8192)
	for i := range img {
		img[i] = 0xEE
	}

	_, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err == nil {
		t.Fatal("OpenVHD accepted an image with no footer at all")
	}

	msg := err.Error()
	for _, want := range []string{
		FooterSourceTrailing.String(),
		FooterSourceTrailingLegacy.String(),
		FooterSourceMirror.String(),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention the %s location: %v", want, err)
		}
	}
}

func TestFooterSourceStringsAreDistinct(t *testing.T) {
	seen := map[string]FooterSource{}
	for _, s := range []FooterSource{
		FooterSourceUnknown,
		FooterSourceTrailing,
		FooterSourceTrailingLegacy,
		FooterSourceMirror,
	} {
		name := s.String()
		if prev, dup := seen[name]; dup {
			t.Fatalf("FooterSource(%d) and FooterSource(%d) both render as %q", prev, s, name)
		}
		seen[name] = s
	}
}
