// SPDX-License-Identifier: MIT

// Package reader provides high-level APIs for reading VHD/VHDX virtual disk files.
package reader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/aoiflux/libvhdi/types"
)

// VHDFooterParser provides functionality to parse VHD file footers.
type VHDFooterParser struct {
	reader io.ReaderAt
}

// NewVHDFooterParser creates a new VHD footer parser.
func NewVHDFooterParser(r io.ReaderAt) *VHDFooterParser {
	return &VHDFooterParser{reader: r}
}

// FooterSource identifies which copy of the VHD footer an image was opened
// from.
//
// A VHD carries its footer at the end of the file, and dynamic and differencing
// disks additionally mirror it at offset 0 precisely so that a damaged tail
// stays recoverable. Which copy was used is forensically material: a disk opened
// from its mirror is one whose trailing footer is unreadable, and a report that
// does not say so presents a recovered image as an intact one.
type FooterSource uint8

const (
	// FooterSourceUnknown means no footer has been read, or the disk is a VHDX,
	// which has no footer at all.
	FooterSourceUnknown FooterSource = iota

	// FooterSourceTrailing is the 512-byte footer in the final sector, which is
	// where the specification puts it and where an intact image is read from.
	FooterSourceTrailing

	// FooterSourceTrailingLegacy is a 511-byte trailing footer, written by
	// Microsoft Virtual PC before the format was documented.
	FooterSourceTrailingLegacy

	// FooterSourceMirror is the copy at offset 0. Reaching it means the
	// trailing footer could not be read, so the image is damaged even though it
	// opened.
	FooterSourceMirror
)

// String names the footer copy for a report or a log line.
func (s FooterSource) String() string {
	switch s {
	case FooterSourceTrailing:
		return "trailing"
	case FooterSourceTrailingLegacy:
		return "trailing-legacy-511"
	case FooterSourceMirror:
		return "mirror-at-zero"
	default:
		return "unknown"
	}
}

// Recovered reports whether the footer came from somewhere other than the
// conformant trailing sector, which means the image is damaged.
func (s FooterSource) Recovered() bool {
	return s == FooterSourceTrailingLegacy || s == FooterSourceMirror
}

// footerSizeLegacy is the footer length written by Microsoft Virtual PC before
// the format was documented: the 512-byte structure with its final reserved
// byte omitted. Reserved bytes are defined to be zero, so zero-padding the
// missing byte reproduces the structure exactly, checksum included.
const footerSizeLegacy = 511

// ReadFooterFromEnd reads the VHD footer from the end of the file (at offset -512).
//
// This is the strict path: it reads only the conformant trailing footer and
// reports an error for anything else. Callers wanting the recovery paths --
// the legacy 511-byte footer and the mirror copy at offset 0 -- want
// readFooterWithRecovery instead.
func (p *VHDFooterParser) ReadFooterFromEnd(fileSize int64) (*types.ParsedFileFooter, error) {
	if fileSize < types.VHDFooterSize {
		return nil, errors.New("file too small to contain VHD footer")
	}

	footerOffset := fileSize - int64(types.VHDFooterSize)
	return p.ReadFooterAt(footerOffset)
}

// readFooterWithRecovery locates a VHD footer, falling back through the two
// recovery locations when the conformant one is unreadable.
//
// The order is deliberate. The trailing 512-byte footer is authoritative and is
// always preferred. A 511-byte trailing footer is tried next because it is a
// real producer's output rather than damage. The mirror at offset 0 is last,
// because reaching it means the tail of the file is gone.
//
// The returned FooterSource is not a diagnostic detail: a caller acquiring
// evidence needs to know the image was recovered rather than read intact.
func (p *VHDFooterParser) readFooterWithRecovery(fileSize int64) (*types.ParsedFileFooter, FooterSource, int64, error) {
	type attempt struct {
		source FooterSource
		offset int64
		size   int
	}

	attempts := make([]attempt, 0, 3)
	if fileSize >= types.VHDFooterSize {
		attempts = append(attempts, attempt{
			source: FooterSourceTrailing,
			offset: fileSize - int64(types.VHDFooterSize),
			size:   types.VHDFooterSize,
		})
	}
	if fileSize >= footerSizeLegacy {
		attempts = append(attempts, attempt{
			source: FooterSourceTrailingLegacy,
			offset: fileSize - footerSizeLegacy,
			size:   footerSizeLegacy,
		})
	}
	if fileSize >= types.VHDFooterSize || fileSize == 0 {
		// fileSize == 0 means the size could not be derived. The mirror is at a
		// fixed offset and so is the one copy still reachable without it.
		attempts = append(attempts, attempt{
			source: FooterSourceMirror,
			offset: 0,
			size:   types.VHDFooterSize,
		})
	}

	if len(attempts) == 0 {
		return nil, FooterSourceUnknown, 0, errors.New("file too small to contain VHD footer")
	}

	var failures []error
	for _, a := range attempts {
		footer, err := p.readFooterAtN(a.offset, a.size)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s footer at offset %d: %w", a.source, a.offset, err))
			continue
		}

		if a.source == FooterSourceMirror && footer.DiskType == types.DiskTypeFixed {
			// Only dynamic and differencing disks mirror the footer. On a fixed
			// disk offset 0 is payload, so a structure that parses there is
			// either a coincidence or an embedded image -- either way it does
			// not describe this file.
			failures = append(failures, fmt.Errorf(
				"%s footer at offset 0: describes a fixed disk, which has no mirror footer", a.source))
			continue
		}

		return footer, a.source, int64(a.size), nil
	}

	return nil, FooterSourceUnknown, 0, fmt.Errorf(
		"no readable VHD footer at any known location: %w", errors.Join(failures...))
}

