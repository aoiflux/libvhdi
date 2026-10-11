// SPDX-License-Identifier: MIT

package types_test

import (
	"math/rand"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// The expected geometries below were derived by hand from the algorithm in the
// VHD specification's appendix, "CHS Calculation", at each point where it
// changes branch. They are not output captured from the code under test.
func TestCHSGeometryAtTheSpecBoundaries(t *testing.T) {
	const gib = 1 << 30 / 512 // sectors per GiB

	for _, tc := range []struct {
		name    string
		sectors uint64
		c       uint16
		h, s    uint8
	}{
		{"empty disk: heads floor at 4", 0, 0, 4, 17},
		{"last disk on 4 heads × 17", 4*1024*17 - 1, 1023, 4, 17},
		{"first disk past 4 heads × 17 jumps to 31", 4 * 1024 * 17, 140, 16, 31},
		{"last disk on 16 heads × 17", 16*1024*17 - 1, 1023, 16, 17},
		{"first disk past 16 heads × 17", 16 * 1024 * 17, 561, 16, 31},
		{"last disk on 31 sectors per track", 16*1024*31 - 1, 1023, 16, 31},
		{"first disk on 63 sectors per track", 16 * 1024 * 31, 503, 16, 63},
		{"last disk below the large-disk branch", 65535*16*63 - 1, 65534, 16, 63},
		{"first disk on 255 sectors per track", 65535 * 16 * 63, 16191, 16, 255},
		{"the largest geometry", 65535 * 16 * 255, 65535, 16, 255},
		{"past the largest geometry: capped", 65535*16*255 + 1, 65535, 16, 255},
		{"2 TiB: capped", 2048 * gib, 65535, 16, 255},

		// Sizes Windows Disk Management offers by default.
		{"512 MiB", gib / 2, 1040, 16, 63},
		{"1 GiB", gib, 2080, 16, 63},
		{"127 GiB", 127 * gib, 65278, 16, 255},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, h, s := types.CHSGeometry(tc.sectors)
			if c != tc.c || h != tc.h || s != tc.s {
				t.Fatalf("CHSGeometry(%d) = %d/%d/%d, want %d/%d/%d",
					tc.sectors, c, h, s, tc.c, tc.h, tc.s)
			}
		})
	}
}

func TestCHSGeometryNeverExceedsTheDisk(t *testing.T) {
	// Whatever the size, the geometry must describe no more than the disk
	// holds, and must only use values the algorithm can produce.
	rng := rand.New(rand.NewSource(20261011))
	for i := range 100000 {
		var sectors uint64
		switch i % 3 {
		case 0:
			sectors = uint64(rng.Int63n(1 << 20))
		case 1:
			sectors = uint64(rng.Int63n(65535 * 16 * 255))
		default:
			sectors = uint64(rng.Int63n(1 << 40))
		}

		c, h, s := types.CHSGeometry(sectors)
		capped := min(sectors, 65535*16*255)
		if uint64(c)*uint64(h)*uint64(s) > capped {
			t.Fatalf("CHSGeometry(%d) = %d/%d/%d describes %d sectors, more than the disk holds",
				sectors, c, h, s, uint64(c)*uint64(h)*uint64(s))
		}
		if h < 4 || h > 16 {
			t.Fatalf("CHSGeometry(%d) gave %d heads, want 4 to 16", sectors, h)
		}
		switch s {
		case 17, 31, 63, 255:
		default:
			t.Fatalf("CHSGeometry(%d) gave %d sectors per track", sectors, s)
		}
	}
}

func TestPackGeometry(t *testing.T) {
	if got := types.PackGeometry(1040, 16, 63); got != 0x0410103F {
		t.Fatalf("PackGeometry(1040, 16, 63) = %#08x, want 0x0410103F", got)
	}

	for _, g := range [][3]uint32{{0, 0, 0}, {1040, 16, 63}, {65535, 16, 255}, {65535, 255, 255}, {1, 2, 3}} {
		packed := types.PackGeometry(uint16(g[0]), uint8(g[1]), uint8(g[2]))
		c, h, s := types.UnpackGeometry(packed)
		if uint32(c) != g[0] || uint32(h) != g[1] || uint32(s) != g[2] {
			t.Errorf("UnpackGeometry(PackGeometry(%v)) = %d/%d/%d", g, c, h, s)
		}
	}
}
