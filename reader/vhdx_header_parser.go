// SPDX-License-Identifier: MIT

// VHDX File Information and Image Header Parser
package reader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

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

// ReadFileInfo reads VHDX file information from offset 0.
func (p *VHDXFileInfoParser) ReadFileInfo() (*types.ParsedFileInformation, error) {
	br := binaryutil.NewReadAtReader(p.reader, 0)

	// Read signature (8 bytes)
	sig, err := br.ReadBytes(8)
	if err != nil {
		return nil, err
	}

	// Verify signature
	if !bytes.Equal(sig, []byte(types.VHDXFileSignature)) {
		return nil, errors.New("invalid VHDX file signature")
	}

	// Read creator string (512 bytes, UTF-8)
	creator, err := br.ReadString(512)
	if err != nil {
		return nil, err
	}

	return &types.ParsedFileInformation{
		Signature: string(sig),
		Creator:   creator,
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
		return nil, errors.New("could not read valid VHDX image header")
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
		return nil, errors.New("invalid VHDX image header signature")
	}

	// Verify CRC-32 over the full 4096 bytes with checksum field zeroed.
	storedChecksum := binary.LittleEndian.Uint32(buf[4:8])
	binary.LittleEndian.PutUint32(buf[4:8], 0)
	computed := binaryutil.CRC32(buf)
	binary.LittleEndian.PutUint32(buf[4:8], storedChecksum)
	if computed != storedChecksum {
		return nil, errors.New("VHDX image header CRC-32 mismatch")
	}

	// Validate format version (must be 0x0001).
	formatVersion := binary.LittleEndian.Uint16(buf[66:68])
	if formatVersion != 0x0001 {
		return nil, errors.New("unsupported VHDX image header format version")
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

// VerifyImageHeaderChecksum verifies the image header's CRC-32 checksum.
func (p *VHDXImageHeaderParser) VerifyImageHeaderChecksum(header *types.ImageHeader) bool {
	// Checksum is calculated with the checksum field itself set to zero
	// For VHDX, the checksum is calculated over the first 8 + 4016 = 4024 bytes (little-endian)
	headerCopy := *header
	headerCopy.Checksum = 0

	// Serialize header to bytes (up to offset 4 + 4020 bytes of the actual 4096-byte header)
	buf := imageHeaderToBytes(&headerCopy)

	// Compute CRC-32
	computed := binaryutil.CRC32(buf)

	return computed == header.Checksum
}

// imageHeaderToBytes serializes an ImageHeader for checksum verification.
func imageHeaderToBytes(header *types.ImageHeader) []byte {
	buf := new(bytes.Buffer)

	// Write signature (4 bytes)
	buf.Write(header.Signature[:])

	// Write checksum (4 bytes, little-endian) - set to zero for calculation
	writeLE32(buf, header.Checksum)

	// Write sequence number (8 bytes, little-endian)
	writeLE64(buf, header.SequenceNumber)

	// Write GUIDs (16 bytes each)
	buf.Write(header.FileWriteIdentifier[:])
	buf.Write(header.DataWriteIdentifier[:])
	buf.Write(header.LogIdentifier[:])

	// Write version fields (2+2 bytes)
	writeLE16(buf, header.LogFormatVersion)
	writeLE16(buf, header.FormatVersion)

	// Write log size and offset (4+8 bytes)
	writeLE32(buf, header.LogSize)
	writeLE64(buf, header.LogOffset)

	// Write reserved (4016 bytes)
	buf.Write(header.Reserved[:])

	return buf.Bytes()
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
		return nil, errors.New("invalid VHDX region table signature")
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
		return nil, errors.New("VHDX region table CRC-32 mismatch")
	}

	// Parse header fields (little-endian).
	numEntries := uint32(buf[8]) | uint32(buf[9])<<8 | uint32(buf[10])<<16 | uint32(buf[11])<<24

	// The specification caps the table at 2047 entries, which is also all that
	// fits in the 64 KB region. Checking up front avoids relying on the
	// per-entry bounds test, whose uint32 offset arithmetic would wrap for very
	// large counts.
	if numEntries > maxRegionTableEntries {
		return nil, fmt.Errorf("VHDX region table declares %d entries, more than the %d allowed",
			numEntries, maxRegionTableEntries)
	}

	// Parse entries (32 bytes each, starting at offset 16).
	entries := make([]types.ParsedRegionTableEntry, 0, numEntries)
	for i := uint32(0); i < numEntries; i++ {
		base := 16 + i*32
		if int(base)+32 > len(buf) {
			return nil, errors.New("region table entry extends beyond buffer")
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
