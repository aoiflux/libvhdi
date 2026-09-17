// SPDX-License-Identifier: MIT

// VHDX File Information and Image Header Parser
package reader

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// maxRegionTableEntries is the region table entry limit from the VHDX
// specification. The 64 KB region could not hold more in any case.
const maxRegionTableEntries = 2047

// VHDXFileInfoParser provides functionality to parse VHDX file information.
type VHDXFileInfoParser struct {
	reader io.ReaderAt
}

// NewVHDXFileInfoParser creates a new VHDX file info parser.
func NewVHDXFileInfoParser(r io.ReaderAt) *VHDXFileInfoParser {
	return &VHDXFileInfoParser{reader: r}
}

// ReadFileInfo reads the VHDX file identifier at offset 0.
//
// The structure is the 8-byte signature followed by a 512-byte Creator field
// naming the tool that produced the image. That string is the only provenance
// VHDX records about its producer, which makes it worth reading even though
// nothing about decoding the image depends on it.
func (p *VHDXFileInfoParser) ReadFileInfo() (*types.ParsedFileInformation, error) {
	br := binaryutil.NewReadAtReader(p.reader, 0)

	sig, err := br.ReadBytes(8)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(sig, []byte(types.VHDXFileSignature)) {
		return nil, types.Corruptf("read VHDX file identifier", types.FileFormatVHDX, 0, "Signature",
			"signature is %q, not %q", printableSignature(sig), types.VHDXFileSignature)
	}

	// The Creator field is 512 bytes of UTF-16 little-endian, not bytes. Read
	// as a NUL-terminated byte string it stops at the high byte of the first
	// character, so "Microsoft Windows 10.0" decodes as "M".
	raw, err := br.ReadBytes(512)
	if err != nil {
		return nil, err
	}

	return &types.ParsedFileInformation{
		Signature: string(sig),
		Creator:   strings.TrimSpace(decodeUTF16LE(raw)),
	}, nil
}

// ============================================================================
// VHDX IMAGE HEADER PARSER
// ============================================================================

// VHDXImageHeaderParser provides functionality to parse VHDX image headers.
type VHDXImageHeaderParser struct {
	reader io.ReaderAt
}

// NewVHDXImageHeaderParser creates a new VHDX image header parser.
func NewVHDXImageHeaderParser(r io.ReaderAt) *VHDXImageHeaderParser {
	return &VHDXImageHeaderParser{reader: r}
}

// ReadImageHeader reads and validates both image headers, returning the valid
// one with the higher sequence number (per the VHDX spec).
func (p *VHDXImageHeaderParser) ReadImageHeader() (*types.ParsedImageHeader, error) {
	primary, err1 := p.ReadImageHeaderAt(types.VHDXFirstHeaderOffset)
	secondary, err2 := p.ReadImageHeaderAt(types.VHDXSecondHeaderOffset)

	switch {
	case err1 == nil && err2 == nil:
		// Both valid — pick the one with the higher sequence number.
		if primary.SequenceNumber >= secondary.SequenceNumber {
			return primary, nil
		}
		return secondary, nil
	case err1 == nil:
		return primary, nil
	case err2 == nil:
		return secondary, nil
	default:
		// Both copies failed. Report both reasons: the two headers fail for
		// different causes often enough that one of them is usually the lead.
		return nil, fmt.Errorf(
			"%w: neither VHDX header could be read: primary at %d: %v; secondary at %d: %v",
			types.ErrCorruptImage,
			types.VHDXFirstHeaderOffset, err1,
			types.VHDXSecondHeaderOffset, err2)
	}
}

// ReadImageHeaderAt reads and validates a single VHDX image header at the given offset.
// CRC-32 is computed over all 4096 bytes with the checksum field (bytes 4-7) zeroed.
func (p *VHDXImageHeaderParser) ReadImageHeaderAt(offset int64) (*types.ParsedImageHeader, error) {
	// Read the full 4096-byte header for in-place CRC verification.
	buf := make([]byte, types.VHDXHeaderSize)
	if _, err := p.reader.ReadAt(buf, offset); err != nil {
		return nil, err
	}

	// Validate signature ("head").
	if !bytes.Equal(buf[0:4], []byte(types.VHDXHeaderSignature)) {
		return nil, types.Corruptf("read VHDX header", types.FileFormatVHDX, offset, "Signature",
			"signature is %q, not %q", printableSignature(buf[0:4]), types.VHDXHeaderSignature)
	}

	// Verify CRC-32 over the full 4096 bytes with checksum field zeroed.
	storedChecksum := binary.LittleEndian.Uint32(buf[4:8])
	binary.LittleEndian.PutUint32(buf[4:8], 0)
	computed := binaryutil.CRC32(buf)
	binary.LittleEndian.PutUint32(buf[4:8], storedChecksum)
	if computed != storedChecksum {
		return nil, types.Corruptf("read VHDX header", types.FileFormatVHDX, offset, "Checksum",
			"stored CRC-32C %#08x does not match the computed %#08x", storedChecksum, computed)
	}

	// Validate format version (must be 0x0001).
	formatVersion := binary.LittleEndian.Uint16(buf[66:68])
	if formatVersion != 0x0001 {
		return nil, types.Unsupportedf("read VHDX header", types.FileFormatVHDX, offset, "Version",
			"version %#04x, only 1 is implemented", formatVersion)
	}

	// Parse all fields from the raw buffer (all little-endian).
	parsed := &types.ParsedImageHeader{
		Checksum:         storedChecksum,
		SequenceNumber:   binary.LittleEndian.Uint64(buf[8:16]),
		LogFormatVersion: binary.LittleEndian.Uint16(buf[64:66]),
		FormatVersion:    formatVersion,
		LogSize:          binary.LittleEndian.Uint32(buf[68:72]),
		LogOffset:        int64(binary.LittleEndian.Uint64(buf[72:80])),
	}
	copy(parsed.FileWriteIdentifier[:], buf[16:32])
	copy(parsed.DataWriteIdentifier[:], buf[32:48])
	copy(parsed.LogIdentifier[:], buf[48:64])

	return parsed, nil
}

