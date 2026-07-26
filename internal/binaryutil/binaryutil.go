// Package binaryutil provides endian-aware binary parsing utilities for the vhdi library.
package binaryutil

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
)

// castagnoliTable is the CRC-32C (Castagnoli) table; uses hardware acceleration
// (SSE4.2 / CLMUL) where available, providing significant throughput over a
// manual bit-loop.
var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// ErrInsufficientData is returned when there's not enough data to read.
var ErrInsufficientData = errors.New("insufficient data")

// Reader provides methods for reading binary data with explicit endianness handling.
type Reader interface {
	// ReadUint32BigEndian reads a 32-bit unsigned integer in big-endian format.
	ReadUint32BigEndian() (uint32, error)

	// ReadUint32LittleEndian reads a 32-bit unsigned integer in little-endian format.
	ReadUint32LittleEndian() (uint32, error)

	// ReadUint64BigEndian reads a 64-bit unsigned integer in big-endian format.
	ReadUint64BigEndian() (uint64, error)

	// ReadUint64LittleEndian reads a 64-bit unsigned integer in little-endian format.
	ReadUint64LittleEndian() (uint64, error)

	// ReadUint16BigEndian reads a 16-bit unsigned integer in big-endian format.
	ReadUint16BigEndian() (uint16, error)

	// ReadUint16LittleEndian reads a 16-bit unsigned integer in little-endian format.
	ReadUint16LittleEndian() (uint16, error)

	// ReadUint8 reads a single byte.
	ReadUint8() (uint8, error)

	// ReadBytes reads exactly n bytes.
	ReadBytes(n int) ([]byte, error)

	// ReadString reads a null-terminated or fixed-length string.
	ReadString(maxLen int) (string, error)

	// Offset returns the current read offset.
	Offset() int64

	// SeekTo moves the offset to an absolute position.
	SeekTo(offset int64)

	// Advance moves the offset by delta bytes.
	Advance(delta int64)
}

// ReadAtReader wraps an io.ReaderAt for offset-based binary parsing.
type ReadAtReader struct {
	r      io.ReaderAt
	offset int64
}

// NewReadAtReader creates a new ReadAtReader at the given offset.
func NewReadAtReader(r io.ReaderAt, offset int64) *ReadAtReader {
	return &ReadAtReader{r: r, offset: offset}
}

// NewReadAtReaderAtStart creates a new ReadAtReader at offset 0.
func NewReadAtReaderAtStart(r io.ReaderAt) *ReadAtReader {
	return &ReadAtReader{r: r, offset: 0}
}

