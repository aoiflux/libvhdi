// SPDX-License-Identifier: MIT

package change_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aoiflux/libvhdi/change"
	"github.com/aoiflux/libvhdi/change/adapters/exfat"
	"github.com/aoiflux/libvhdi/change/adapters/ext"
	"github.com/aoiflux/libvhdi/change/adapters/fat"
	"github.com/aoiflux/libvhdi/change/adapters/hfs"
	"github.com/aoiflux/libvhdi/change/adapters/ntfs"
	"github.com/aoiflux/libvhdi/change/adapters/xfs"
	"github.com/aoiflux/libvhdi/vhdimap"
)

// Every adapter must satisfy the contract. These are compile-time facts, and
// asserting them here as well means the module's own tests fail rather than a
// consumer's build.
var (
	_ vhdimap.Filesystem = (*ntfs.Volume)(nil)
	_ vhdimap.Filesystem = (*ext.Volume)(nil)
	_ vhdimap.Filesystem = (*fat.Volume)(nil)
	_ vhdimap.Filesystem = (*exfat.Volume)(nil)
	_ vhdimap.Filesystem = (*hfs.Volume)(nil)
	_ vhdimap.Filesystem = (*xfs.Volume)(nil)
	_ vhdimap.Journal    = (*ntfs.Volume)(nil)
)

// signatureDisk builds a 4 KiB region carrying one filesystem's signature and
// nothing else. It is enough to exercise detection, which is deliberately a
// signature check rather than "open each parser and see which one does not
// error" -- a parser handed a structure it does not understand can succeed on
// plausible rubbish, and the first one to do so would win.
func signatureDisk(place func([]byte)) []byte {
	buf := make([]byte, 4096)
	place(buf)
	return buf
}

func TestUnknownFilesystemIsDistinctFromADamagedOne(t *testing.T) {
	blank := bytes.NewReader(make([]byte, 4096))

	_, err := change.OpenVolumeAt(context.Background(), blank, 0, 4096, 4096)
	if !errors.Is(err, change.ErrUnknownFilesystem) {
		t.Fatalf("err = %v, want ErrUnknownFilesystem", err)
	}
}

// TestDetectionRejectsWhatItCannotRead pins that each signature is required in
// full. A near miss must not be accepted, because handing the wrong parser a
// volume is how a confident wrong answer is produced.
func TestDetectionRejectsWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		place func([]byte)
	}{
		{"NTFS signature one byte short", func(b []byte) { copy(b[3:], "NTFS   ") }},
		{"NTFS signature at the wrong offset", func(b []byte) { copy(b[0:], "NTFS    ") }},
		{"ext magic byte-swapped", func(b []byte) { b[1080], b[1081] = 0xEF, 0x53 }},
		{"HFS signature at the wrong offset", func(b []byte) { copy(b[512:], "H+") }},
		{"XFS magic at the wrong offset", func(b []byte) { copy(b[512:], "XFSB") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := bytes.NewReader(signatureDisk(tc.place))
			_, err := change.OpenVolumeAt(context.Background(), r, 0, 4096, 4096)
			if !errors.Is(err, change.ErrUnknownFilesystem) {
				t.Errorf("err = %v, want ErrUnknownFilesystem", err)
			}
		})
	}
}

// TestDetectionReachesTheRightParser checks that a recognised signature stops
// being "unknown". The parsers then fail on the rest of the structure, which is
// the correct outcome for four kilobytes of zeroes -- what matters is that the
// failure is a parse failure and not ErrUnknownFilesystem, since the two mean
// opposite things to an examiner.
func TestDetectionReachesTheRightParser(t *testing.T) {
	for _, tc := range []struct {
		name  string
		place func([]byte)
	}{
		{"ntfs", func(b []byte) { copy(b[3:], "NTFS    ") }},
		{"exfat", func(b []byte) { copy(b[3:], "EXFAT   ") }},
		{"xfs", func(b []byte) { copy(b[0:], "XFSB") }},
		{"fat32", func(b []byte) { copy(b[82:], "FAT32   ") }},
		{"fat16", func(b []byte) { copy(b[54:], "FAT16   ") }},
		{"fat12", func(b []byte) { copy(b[54:], "FAT12   ") }},
		{"hfs+", func(b []byte) { copy(b[1024:], "H+") }},
		{"hfsx", func(b []byte) { copy(b[1024:], "HX") }},
		{"ext", func(b []byte) { b[1080], b[1081] = 0x53, 0xEF }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := bytes.NewReader(signatureDisk(tc.place))
			_, err := change.OpenVolumeAt(context.Background(), r, 0, 4096, 4096)
			if errors.Is(err, change.ErrUnknownFilesystem) {
				t.Errorf("signature not recognised: %v", err)
			}
		})
	}
}

// TestTruncatedRegionFailsRatherThanGuessing: fewer than 1024 bytes cannot
// carry the superblock signatures, so detection must refuse rather than decide
// on the boot sector alone.
func TestTruncatedRegionFailsRatherThanGuessing(t *testing.T) {
	r := bytes.NewReader(make([]byte, 512))

	_, err := change.OpenVolumeAt(context.Background(), r, 0, 512, 512)
	if err == nil {
		t.Fatal("a 512-byte region was accepted")
	}
}

// TestSpanCoversToTheEndOfTheDiskWhenLengthIsUnknown. Guessing a shorter span
// would silently drop every changed range past the guess, which reads as "those
// files did not change".
func TestSpanCoversToTheEndOfTheDiskWhenLengthIsUnknown(t *testing.T) {
	v := &change.Volume{Base: 1 << 20}

	got := v.Span(4 << 20)
	if got.Offset != 1<<20 {
		t.Errorf("offset = %d, want %d", got.Offset, 1<<20)
	}
	if want := int64(3 << 20); got.Length != want {
		t.Errorf("length = %d, want %d", got.Length, want)
	}
}

func TestSpanUsesTheRecordedLengthWhenThereIsOne(t *testing.T) {
	v := &change.Volume{Base: 1 << 20, Length: 1 << 20}

	if got := v.Span(4 << 20); got.Length != 1<<20 {
		t.Errorf("length = %d, want %d", got.Length, 1<<20)
	}
}

func TestNilReaderIsRejected(t *testing.T) {
	if _, errs := change.OpenVolumes(context.Background(), nil, 0); len(errs) == 0 {
		t.Fatal("a nil reader was accepted")
	}
}