// ReadFooterAt reads the VHD footer from a specific offset.
func (p *VHDFooterParser) ReadFooterAt(offset int64) (*types.ParsedFileFooter, error) {
	return p.readFooterAtN(offset, types.VHDFooterSize)
}

// readFooterAtN reads a footer of n bytes at offset, where n is either the
// 512-byte structure or the 511-byte legacy truncation of it.
//
// The bytes are pulled into a fixed 512-byte buffer first. That is what lets
// the legacy footer share every line of parsing and checksum logic with the
// conformant one: the absent final byte is a reserved byte, defined to be zero,
// and the buffer already holds zero there.
func (p *VHDFooterParser) readFooterAtN(offset int64, n int) (*types.ParsedFileFooter, error) {
	if n <= 0 || n > types.VHDFooterSize {
		return nil, fmt.Errorf("invalid VHD footer length %d", n)
	}
	if offset < 0 {
		return nil, fmt.Errorf("negative VHD footer offset %d", offset)
	}

	raw := make([]byte, types.VHDFooterSize)
	if _, err := io.ReadFull(io.NewSectionReader(p.reader, offset, int64(n)), raw[:n]); err != nil {
		return nil, err
	}

	return p.parseFooter(raw, offset)
}

// parseFooter decodes and validates a 512-byte footer image.
//
// offset is where the footer was read from. It is carried only so that errors
// can say which copy of the footer failed, which is the difference between "the
// image is broken" and "the trailing footer is broken but the mirror may not
// be".
func (p *VHDFooterParser) parseFooter(raw []byte, offset int64) (*types.ParsedFileFooter, error) {
	footer, err := decodeFooter(raw)
	if err != nil {
		return nil, err
	}

	// Verify signature
	if !bytes.Equal(footer.Signature[:], []byte(types.VHDFooterSignature)) {
		return nil, types.Corruptf("read VHD footer", types.FileFormatVHD, offset, "Cookie",
			"signature is %q, not %q", printableSignature(footer.Signature[:]), types.VHDFooterSignature)
	}

	// Verify the footer's one's-complement checksum. This is the only integrity
	// check a VHD footer carries, so a mismatch means every field below is
	// suspect -- including the ones that say where the rest of the image is.
	if !p.VerifyFooterChecksum(footer) {
		return nil, types.Corruptf("read VHD footer", types.FileFormatVHD, offset, "Checksum",
			"stored checksum %#08x does not match the footer contents", footer.Checksum)
	}

	// Parse into typed footer. Every field the footer carries is retained:
	// dropping one here makes it unreportable, and an image's geometry, its
	// creating application and whether it was saved from a running machine are
	// all facts an examiner may need.
	parsed := &types.ParsedFileFooter{
		FormatVersion:  footer.FormatVersion,
		NextOffset:     int64(footer.NextOffset),
		MediaSize:      footer.CurrentSize,
		DiskType:       types.DiskType(footer.DiskType),
		Checksum:       footer.Checksum,
		Identifier:     footer.Identifier,
		ModTime:        types.Time(footer.ModificationTime),
		CreatorApp:     creatorAppString(footer.CreatorApplication),
		Features:       footer.Features,
		CreatorVersion: footer.CreatorVersion,
		CreatorOS:      creatorAppString(footer.CreatorOS),
		OriginalSize:   footer.OriginalSize,
		SavedState:     footer.SavedState != 0,
	}

	// Disk geometry packs cylinders into the high 16 bits, then heads and
	// sectors-per-track into one byte each.
	parsed.Cylinders, parsed.Heads, parsed.SectorsPerTrack = types.UnpackGeometry(footer.DiskGeometry)

	// Validate format version (must be 0x00010000 = v1.0). A different version
	// is a well-formed image this library cannot read, not a damaged one.
	if footer.FormatVersion != types.FormatVersion1 {
		return nil, types.Unsupportedf("read VHD footer", types.FileFormatVHD, offset, "File Format Version",
			"version %#08x, only 0x00010000 (1.0) is implemented", footer.FormatVersion)
	}

	// Validate disk type.
	switch parsed.DiskType {
	case types.DiskTypeFixed, types.DiskTypeDynamic, types.DiskTypeDifferential:
		// valid
	default:
		// Types 0, 1, 5 and 6 are defined by the specification as reserved or
		// deprecated. They are well-formed values this library does not
		// implement rather than corruption, so say so.
		return nil, types.Unsupportedf("read VHD footer", types.FileFormatVHD, offset, "Disk Type",
			"disk type %d is not fixed (2), dynamic (3) or differencing (4)", uint32(parsed.DiskType))
	}

	// Validate next_offset: fixed disks must have 0xFFFFFFFFFFFFFFFF;
	// dynamic/differencing must have a sane offset (>= 512).
	switch parsed.DiskType {
	case types.DiskTypeFixed:
		if footer.NextOffset != types.FixedDataOffset {
			return nil, types.Corruptf("read VHD footer", types.FileFormatVHD, offset, "Data Offset",
				"fixed disk records %#016x, but a fixed disk has no further structures and must record 0xFFFFFFFFFFFFFFFF",
				footer.NextOffset)
		}
	case types.DiskTypeDynamic, types.DiskTypeDifferential:
		if parsed.NextOffset < 512 {
			return nil, types.Corruptf("read VHD footer", types.FileFormatVHD, offset, "Data Offset",
				"dynamic disk header at %d, which is inside the footer copy at offset 0", parsed.NextOffset)
		}
	}

	return parsed, nil
}

