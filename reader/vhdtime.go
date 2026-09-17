// SPDX-License-Identifier: MIT

package reader

import "time"

// vhdEpochUnix is the VHD format's epoch expressed as a Unix timestamp:
// 1 January 2000, 00:00:00 UTC.
//
// VHD does not use the Unix epoch. Every timestamp in a VHD footer or dynamic
// disk header counts seconds from 2000, so decoding one as a Unix timestamp
// lands it thirty years early -- a disk created in 2024 reports as 1994. That
// is not a cosmetic error in a library whose output is evidence: an image's
// creation time and a parent's recorded modification time are exactly the sort
// of fact a report exists to state.
const vhdEpochUnix = 946684800

// vhdTimestamp converts a raw VHD timestamp into a UTC time.
//
// A raw zero is returned as the zero Time rather than as midnight on 1 January
// 2000. The distinction matters: a zero field means the producer recorded no
// timestamp -- which is what a non-differencing disk's parent modification time
// always is -- and rendering that as a real instant would invent a fact. Callers
// test with IsZero.
func vhdTimestamp(raw uint32) time.Time {
	if raw == 0 {
		return time.Time{}
	}
	return time.Unix(vhdEpochUnix+int64(raw), 0).UTC()
}

// vhdTimestampRaw converts a time back into the format's own encoding. It is the
// inverse of vhdTimestamp and exists so tests can build fixtures with real
// timestamps rather than magic numbers.
func vhdTimestampRaw(t time.Time) uint32 {
	if t.IsZero() {
		return 0
	}
	secs := t.Unix() - vhdEpochUnix
	if secs <= 0 {
		return 0
	}
	return uint32(secs)
}
