// SPDX-License-Identifier: MIT

package reader

import "testing"

// TestPlatformCodeConstantsMatchFourCC pins every VHD parent locator platform
// code against the four ASCII characters the specification names it by.
//
// These constants are written as hex literals, so a transposed nibble produces a
// value that still compiles, still looks plausible in review, and silently stops
// matching real images. That is exactly what happened to platformCodeW2ku, which
// was 0x5732316B ("W21k") and therefore never matched a real absolute-path
// locator: readLocatorPath fell to its default branch, the entry's Value stayed
// empty, and parentCandidates lost the fallback path entirely.
//
// Deriving the expected value from the name rather than restating the literal is
// what gives this test its power; a copied-and-adjusted literal would reproduce
// the bug.
func TestPlatformCodeConstantsMatchFourCC(t *testing.T) {
	fourCC := func(s string) uint32 {
		if len(s) != 4 {
			t.Fatalf("platform code name %q is not four characters", s)
		}
		return uint32(s[0])<<24 | uint32(s[1])<<16 | uint32(s[2])<<8 | uint32(s[3])
	}

	cases := []struct {
		name string
		got  uint32
	}{
		{"Wi2r", platformCodeWi2r},
		{"Wi2k", platformCodeWi2k},
		{"W2ru", platformCodeW2ru},
		{"W2ku", platformCodeW2ku},
		{"Mac ", platformCodeMac},
		{"MacX", platformCodeMacX},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if want := fourCC(tc.name); tc.got != want {
				t.Errorf("platform code for %q = %#08x (%q), want %#08x",
					tc.name, tc.got, platformCodeString(tc.got), want)
			}
			if got := platformCodeString(tc.got); got != tc.name {
				t.Errorf("platformCodeString(%#08x) = %q, want %q", tc.got, got, tc.name)
			}
		})
	}
}

// TestPlatformCodeNoneIsEmpty documents that the zero code is not a FourCC and
// must render as the empty string, since parentCandidates skips empty values.
func TestPlatformCodeNoneIsEmpty(t *testing.T) {
	if got := platformCodeString(platformCodeNone); got != "" {
		t.Errorf("platformCodeString(none) = %q, want empty", got)
	}
}
