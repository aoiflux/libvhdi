// Package reader provides high-level APIs for reading VHD/VHDX virtual disk files.
package reader

import (
	"bytes"
	"errors"
	"io"
	"time"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
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

// ReadFooterFromEnd reads the VHD footer from the end of the file (at offset -512).
func (p *VHDFooterParser) ReadFooterFromEnd(fileSize int64) (*types.ParsedFileFooter, error) {
	if fileSize < types.VHDFooterSize {
		return nil, errors.New("file too small to contain VHD footer")
	}

	footerOffset := fileSize - int64(types.VHDFooterSize)
	return p.ReadFooterAt(footerOffset)
}

// ReadFooterAt reads the VHD footer from a specific offset.
func (p *VHDFooterParser) ReadFooterAt(offset int64) (*types.ParsedFileFooter, error) {
	br := binaryutil.NewReadAtReader(p.reader, offset)

	// Read footer structure (512 bytes, big-endian)
	footer := &types.FileFooter{}

	// Read signature (8 bytes)
	sig, err := br.ReadBytes(8)
	if err != nil {
		return nil, err
	}
	copy(footer.Signature[:], sig)

	// Verify signature
	if !bytes.Equal(footer.Signature[:], []byte(types.VHDFooterSignature)) {
		return nil, errors.New("invalid VHD footer signature")
	}

	// Read remaining fields (big-endian)
	footer.Features, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	footer.FormatVersion, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	footer.NextOffset, err = br.ReadUint64BigEndian()
	if err != nil {
		return nil, err
	}

	footer.ModificationTime, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	footer.CreatorApplication, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	footer.CreatorVersion, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	footer.CreatorOS, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	footer.DiskSize, err = br.ReadUint64BigEndian()
	if err != nil {
		return nil, err
	}

	footer.DataSize, err = br.ReadUint64BigEndian()
	if err != nil {
		return nil, err
	}

	footer.DiskGeometry, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	footer.DiskType, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	footer.Checksum, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	// Read GUID (16 bytes)
	identifier, err := br.ReadBytes(16)
	if err != nil {
		return nil, err
	}
	copy(footer.Identifier[:], identifier)

	// Read saved state flag (1 byte)
	savedStateByte, err := br.ReadUint8()
	if err != nil {
		return nil, err
	}
	footer.SavedState = savedStateByte

	// Read reserved padding (427 bytes) — must be stored for checksum calculation.
	reserved, err := br.ReadBytes(427)
	if err != nil {
		return nil, err
	}
	copy(footer.Reserved[:], reserved)

	// Verify CRC-32 checksum.
	if !p.VerifyFooterChecksum(footer) {
		return nil, errors.New("VHD footer checksum mismatch")
	}

	// Parse into typed footer
	parsed := &types.ParsedFileFooter{
		FormatVersion: footer.FormatVersion,
		NextOffset:    int64(footer.NextOffset),
		MediaSize:     footer.DiskSize,
		DiskType:      types.DiskType(footer.DiskType),
		Checksum:      footer.Checksum,
		Identifier:    footer.Identifier,
		ModTime:       time.Unix(int64(footer.ModificationTime), 0),
		CreatorApp:    creatorAppString(footer.CreatorApplication),
	}

	// Validate format version (must be 0x00010000 = v1.0).
	if footer.FormatVersion != 0x00010000 {
		return nil, errors.New("unsupported VHD footer format version")
	}

	// Validate disk type.
	switch parsed.DiskType {
	case types.DiskTypeFixed, types.DiskTypeDynamic, types.DiskTypeDifferential:
		// valid
	default:
		return nil, errors.New("unsupported VHD disk type")
	}

	// Validate next_offset: fixed disks must have 0xFFFFFFFFFFFFFFFF;
	// dynamic/differencing must have a sane offset (>= 512).
	switch parsed.DiskType {
	case types.DiskTypeFixed:
		if footer.NextOffset != 0xFFFFFFFFFFFFFFFF {
			return nil, errors.New("VHD fixed disk has unexpected next_offset")
		}
	case types.DiskTypeDynamic, types.DiskTypeDifferential:
		if parsed.NextOffset < 512 {
			return nil, errors.New("VHD dynamic disk has invalid next_offset")
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
	// Checksum is calculated with the checksum field itself set to zero
	// Create a copy with checksum zeroed out
	footerCopy := *footer
	footerCopy.Checksum = 0

	// Serialize footer to bytes
	buf := footerToBytes(&footerCopy)

	return binaryutil.VHDChecksum(buf) == footer.Checksum
}

// footerToBytes serializes a FileFooter to bytes.
func footerToBytes(footer *types.FileFooter) []byte {
	buf := new(bytes.Buffer)

	// Write all fields in big-endian order
	buf.Write(footer.Signature[:])
	writeBE32(buf, footer.Features)
	writeBE32(buf, footer.FormatVersion)
	writeBE64(buf, footer.NextOffset)
	writeBE32(buf, footer.ModificationTime)
	writeBE32(buf, footer.CreatorApplication)
	writeBE32(buf, footer.CreatorVersion)
	writeBE32(buf, footer.CreatorOS)
	writeBE64(buf, footer.DiskSize)
	writeBE64(buf, footer.DataSize)
	writeBE32(buf, footer.DiskGeometry)
	writeBE32(buf, footer.DiskType)
	writeBE32(buf, footer.Checksum)
	buf.Write(footer.Identifier[:])
	buf.WriteByte(footer.SavedState)
	buf.Write(footer.Reserved[:])

	return buf.Bytes()
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