// ReadUint32BigEndian reads a 32-bit big-endian unsigned integer.
func (r *ReadAtReader) ReadUint32BigEndian() (uint32, error) {
	b, err := r.ReadBytes(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

// ReadUint32LittleEndian reads a 32-bit little-endian unsigned integer.
func (r *ReadAtReader) ReadUint32LittleEndian() (uint32, error) {
	b, err := r.ReadBytes(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

// ReadUint64BigEndian reads a 64-bit big-endian unsigned integer.
func (r *ReadAtReader) ReadUint64BigEndian() (uint64, error) {
	b, err := r.ReadBytes(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

// ReadUint64LittleEndian reads a 64-bit little-endian unsigned integer.
func (r *ReadAtReader) ReadUint64LittleEndian() (uint64, error) {
	b, err := r.ReadBytes(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

// ReadUint16BigEndian reads a 16-bit big-endian unsigned integer.
func (r *ReadAtReader) ReadUint16BigEndian() (uint16, error) {
	b, err := r.ReadBytes(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

// ReadUint16LittleEndian reads a 16-bit little-endian unsigned integer.
func (r *ReadAtReader) ReadUint16LittleEndian() (uint16, error) {
	b, err := r.ReadBytes(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

// ReadUint8 reads a single byte.
func (r *ReadAtReader) ReadUint8() (uint8, error) {
	b, err := r.ReadBytes(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// ReadBytes reads exactly n bytes from the reader at the current offset.
func (r *ReadAtReader) ReadBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	nn, err := r.r.ReadAt(b, r.offset)
	if err != nil {
		if err == io.EOF && nn > 0 {
			// Partial read before EOF
			r.offset += int64(nn)
			return b[:nn], err
		}
		return nil, err
	}
	if nn != n {
		return nil, ErrInsufficientData
	}
	r.offset += int64(n)
	return b, nil
}

// ReadString reads a fixed-length string, stripping null terminators.
func (r *ReadAtReader) ReadString(maxLen int) (string, error) {
	b, err := r.ReadBytes(maxLen)
	if err != nil {
		return "", err
	}
	// Find null terminator
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), nil
		}
	}
	return string(b), nil
}

// ReadByteArray reads bytes into a fixed-size array.
func (r *ReadAtReader) ReadByteArray(size int) ([]*byte, error) {
	b, err := r.ReadBytes(size)
	if err != nil {
		return nil, err
	}
	arr := make([]*byte, size)
	for i := range b {
		arr[i] = &b[i]
	}
	return arr, nil
}

// Offset returns the current read offset.
func (r *ReadAtReader) Offset() int64 {
	return r.offset
}

// SeekTo moves the offset to an absolute position.
func (r *ReadAtReader) SeekTo(offset int64) {
	r.offset = offset
}

// Advance moves the offset by delta bytes.
func (r *ReadAtReader) Advance(delta int64) {
	r.offset += delta
}

// SkipBytes skips n bytes without reading them.
func (r *ReadAtReader) SkipBytes(n int) error {
	if n < 0 {
		return errors.New("cannot skip negative bytes")
	}
	r.offset += int64(n)
	return nil
}

// ============================================================================
// HELPER FUNCTIONS FOR COMMON PARSING TASKS
// ============================================================================

// ReadInt32BigEndian reads a 32-bit big-endian signed integer.
func (r *ReadAtReader) ReadInt32BigEndian() (int32, error) {
	v, err := r.ReadUint32BigEndian()
	return int32(v), err
}

// ReadInt32LittleEndian reads a 32-bit little-endian signed integer.
func (r *ReadAtReader) ReadInt32LittleEndian() (int32, error) {
	v, err := r.ReadUint32LittleEndian()
	return int32(v), err
}

// ReadInt64BigEndian reads a 64-bit big-endian signed integer.
func (r *ReadAtReader) ReadInt64BigEndian() (int64, error) {
	v, err := r.ReadUint64BigEndian()
	return int64(v), err
}

// ReadInt64LittleEndian reads a 64-bit little-endian signed integer.
func (r *ReadAtReader) ReadInt64LittleEndian() (int64, error) {
	v, err := r.ReadUint64LittleEndian()
	return int64(v), err
}

// ReadInt16BigEndian reads a 16-bit big-endian signed integer.
func (r *ReadAtReader) ReadInt16BigEndian() (int16, error) {
	v, err := r.ReadUint16BigEndian()
	return int16(v), err
}

// ReadInt16LittleEndian reads a 16-bit little-endian signed integer.
func (r *ReadAtReader) ReadInt16LittleEndian() (int16, error) {
	v, err := r.ReadUint16LittleEndian()
	return int16(v), err
}

// ============================================================================
// CHECKSUM & VALIDATION HELPERS
// ============================================================================

// CRC32 computes a CRC-32C (Castagnoli) checksum matching the VHDX polynomial
// 0x82f63b78. Uses hardware acceleration where available.
//
// This is a VHDX construct. VHD footers and dynamic disk headers use an
// entirely different algorithm — see VHDChecksum.
func CRC32(data []byte) uint32 {
	return crc32.Checksum(data, castagnoliTable)
}

// VHDChecksum computes the checksum used by VHD file footers and dynamic disk
// headers, as defined by the VHD Image Format Specification: "a one's
// complement of the sum of all the bytes in the footer without the checksum
// field".
//
// The caller must zero the structure's checksum field before calling.
//
// This is deliberately not a CRC. Applying CRC32 (CRC-32C) to VHD structures
// rejects every conformant VHD image.
func VHDChecksum(data []byte) uint32 {
	var sum uint32
	for _, b := range data {
		sum += uint32(b)
	}
	return ^sum
}

// VerifyVHDChecksum verifies a VHD structure whose checksum field has already
// been zeroed against its stored checksum value.
func VerifyVHDChecksum(data []byte, storedChecksum uint32) bool {
	return VHDChecksum(data) == storedChecksum
}

// CRC32WithInitial computes a CRC-32C checksum starting from an existing CRC state.
func CRC32WithInitial(data []byte, initial uint32) uint32 {
	return crc32.Update(initial^0xFFFFFFFF, castagnoliTable, data) ^ 0xFFFFFFFF
}

// VerifyCRC32 verifies data against a stored CRC-32 checksum.
func VerifyCRC32(data []byte, storedChecksum uint32) bool {
	computed := CRC32(data)
	return computed == storedChecksum
}

// ============================================================================
// GUID UTILITIES
// ============================================================================

// GUIDToString converts a 16-byte GUID to its string representation (RFC 4122 format).
func GUIDToString(guid [16]byte) string {
	// Format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
	// GUID bytes are stored as little-endian for first three fields, big-endian for rest
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%s",
		binary.LittleEndian.Uint32(guid[0:4]),
		binary.LittleEndian.Uint16(guid[4:6]),
		binary.LittleEndian.Uint16(guid[6:8]),
		binary.BigEndian.Uint16(guid[8:10]),
		hex.EncodeToString(guid[10:16]))
}

// GUIDFromString parses a GUID string and returns the byte array.
// Format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
// The first three components are stored as little-endian, matching GUIDToString.
func GUIDFromString(s string) ([16]byte, error) {
	var guid [16]byte

	// Remove hyphens
	s = strings.ReplaceAll(s, "-", "")

	// Check length
	if len(s) != 32 {
		return guid, errors.New("invalid GUID format")
	}

	// Decode hex string
	b, err := hex.DecodeString(s)
	if err != nil {
		return guid, err
	}

	// Reverse bytes for the three little-endian components to match file storage.
	// Component 1: bytes 0-3 (uint32 LE)
	binary.LittleEndian.PutUint32(guid[0:4], binary.BigEndian.Uint32(b[0:4]))
	// Component 2: bytes 4-5 (uint16 LE)
	binary.LittleEndian.PutUint16(guid[4:6], binary.BigEndian.Uint16(b[4:6]))
	// Component 3: bytes 6-7 (uint16 LE)
	binary.LittleEndian.PutUint16(guid[6:8], binary.BigEndian.Uint16(b[6:8]))
	// Components 4-5: bytes 8-15 (big-endian, stored as-is)
	copy(guid[8:], b[8:])

	return guid, nil
}

// GUIDEqual compares two GUIDs for equality.
func GUIDEqual(a, b [16]byte) bool {
	return a == b
}

// ============================================================================
// UTILITIES FOR READING FIXED STRUCTURES
// ============================================================================

// ReadFixedBytes reads a fixed number of bytes into an array.
func (r *ReadAtReader) ReadFixedBytes(size int) ([]byte, error) {
	return r.ReadBytes(size)
}

// ReadFixedByteArray reads into a fixed-size byte array (e.g., [8]byte).
// Note: Due to Go's type system, callers should read bytes and copy manually
// or use specific helper methods for their structure.

// PeekBytes reads bytes without advancing the offset.
func (r *ReadAtReader) PeekBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	nn, err := r.r.ReadAt(b, r.offset)
	if err != nil {
		if err == io.EOF && nn > 0 {
			return b[:nn], err
		}
		return nil, err
	}
	if nn != n {
		return nil, ErrInsufficientData
	}
	return b, nil
}

// PeekUint32BigEndian peeks at a 32-bit big-endian value without advancing.
func (r *ReadAtReader) PeekUint32BigEndian() (uint32, error) {
	b, err := r.PeekBytes(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

// PeekUint32LittleEndian peeks at a 32-bit little-endian value without advancing.
func (r *ReadAtReader) PeekUint32LittleEndian() (uint32, error) {
	b, err := r.PeekBytes(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}
