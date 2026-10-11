// SPDX-License-Identifier: MIT

package types

// CHS limits from the VHD specification's "CHS Calculation" appendix.
const (
	chsMaxSectors    = 65535 * 16 * 255 // the largest geometry the field can hold
	chsLargeDiskFrom = 65535 * 16 * 63  // from here on, 255 sectors per track
)

// CHSGeometry computes the cylinder/head/sectors-per-track geometry the VHD
// specification assigns to a disk of totalSectors 512-byte sectors. It is a
// direct port of the algorithm in the specification's appendix, "CHS
// Calculation".
//
// The encoding tops out at 65535 × 16 × 255 sectors, about 127 GB. Every larger
// disk gets that maximum geometry, which cannot describe it; the virtual size,
// not the geometry, is the authoritative size of a VHD.
func CHSGeometry(totalSectors uint64) (cylinders uint16, heads, sectorsPerTrack uint8) {
	if totalSectors > chsMaxSectors {
		totalSectors = chsMaxSectors
	}

	var spt, h, cth uint64 // cth is cylinders × heads
	if totalSectors >= chsLargeDiskFrom {
		spt = 255
		h = 16
		cth = totalSectors / spt
	} else {
		spt = 17
		cth = totalSectors / spt
		h = max((cth+1023)/1024, 4)
		if cth >= h*1024 || h > 16 {
			spt = 31
			h = 16
			cth = totalSectors / spt
		}
		if cth >= h*1024 {
			spt = 63
			h = 16
			cth = totalSectors / spt
		}
	}

	return uint16(cth / h), uint8(h), uint8(spt)
}

// PackGeometry packs a CHS geometry into the footer's Disk Geometry field:
// cylinders in the high 16 bits, then heads and sectors per track in one byte
// each.
func PackGeometry(c uint16, h, s uint8) uint32 {
	return uint32(c)<<16 | uint32(h)<<8 | uint32(s)
}

// UnpackGeometry splits the footer's Disk Geometry field into cylinders, heads
// and sectors per track. It is the inverse of PackGeometry.
func UnpackGeometry(g uint32) (cylinders uint16, heads, sectorsPerTrack uint8) {
	return uint16(g >> 16), uint8(g >> 8), uint8(g)
}
