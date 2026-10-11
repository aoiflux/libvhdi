// SPDX-License-Identifier: MIT

package types

import (
	"encoding/binary"
	"time"
)

// The functions in this file are pure codecs for VHD footer fields. They read
// and write byte slices and values, never files: libvhdi stays read-only for
// images. They exist so that tools and tests which produce a VHD footer of their
// own compute it exactly the way libvhdi verifies it.

// Epoch is the VHD format's epoch: 1 January 2000, 00:00:00 UTC.
//
// VHD does not use the Unix epoch. Every timestamp in a VHD footer or dynamic
// disk header counts seconds from 2000, so decoding one as a Unix timestamp
// lands it thirty years early -- a disk created in 2024 reports as 1994. That
// is not a cosmetic error in a library whose output is evidence: an image's
// creation time and a parent's recorded modification time are exactly the sort
// of fact a report exists to state.
//
// Epoch is informational. Time and Timestamp never read it; they use a constant,
// so assigning a different value to Epoch has no effect on any codec.
var Epoch = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// epochUnix is the VHD epoch as a Unix timestamp. Every codec uses it rather
// than Epoch, which is an exported variable a caller could reassign.
const epochUnix = 946684800

// Time converts a raw VHD timestamp into a UTC time.
//
// A raw zero is returned as the zero Time rather than as midnight on 1 January
// 2000. The distinction matters: a zero field means the producer recorded no
// timestamp -- which is what a non-differencing disk's parent modification time
// always is -- and rendering that as a real instant would invent a fact. Callers
// test with IsZero.
func Time(v uint32) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(epochUnix+int64(v), 0).UTC()
}

// Timestamp converts a time into the format's own encoding: whole seconds since
// Epoch. It is the inverse of Time for every instant the encoding can hold,
// which is 2000-01-01T00:00:01Z to 2136-02-07T06:28:15Z. Sub-second precision
// is truncated.
//
// The encoding is unsigned and 32 bits wide, so out-of-range times are clamped
// rather than wrapped: the zero Time and anything at or before Epoch encode as
// 0, which Time reads back as "no timestamp recorded", and anything after 2136
// encodes as 0xFFFFFFFF. Wrapping would turn a time just before the epoch into
// one in 2136, and one just after 2136 into one in 2000.
func Timestamp(t time.Time) uint32 {
	if t.IsZero() {
		return 0
	}
	secs := t.Unix() - epochUnix
	switch {
	case secs <= 0:
		return 0
	case secs > 0xFFFFFFFF:
		return 0xFFFFFFFF
	}
	return uint32(secs)
}

// Checksum computes the checksum used by VHD file footers and dynamic disk
// headers, as defined by the VHD Image Format Specification: "a one's
// complement of the sum of all the bytes in the footer without the checksum
// field".
//
// The caller must zero the structure's checksum field before calling;
// FooterChecksum does that for a footer.
//
// This is deliberately not a CRC. CRC-32C is a VHDX construct, and applying it
// to VHD structures rejects every conformant VHD image.
func Checksum(b []byte) uint32 {
	var sum uint32
	for _, c := range b {
		sum += uint32(c)
	}
	return ^sum
}

// FooterChecksum returns the checksum f should carry: Checksum over the
// marshalled footer with its Checksum field taken as zero. The value f.Checksum
// currently holds does not affect the result.
func FooterChecksum(f *FileFooter) uint32 {
	c := *f
	c.Checksum = 0
	b, _ := c.MarshalBinary()
	return Checksum(b)
}

// MarshalBinary encodes the footer as the 512-byte big-endian structure the
// specification defines, with every field at its specified offset.
//
// It writes the fields exactly as given. It neither validates them nor
// computes the checksum: set f.Checksum = FooterChecksum(f) first. The error is
// always nil; the method exists to satisfy encoding.BinaryMarshaler.
func (f *FileFooter) MarshalBinary() ([]byte, error) {
	b := make([]byte, VHDFooterSize)
	be := binary.BigEndian

	copy(b[0:8], f.Signature[:])
	be.PutUint32(b[8:12], f.Features)
	be.PutUint32(b[12:16], f.FormatVersion)
	be.PutUint64(b[16:24], f.NextOffset)
	be.PutUint32(b[24:28], f.ModificationTime)
	be.PutUint32(b[28:32], f.CreatorApplication)
	be.PutUint32(b[32:36], f.CreatorVersion)
	be.PutUint32(b[36:40], f.CreatorOS)
	be.PutUint64(b[40:48], f.OriginalSize)
	be.PutUint64(b[48:56], f.CurrentSize)
	be.PutUint32(b[56:60], f.DiskGeometry)
	be.PutUint32(b[60:64], f.DiskType)
	be.PutUint32(b[64:68], f.Checksum)
	copy(b[68:84], f.Identifier[:])
	b[84] = f.SavedState
	copy(b[85:], f.Reserved[:])

	return b, nil
}
