package reader

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// ============================================================================
// Real-image corpus validation
//
// The synthetic fixtures elsewhere in this package are built to the published
// specifications, which is what caught the VHD checksum defect — but they cannot
// prove agreement with the images Hyper-V and qemu actually produce.
//
// These tests compare this library's decoded output against a raw dump produced
// by an independent implementation, byte for byte. They are skipped unless
// LIBVHDI_CORPUS names a directory laid out as:
//
//	<name>.vhd  or  <name>.vhdx   the image under test
//	<name>.raw                    its expected decoded contents
//
// A child of a differencing chain needs its parents present in the same
// directory; they resolve automatically. Parents may have their own .raw files,
// in which case they are checked too.
//
// scripts/gen-corpus.ps1 generates such a directory.
// ============================================================================

const corpusEnv = "LIBVHDI_CORPUS"

// corpusDir returns the corpus directory, or skips the test.
func corpusDir(t *testing.T) string {
	t.Helper()

	dir := os.Getenv(corpusEnv)
	if dir == "" {
		t.Skipf("set %s to a directory of real images with .raw counterparts (see scripts/gen-corpus.ps1)", corpusEnv)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("%s=%q: %v", corpusEnv, dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s=%q is not a directory", corpusEnv, dir)
	}
	return dir
}

// TestCorpusMatchesRawDumps decodes every corpus image and compares it against
// its raw dump.
func TestCorpusMatchesRawDumps(t *testing.T) {
	dir := corpusDir(t)

	images, err := corpusImages(dir)
	if err != nil {
		t.Fatalf("scanning %s: %v", dir, err)
	}
	if len(images) == 0 {
		t.Fatalf("no image/.raw pairs found in %s", dir)
	}
	t.Logf("validating %d image(s) against raw dumps", len(images))

	for _, image := range images {
		t.Run(filepath.Base(image), func(t *testing.T) {
			raw := rawPathFor(image)

			d, err := OpenFile(image)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer d.Close()

			if d.NeedsParent() {
				t.Fatalf("incomplete parent chain: %v", d.ParentResolveError())
			}
			t.Logf("format=%v type=%d size=%d block=%d sector=%d chain=%d log=%v",
				d.Format(), d.DiskType(), d.Size(), d.BlockSize(), d.SectorSize(),
				d.ChainDepth(), d.HasLog())

			rf, err := os.Open(raw)
			if err != nil {
				t.Fatalf("opening raw dump: %v", err)
			}
			defer rf.Close()

			rawInfo, err := rf.Stat()
			if err != nil {
				t.Fatalf("stat raw dump: %v", err)
			}

			// A raw dump shorter than the virtual disk is acceptable only if the
			// remainder of the disk is zeroes; some tools truncate trailing
			// zeroes. A longer dump always indicates a size disagreement.
			if uint64(rawInfo.Size()) > d.Size() {
				t.Fatalf("raw dump is %d bytes but the disk reports %d", rawInfo.Size(), d.Size())
			}

			if err := compareStreams(t, d, rf, d.Size(), uint64(rawInfo.Size())); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestCorpusRandomAccessMatchesSequential checks that random-access reads agree
// with the sequential decode, which exercises block and sector boundaries that a
// front-to-back read can mask.
func TestCorpusRandomAccessMatchesSequential(t *testing.T) {
	dir := corpusDir(t)

	images, err := corpusImages(dir)
	if err != nil {
		t.Fatalf("scanning %s: %v", dir, err)
	}
	if len(images) == 0 {
		t.Skipf("no image/.raw pairs found in %s", dir)
	}

	for _, image := range images {
		t.Run(filepath.Base(image), func(t *testing.T) {
			d, err := OpenFile(image)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer d.Close()

			rf, err := os.Open(rawPathFor(image))
			if err != nil {
				t.Fatalf("opening raw dump: %v", err)
			}
			defer rf.Close()

			rawInfo, _ := rf.Stat()
			rawSize := uint64(rawInfo.Size())

			// Offsets around structure boundaries, where off-by-one errors live.
			block := uint64(d.BlockSize())
			sector := uint64(d.SectorSize())
			var offsets []uint64
			for _, o := range []uint64{
				0, sector, sector - 1, block - sector, block - 1, block, block + 1,
				block + sector, 2 * block, d.Size() / 2,
			} {
				if o < rawSize {
					offsets = append(offsets, o)
				}
			}
			if rawSize > sector {
				offsets = append(offsets, rawSize-sector)
			}

			for _, off := range offsets {
				length := 4 * sector
				if off+length > rawSize {
					length = rawSize - off
				}
				if length == 0 {
					continue
				}

				got := make([]byte, length)
				if _, err := d.ReadAt(got, int64(off)); err != nil && !errors.Is(err, io.EOF) {
					t.Fatalf("ReadAt(%d): %v", off, err)
				}
				want := make([]byte, length)
				if _, err := rf.ReadAt(want, int64(off)); err != nil && !errors.Is(err, io.EOF) {
					t.Fatalf("raw ReadAt(%d): %v", off, err)
				}

				if !bytes.Equal(got, want) {
					t.Fatalf("mismatch at offset %d: first differing byte at +%d", off, firstDiff(got, want))
				}
			}
		})
	}
}

// compareStreams walks the whole disk, comparing against the raw dump and
// requiring anything past the dump's end to read as zeroes.
func compareStreams(t *testing.T, d *VirtualDisk, raw io.ReaderAt, diskSize, rawSize uint64) error {
	t.Helper()

	const chunk = 1 << 20
	got := make([]byte, chunk)
	want := make([]byte, chunk)

	for off := uint64(0); off < diskSize; off += chunk {
		n := uint64(chunk)
		if off+n > diskSize {
			n = diskSize - off
		}

		if _, err := d.ReadAt(got[:n], int64(off)); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("disk ReadAt(%d): %w", off, err)
		}

		if off >= rawSize {
			// Past the dump: the disk must be sparse here.
			for i := uint64(0); i < n; i++ {
				if got[i] != 0 {
					return fmt.Errorf("offset %d is %#x but lies past the raw dump, so it must be zero", off+i, got[i])
				}
			}
			continue
		}

		m := n
		if off+m > rawSize {
			m = rawSize - off
		}
		if _, err := raw.ReadAt(want[:m], int64(off)); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("raw ReadAt(%d): %w", off, err)
		}
		if !bytes.Equal(got[:m], want[:m]) {
			return fmt.Errorf("mismatch in [%d, %d): first differing byte at %d",
				off, off+m, off+uint64(firstDiff(got[:m], want[:m])))
		}
		// Any tail of this chunk beyond the dump must be zero.
		for i := m; i < n; i++ {
			if got[i] != 0 {
				return fmt.Errorf("offset %d is %#x but lies past the raw dump, so it must be zero", off+i, got[i])
			}
		}
	}
	return nil
}

// TestCorpusHarnessSelfCheck exercises the corpus machinery against synthetic
// images whose decoded contents are known exactly.
//
// The real corpus needs Hyper-V and qemu-img, so it cannot run everywhere. This
// keeps the scanning, pairing and comparison logic covered regardless, so a
// broken harness cannot sit silently green until someone has the tooling.
func TestCorpusHarnessSelfCheck(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	// A fixed VHD decodes to exactly its payload.
	const fixedSize = 8192
	write("fixed.vhd", buildFixedVHD(fixedSize, func(p []byte) {
		for i := range p {
			p[i] = byte(i % 251)
		}
	}))
	fixedRaw := make([]byte, fixedSize)
	for i := range fixedRaw {
		fixedRaw[i] = byte(i % 251)
	}
	write("fixed.raw", fixedRaw)

	// A differencing chain: the child holds 0xC0 in sectors 0, 2 and 4; every
	// other sector must come from the parent's 0xA0.
	parentImg, childImg, _ := diffVHDPair("chain-parent.vhd")
	write("chain-parent.vhd", parentImg) // no .raw: not the view under test
	write("chain-child.vhd", childImg)

	childRaw := make([]byte, chainMediaSize)
	for s := 0; s < int(chainMediaSize)/512; s++ {
		fill := byte(0xA0)
		if s == 0 || s == 2 || s == 4 {
			fill = 0xC0
		}
		for i := 0; i < 512; i++ {
			childRaw[s*512+i] = fill
		}
	}
	write("chain-child.raw", childRaw)

	// An image with no .raw counterpart must be ignored rather than failing.
	write("orphan.vhdx", buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
	}))
	// A non-image file must also be ignored.
	write("notes.txt", []byte("not an image"))

	images, err := corpusImages(dir)
	if err != nil {
		t.Fatalf("corpusImages: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("corpusImages returned %d image(s) (%v), want 2 (fixed.vhd, chain-child.vhd)",
			len(images), images)
	}

	for _, image := range images {
		t.Run(filepath.Base(image), func(t *testing.T) {
			d, err := OpenFile(image)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer d.Close()

			if d.NeedsParent() {
				t.Fatalf("incomplete chain: %v", d.ParentResolveError())
			}

			rf, err := os.Open(rawPathFor(image))
			if err != nil {
				t.Fatalf("opening raw dump: %v", err)
			}
			defer rf.Close()
			info, _ := rf.Stat()

			if err := compareStreams(t, d, rf, d.Size(), uint64(info.Size())); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestCorpusHarnessDetectsMismatch confirms compareStreams actually fails on a
// difference, so a passing corpus run means something.
func TestCorpusHarnessDetectsMismatch(t *testing.T) {
	const size = 8192
	img := buildFixedVHD(size, func(p []byte) {
		for i := range p {
			p[i] = 0xCC
		}
	})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	wrong := make([]byte, size)
	for i := range wrong {
		wrong[i] = 0xCC
	}
	wrong[5000] = 0x00 // single flipped byte

	err = compareStreams(t, d, bytes.NewReader(wrong), d.Size(), uint64(len(wrong)))
	if err == nil {
		t.Fatal("compareStreams accepted a one-byte mismatch")
	}
	if !strings.Contains(err.Error(), "5000") {
		t.Errorf("error %q does not identify the differing offset 5000", err)
	}
}

// corpusImages returns every image in dir that has a .raw counterpart.
func corpusImages(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var images []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".vhd" && ext != ".vhdx" {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if _, err := os.Stat(rawPathFor(full)); err != nil {
			continue // no ground truth for this image
		}
		images = append(images, full)
	}
	return images, nil
}

// rawPathFor maps an image path to its expected raw dump.
func rawPathFor(image string) string {
	return strings.TrimSuffix(image, filepath.Ext(image)) + ".raw"
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}