// VerifyFooterChecksum verifies the footer's checksum.
//
// Per the VHD specification this is a one's complement of the sum of all footer
// bytes with the checksum field zeroed — not a CRC. CRC-32C applies only to
// VHDX structures.
func (p *VHDFooterParser) VerifyFooterChecksum(footer *types.FileFooter) bool {
	return types.FooterChecksum(footer) == footer.Checksum
}

// decodeFooter decodes the 512-byte footer structure field by field, without
// validating any of it. It is the inverse of types.FileFooter.MarshalBinary.
func decodeFooter(raw []byte) (*types.FileFooter, error) {
	if len(raw) != types.VHDFooterSize {
		return nil, fmt.Errorf("VHD footer is %d bytes, want %d", len(raw), types.VHDFooterSize)
	}

	be := binary.BigEndian
	footer := &types.FileFooter{
		Features:           be.Uint32(raw[8:12]),
		FormatVersion:      be.Uint32(raw[12:16]),
		NextOffset:         be.Uint64(raw[16:24]),
		ModificationTime:   be.Uint32(raw[24:28]),
		CreatorApplication: be.Uint32(raw[28:32]),
		CreatorVersion:     be.Uint32(raw[32:36]),
		CreatorOS:          be.Uint32(raw[36:40]),
		OriginalSize:       be.Uint64(raw[40:48]),
		CurrentSize:        be.Uint64(raw[48:56]),
		DiskGeometry:       be.Uint32(raw[56:60]),
		DiskType:           be.Uint32(raw[60:64]),
		Checksum:           be.Uint32(raw[64:68]),
		SavedState:         raw[84],
	}
	copy(footer.Signature[:], raw[0:8])
	copy(footer.Identifier[:], raw[68:84])
	// The reserved padding is kept: it is part of the checksummed bytes.
	copy(footer.Reserved[:], raw[85:])

	return footer, nil
}

// Helper functions for writing big-endian values
func writeBE32(buf *bytes.Buffer, v uint32) {
	buf.WriteByte(byte(v >> 24))
	buf.WriteByte(byte(v >> 16))
	buf.WriteByte(byte(v >> 8))
	buf.WriteByte(byte(v))
}

func writeBE64(buf *bytes.Buffer, v uint64) {
	buf.WriteByte(byte(v >> 56))
	buf.WriteByte(byte(v >> 48))
	buf.WriteByte(byte(v >> 40))
	buf.WriteByte(byte(v >> 32))
	buf.WriteByte(byte(v >> 24))
	buf.WriteByte(byte(v >> 16))
	buf.WriteByte(byte(v >> 8))
	buf.WriteByte(byte(v))
}

// creatorAppString converts a 4-byte creator app code to a human-readable string.
func creatorAppString(code uint32) string {
	// Creator application codes are typically ASCII bytes
	return string([]byte{
		byte(code >> 24),
		byte(code >> 16),
		byte(code >> 8),
		byte(code),
	})
}

// printableSignature renders a signature field for an error message, so a
// corrupt image cannot inject control characters into a log.
func printableSignature(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			out = append(out, '.')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
