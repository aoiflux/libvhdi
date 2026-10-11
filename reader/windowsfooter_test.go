// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// TestWindowsFootersMatchTheCodecs holds the exported footer codecs to footers
// Windows itself wrote. testdata/footers holds the trailing 512 bytes of VHDs
// created by the Windows virtual disk service; see the README there for how.
// Unlike TestCorpusVHDFootersMatchTheCodecs, which needs full images and an
// environment variable, this runs on every go test, so CI checks the CHS port
// against Microsoft's encoding and not only against values derived by hand.
func TestWindowsFootersMatchTheCodecs(t *testing.T) {
	// Each geometry here is what Windows recorded. The table pins them so a
	// replaced or truncated file cannot pass by agreeing with a broken codec.
	for _, tc := range []struct {
		file     string
		size     uint64
		diskType types.DiskType
		c        uint16
		h, s     uint8
	}{
		{"fixed-3MiB.bin", 3 << 20, types.DiskTypeFixed, 90, 4, 17},
		{"fixed-512MiB.bin", 512 << 20, types.DiskTypeFixed, 1040, 16, 63},
		{"fixed-1GiB.bin", 1 << 30, types.DiskTypeFixed, 2080, 16, 63},
		{"fixed-40GiB.bin", 40 << 30, types.DiskTypeFixed, 20560, 16, 255},
		{"dynamic-127GiB.bin", 127 << 30, types.DiskTypeDynamic, 65278, 16, 255},
	} {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "footers", tc.file))
			if err != nil {
				t.Fatal(err)
			}

			footer, err := decodeFooter(raw)
			if err != nil {
				t.Fatalf("decodeFooter: %v", err)
			}
			if string(footer.Signature[:]) != types.VHDFooterSignature {
				t.Fatalf("signature %q, want %q", footer.Signature[:], types.VHDFooterSignature)
			}
			if footer.CreatorOS != types.CreatorOSWindows {
				t.Fatalf("Creator Host OS %#08x, want Wi2k", footer.CreatorOS)
			}
			if footer.OriginalSize != tc.size || footer.CurrentSize != tc.size {
				t.Fatalf("sizes %d/%d, want %d", footer.OriginalSize, footer.CurrentSize, tc.size)
			}
			if types.DiskType(footer.DiskType) != tc.diskType {
				t.Fatalf("disk type %d, want %s", footer.DiskType, tc.diskType)
			}

			if got := types.FooterChecksum(footer); got != footer.Checksum {
				t.Errorf("FooterChecksum = %#08x, Windows stored %#08x", got, footer.Checksum)
			}

			wc, wh, ws := types.UnpackGeometry(footer.DiskGeometry)
			if wc != tc.c || wh != tc.h || ws != tc.s {
				t.Fatalf("recorded geometry %d/%d/%d, want %d/%d/%d", wc, wh, ws, tc.c, tc.h, tc.s)
			}
			c, h, s := types.CHSGeometry(footer.CurrentSize / types.DefaultSectorSize)
			if c != wc || h != wh || s != ws {
				t.Errorf("CHSGeometry = %d/%d/%d, Windows recorded %d/%d/%d", c, h, s, wc, wh, ws)
			}

			again, _ := footer.MarshalBinary()
			if !bytes.Equal(again, raw) {
				t.Error("MarshalBinary(decodeFooter(footer)) is not byte-for-byte what Windows wrote")
			}

			if _, err := NewVHDFooterParser(bytes.NewReader(raw)).ReadFooterAt(0); err != nil {
				t.Errorf("ReadFooterAt rejected a Windows-written footer: %v", err)
			}
		})
	}
}