// Helper functions for writing little-endian values
func writeLE16(buf *bytes.Buffer, v uint16) {
	buf.WriteByte(byte(v))
	buf.WriteByte(byte(v >> 8))
}

func writeLE32(buf *bytes.Buffer, v uint32) {
	buf.WriteByte(byte(v))
	buf.WriteByte(byte(v >> 8))
	buf.WriteByte(byte(v >> 16))
	buf.WriteByte(byte(v >> 24))
}

func writeLE64(buf *bytes.Buffer, v uint64) {
	buf.WriteByte(byte(v))
	buf.WriteByte(byte(v >> 8))
	buf.WriteByte(byte(v >> 16))
	buf.WriteByte(byte(v >> 24))
	buf.WriteByte(byte(v >> 32))
	buf.WriteByte(byte(v >> 40))
	buf.WriteByte(byte(v >> 48))
	buf.WriteByte(byte(v >> 56))
}

// ============================================================================
// VHDX REGION TABLE PARSER
// ============================================================================

// VHDXRegionTableParser provides functionality to parse VHDX region tables.
type VHDXRegionTableParser struct {
	reader io.ReaderAt
}

// NewVHDXRegionTableParser creates a new VHDX region table parser.
func NewVHDXRegionTableParser(r io.ReaderAt) *VHDXRegionTableParser {
	return &VHDXRegionTableParser{reader: r}
}

// ReadRegionTableAt reads a VHDX region table from a specific offset.
// The region table is 64KB; CRC-32 is verified over the full 64KB with the
// checksum field (bytes 4-7) zeroed before calculation.
func (p *VHDXRegionTableParser) ReadRegionTableAt(offset int64) ([]types.ParsedRegionTableEntry, error) {
	// Read the full 64KB region table for CRC-32 verification.
	buf := make([]byte, types.VHDXRegionTableSize)
	if _, err := p.reader.ReadAt(buf, offset); err != nil {
		return nil, err
	}

	// Validate signature.
	if !bytes.Equal(buf[0:4], []byte(types.VHDXRegionSignature)) {
		return nil, types.Corruptf("read VHDX region table", types.FileFormatVHDX, offset, "Signature",
			"signature is %q, not %q", printableSignature(buf[0:4]), types.VHDXRegionSignature)
	}

	// Verify CRC-32 (polynomial 0x82f63b78, initial 0xFFFFFFFF, final XOR 0xFFFFFFFF).
	// Zero the stored checksum field (bytes 4-7) before computing.
	storedChecksum := uint32(buf[4]) | uint32(buf[5])<<8 | uint32(buf[6])<<16 | uint32(buf[7])<<24
	buf[4], buf[5], buf[6], buf[7] = 0, 0, 0, 0
	computed := binaryutil.CRC32(buf)
	buf[4] = byte(storedChecksum)
	buf[5] = byte(storedChecksum >> 8)
	buf[6] = byte(storedChecksum >> 16)
	buf[7] = byte(storedChecksum >> 24)
	if computed != storedChecksum {
		return nil, types.Corruptf("read VHDX region table", types.FileFormatVHDX, offset, "Checksum",
			"stored CRC-32C %#08x does not match the computed %#08x", storedChecksum, computed)
	}

	// Parse header fields (little-endian).
	numEntries := uint32(buf[8]) | uint32(buf[9])<<8 | uint32(buf[10])<<16 | uint32(buf[11])<<24

	// The specification caps the table at 2047 entries, which is also all that
	// fits in the 64 KB region. Checking up front avoids relying on the
	// per-entry bounds test, whose uint32 offset arithmetic would wrap for very
	// large counts.
	if numEntries > maxRegionTableEntries {
		return nil, types.Corruptf("read VHDX region table", types.FileFormatVHDX, offset, "Entry Count",
			"declares %d entries, more than the %d the 64 KB region can hold",
			numEntries, maxRegionTableEntries)
	}

	// Parse entries (32 bytes each, starting at offset 16).
	entries := make([]types.ParsedRegionTableEntry, 0, numEntries)
	for i := uint32(0); i < numEntries; i++ {
		base := 16 + i*32
		if int(base)+32 > len(buf) {
			return nil, types.Corruptf("read VHDX region table", types.FileFormatVHDX,
				offset+int64(base), "Region Table Entry",
				"entry %d extends past the end of the 64 KB region table", i)
		}

		var typeIDArray [16]byte
		copy(typeIDArray[:], buf[base:base+16])

		dataOffset := uint64(buf[base+16]) | uint64(buf[base+17])<<8 |
			uint64(buf[base+18])<<16 | uint64(buf[base+19])<<24 |
			uint64(buf[base+20])<<32 | uint64(buf[base+21])<<40 |
			uint64(buf[base+22])<<48 | uint64(buf[base+23])<<56

		dataSize := uint32(buf[base+24]) | uint32(buf[base+25])<<8 |
			uint32(buf[base+26])<<16 | uint32(buf[base+27])<<24

		isRequired := uint32(buf[base+28]) | uint32(buf[base+29])<<8 |
			uint32(buf[base+30])<<16 | uint32(buf[base+31])<<24

		entries = append(entries, types.ParsedRegionTableEntry{
			TypeIdentifier: typeIDArray,
			DataOffset:     int64(dataOffset),
			DataSize:       dataSize,
			IsRequired:     isRequired != 0,
		})
	}

	return entries, nil
}
