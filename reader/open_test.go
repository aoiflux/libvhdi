package reader

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/aoiflux/libvhdi/types"
)

// ============================================================================
// Size derivation
// ============================================================================

// bareReaderAt exposes only ReadAt, with no Size, Stat or Seek, so size cannot
// be derived from it.
type bareReaderAt struct{ b []byte }

func (r bareReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestOpen_DerivesSizeFromBytesReader(t *testing.T) {
	img := buildFixedVHD(4096, func(p []byte) { copy(p, repeatByte(0xCC, 4096)) })

	// *bytes.Reader exposes Size().
	d, err := Open(bytes.NewReader(img), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if d.Size() != 4096 {
		t.Errorf("Size() = %d, want 4096", d.Size())
	}
}

func TestOpen_DerivesSizeFromFileStat(t *testing.T) {
	img := buildFixedVHD(4096, func(p []byte) { copy(p, repeatByte(0xCC, 4096)) })
	path := writeTempImage(t, "fixed.vhd", img)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("os.Open: %v", err)
	}
	defer f.Close()

	// *os.File exposes both Stat and Seek.
	d, err := Open(f, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if d.Size() != 4096 {
		t.Errorf("Size() = %d, want 4096", d.Size())
	}
}

func TestOpen_DerivesSizeFromSectionReader(t *testing.T) {
	img := buildFixedVHD(4096, func(p []byte) { copy(p, repeatByte(0xCC, 4096)) })

	// A VHD embedded at an offset inside a larger container, which is how a
	// forensic image layer typically presents a partition member.
	container := make([]byte, 8192+len(img))
	copy(container[8192:], img)
	sr := io.NewSectionReader(bytes.NewReader(container), 8192, int64(len(img)))

	d, err := Open(sr, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if d.Size() != 4096 {
		t.Errorf("Size() = %d, want 4096", d.Size())
	}
}

func TestOpen_BareReaderAtNeedsExplicitSizeForVHD(t *testing.T) {
	img := buildFixedVHD(4096, nil)

	if _, err := Open(bareReaderAt{img}, nil); !errors.Is(err, ErrSizeUnknown) {
		t.Fatalf("Open error = %v, want ErrSizeUnknown", err)
	}

	// Supplying the size explicitly must work.
	d, err := Open(bareReaderAt{img}, &Options{Size: int64(len(img))})
	if err != nil {
		t.Fatalf("Open with explicit size: %v", err)
	}
	defer d.Close()
	if d.Size() != 4096 {
		t.Errorf("Size() = %d, want 4096", d.Size())
	}
}

func TestOpen_VHDXNeedsNoSize(t *testing.T) {
	img := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
	})

	// VHDX locates every structure from fixed offsets, so a reader that cannot
	// report its length is still usable.
	d, err := Open(bareReaderAt{img}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if d.Size() != vhdxDiffVirtual {
		t.Errorf("Size() = %d, want %d", d.Size(), vhdxDiffVirtual)
	}
}

// ============================================================================
// Automatic parent chain resolution
// ============================================================================

func writeTempImage(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
	return p
}

// writeChainDir writes several images into one directory and returns it.
func writeChainDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	return dir
}

const (
	chainBlockSize = 2 * 1024 * 1024
	chainMediaSize = uint64(chainBlockSize)
)

// Distinct identifiers for each disk in a chain. Every disk needs its own so a
// child can name its parent unambiguously and cycle detection stays meaningful.
var (
	idBase  = [16]byte{0xB0, 0x5E}
	idMid   = [16]byte{0x1D, 0x1D}
	idTop   = [16]byte{0x70, 0x70}
	idOther = [16]byte{0xFE, 0xFE}
)

// diffVHDPair builds a parent/child VHD pair wired to each other, with the
// child holding 0xC0 in sectors 0, 2 and 4 and the parent 0xA0 everywhere.
func diffVHDPair(parentName string) (parentImg, childImg []byte, parentID [16]byte) {
	parentImg = buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: chainMediaSize,
		blockSize: chainBlockSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: allSectors(chainBlockSize / 512), data: repeatByte(0xA0, chainBlockSize)},
		},
		selfID: idBase,
	})

	childData := make([]byte, chainBlockSize)
	for _, s := range []int{0, 2, 4} {
		copy(childData[s*512:(s+1)*512], repeatByte(0xC0, 512))
	}
	childImg = buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDifferential,
		mediaSize: chainMediaSize,
		blockSize: chainBlockSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0, 2, 4}, data: childData},
		},
		parentName: parentName,
		parentID:   idBase,
		selfID:     idTop,
	})

	return parentImg, childImg, idBase
}

// TestOpenFileWith_AutoResolvesSiblingParent is the core integration case: a
// caller opens only the child and gets a correct contiguous device, with no
// manual SetParent wiring.
func TestOpenFileWith_AutoResolvesSiblingParent(t *testing.T) {
	parentImg, childImg, _ := diffVHDPair("parent.vhd")
	dir := writeChainDir(t, map[string][]byte{
		"parent.vhd": parentImg,
		"child.vhd":  childImg,
	})

	d, err := OpenFileWith(filepath.Join(dir, "child.vhd"), nil)
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("NeedsParent() = true after auto-resolution; ParentResolveError = %v", d.ParentResolveError())
	}
	if d.ChainDepth() != 2 {
		t.Errorf("ChainDepth() = %d, want 2", d.ChainDepth())
	}

	buf := make([]byte, 6*512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	for s := 0; s < 6; s++ {
		want := byte(0xA0)
		if s%2 == 0 {
			want = 0xC0
		}
		if got := buf[s*512]; got != want {
			t.Errorf("sector %d = %#x, want %#x", s, got, want)
		}
	}
}

// TestOpenFile_AutoResolvesSiblingParent pins that the plain OpenFile entry
// point resolves chains too. It is the obvious function to reach for, so leaving
// it without resolution would make a differencing disk fail closed by default
// even when its parent sits right beside it.
func TestOpenFile_AutoResolvesSiblingParent(t *testing.T) {
	parentImg, childImg, _ := diffVHDPair("parent.vhd")
	dir := writeChainDir(t, map[string][]byte{
		"parent.vhd": parentImg,
		"child.vhd":  childImg,
	})

	d, err := OpenFile(filepath.Join(dir, "child.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("OpenFile did not resolve the sibling parent: %v", d.ParentResolveError())
	}

	buf := make([]byte, 2*512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if buf[0] != 0xC0 {
		t.Errorf("sector 0 = %#x, want 0xC0 from child", buf[0])
	}
	if buf[512] != 0xA0 {
		t.Errorf("sector 1 = %#x, want 0xA0 from parent", buf[512])
	}
}

// TestOpenFile_NonDifferencingUnaffected guards the common path: a plain dynamic
// disk must not acquire any parent-resolution behaviour.
func TestOpenFile_NonDifferencingUnaffected(t *testing.T) {
	img := buildDynamicVHD(types.DiskTypeDynamic, chainMediaSize, chainBlockSize, []vhdBlock{
		{allocated: true, presentSectors: allSectors(chainBlockSize / 512), data: repeatByte(0x11, chainBlockSize)},
	}, "", [16]byte{})
	path := writeTempImage(t, "dynamic.vhd", img)

	d, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Error("NeedsParent() = true for a dynamic disk")
	}
	if d.ChainDepth() != 1 {
		t.Errorf("ChainDepth() = %d, want 1", d.ChainDepth())
	}
	if err := d.ParentResolveError(); err != nil {
		t.Errorf("ParentResolveError() = %v, want nil", err)
	}

	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if buf[0] != 0x11 {
		t.Errorf("data = %#x, want 0x11", buf[0])
	}
}

// TestOpenFileWith_ResolvesWindowsStyleRelativePath covers the path form real
// tools record: a Windows relative path with backslashes.
func TestOpenFileWith_ResolvesWindowsStyleRelativePath(t *testing.T) {
	parentImg, childImg, _ := diffVHDPair(`.\parent.vhd`)
	dir := writeChainDir(t, map[string][]byte{
		"parent.vhd": parentImg,
		"child.vhd":  childImg,
	})

	d, err := OpenFileWith(filepath.Join(dir, "child.vhd"), nil)
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("failed to resolve %q: %v", `.\parent.vhd`, d.ParentResolveError())
	}
}

// TestOpenFileWith_FallsBackToBasenameForStaleAbsolutePath covers a chain that
// has been copied off its original machine, so the recorded absolute path no
// longer exists.
func TestOpenFileWith_FallsBackToBasenameForStaleAbsolutePath(t *testing.T) {
	parentImg, childImg, _ := diffVHDPair(`C:\VMs\NoSuchDir\parent.vhd`)
	dir := writeChainDir(t, map[string][]byte{
		"parent.vhd": parentImg,
		"child.vhd":  childImg,
	})

	d, err := OpenFileWith(filepath.Join(dir, "child.vhd"), nil)
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("failed to fall back to basename: %v", d.ParentResolveError())
	}
}

// TestOpenFileWith_MissingParentIsBestEffort verifies that an unresolvable
// parent still opens, reports why, and fails closed on read rather than
// returning zeroes.
func TestOpenFileWith_MissingParentIsBestEffort(t *testing.T) {
	_, childImg, _ := diffVHDPair("parent.vhd")
	dir := writeChainDir(t, map[string][]byte{"child.vhd": childImg})

	d, err := OpenFileWith(filepath.Join(dir, "child.vhd"), nil)
	if err != nil {
		t.Fatalf("OpenFileWith should succeed best-effort: %v", err)
	}
	defer d.Close()

	if !d.NeedsParent() {
		t.Error("NeedsParent() = false, want true")
	}
	if !errors.Is(d.ParentResolveError(), ErrParentNotFound) {
		t.Errorf("ParentResolveError() = %v, want ErrParentNotFound", d.ParentResolveError())
	}

	// Sector 1 lives in the parent, so it must fail rather than read as zero.
	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 512); !errors.Is(err, ErrParentRequired) {
		t.Errorf("ReadAt error = %v, want ErrParentRequired", err)
	}
}

// TestOpenFileWith_RequireParentChainFailsOpen checks the opt-in strict mode.
func TestOpenFileWith_RequireParentChainFailsOpen(t *testing.T) {
	_, childImg, _ := diffVHDPair("parent.vhd")
	dir := writeChainDir(t, map[string][]byte{"child.vhd": childImg})

	_, err := OpenFileWith(filepath.Join(dir, "child.vhd"), &Options{RequireParentChain: true})
	if !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("OpenFileWith error = %v, want ErrParentNotFound", err)
	}
}

// TestOpenFileWith_RejectsWrongParentByGUID is the important negative case. A
// same-sized but unrelated image must not be accepted as the parent: doing so
// yields a plausible device made of the wrong bytes.
func TestOpenFileWith_RejectsWrongParentByGUID(t *testing.T) {
	const (
		blockSize = chainBlockSize
		mediaSize = chainMediaSize
	)

	// The child expects a parent identified by idOther, but the image sitting
	// next to it is idBase — a same-sized, plausible, wrong disk.
	childData := make([]byte, blockSize)
	copy(childData[0:512], repeatByte(0xC0, 512))
	childImg := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDifferential,
		mediaSize: mediaSize,
		blockSize: blockSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: childData},
		},
		parentName: "parent.vhd",
		parentID:   idOther,
		selfID:     idTop,
	})

	wrongParent := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: mediaSize,
		blockSize: blockSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0xA0, blockSize)},
		},
		selfID: idBase,
	})

	dir := writeChainDir(t, map[string][]byte{
		"parent.vhd": wrongParent,
		"child.vhd":  childImg,
	})

	d, err := OpenFileWith(filepath.Join(dir, "child.vhd"), nil)
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	if !d.NeedsParent() {
		t.Fatal("attached a parent whose identifier does not match the child")
	}
	if !errors.Is(d.ParentResolveError(), ErrParentMismatch) {
		t.Errorf("ParentResolveError() = %v, want ErrParentMismatch", d.ParentResolveError())
	}

	// The mismatch must be reported, not papered over with zeroes.
	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 512); !errors.Is(err, ErrParentRequired) {
		t.Errorf("ReadAt error = %v, want ErrParentRequired", err)
	}
}

// TestOpenFileWith_GUIDMismatchOverride checks the documented escape hatch.
func TestOpenFileWith_GUIDMismatchOverride(t *testing.T) {
	const (
		blockSize = chainBlockSize
		mediaSize = chainMediaSize
	)

	childData := make([]byte, blockSize)
	copy(childData[0:512], repeatByte(0xC0, 512))
	childImg := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDifferential,
		mediaSize: mediaSize,
		blockSize: blockSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: childData},
		},
		parentName: "parent.vhd",
		parentID:   idOther,
		selfID:     idTop,
	})

	parentImg := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: mediaSize,
		blockSize: blockSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0xA0, blockSize)},
		},
		selfID: idBase,
	})

	dir := writeChainDir(t, map[string][]byte{
		"parent.vhd": parentImg,
		"child.vhd":  childImg,
	})

	d, err := OpenFileWith(filepath.Join(dir, "child.vhd"), &Options{AllowParentGUIDMismatch: true})
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("override did not attach the parent: %v", d.ParentResolveError())
	}
	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 512); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if buf[0] != 0xA0 {
		t.Errorf("sector 1 = %#x, want 0xA0 from parent", buf[0])
	}
}

// TestOpenFileWith_ThreeDeepChain exercises a grandparent, so each layer's
// sector-level resolution has to compose.
func TestOpenFileWith_ThreeDeepChain(t *testing.T) {
	const (
		blockSize = chainBlockSize
		mediaSize = chainMediaSize
	)
	sectors := blockSize / 512

	// base holds 0xA0 everywhere.
	base := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: mediaSize,
		blockSize: blockSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: allSectors(sectors), data: repeatByte(0xA0, blockSize)},
		},
		selfID: idBase,
	})

	// mid is a differencing disk over base, holding 0xB0 in sectors 0 and 1.
	midData := make([]byte, blockSize)
	copy(midData[0:512], repeatByte(0xB0, 512))
	copy(midData[512:1024], repeatByte(0xB0, 512))
	mid := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDifferential,
		mediaSize: mediaSize,
		blockSize: blockSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0, 1}, data: midData},
		},
		parentName: "base.vhd",
		parentID:   idBase,
		selfID:     idMid,
	})

	// top is a differencing disk over mid, holding 0xC0 in sector 0 only.
	topData := make([]byte, blockSize)
	copy(topData[0:512], repeatByte(0xC0, 512))
	top := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDifferential,
		mediaSize: mediaSize,
		blockSize: blockSize,
		blocks: []vhdBlock{
			{allocated: true, presentSectors: []int{0}, data: topData},
		},
		parentName: "mid.vhd",
		parentID:   idMid,
		selfID:     idTop,
	})

	dir := writeChainDir(t, map[string][]byte{
		"base.vhd": base,
		"mid.vhd":  mid,
		"top.vhd":  top,
	})

	d, err := OpenFileWith(filepath.Join(dir, "top.vhd"), nil)
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	defer d.Close()

	if d.NeedsParent() {
		t.Fatalf("chain incomplete: %v", d.ParentResolveError())
	}
	if got := d.ChainDepth(); got != 3 {
		t.Errorf("ChainDepth() = %d, want 3", got)
	}

	buf := make([]byte, 3*512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	// Sector 0 from top, sector 1 from mid, sector 2 from base.
	for i, want := range []byte{0xC0, 0xB0, 0xA0} {
		if got := buf[i*512]; got != want {
			t.Errorf("sector %d = %#x, want %#x", i, got, want)
		}
	}
}

// TestOpenFileWith_ChainDepthLimit verifies the depth guard fires.
func TestOpenFileWith_ChainDepthLimit(t *testing.T) {
	parentImg, childImg, _ := diffVHDPair("parent.vhd")
	dir := writeChainDir(t, map[string][]byte{
		"parent.vhd": parentImg,
		"child.vhd":  childImg,
	})

	_, err := OpenFileWith(filepath.Join(dir, "child.vhd"), &Options{
		MaxChainDepth:      1,
		RequireParentChain: true,
	})
	if !errors.Is(err, ErrChainTooDeep) {
		t.Fatalf("error = %v, want ErrChainTooDeep", err)
	}
}

// TestCloseReleasesAutoResolvedChain checks that parents opened on the caller's
// behalf are closed with the child, while a hand-attached parent is not.
func TestCloseReleasesAutoResolvedChain(t *testing.T) {
	parentImg, childImg, _ := diffVHDPair("parent.vhd")
	dir := writeChainDir(t, map[string][]byte{
		"parent.vhd": parentImg,
		"child.vhd":  childImg,
	})

	d, err := OpenFileWith(filepath.Join(dir, "child.vhd"), nil)
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	if d.NeedsParent() {
		t.Fatalf("chain incomplete: %v", d.ParentResolveError())
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The parent's handle is closed too, so reading through it now fails
	// instead of returning stale bytes.
	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 512); err == nil {
		t.Error("ReadAt succeeded after Close; parent handle was not released")
	}
}

// TestSetParentKeepsCallerOwnership verifies Close does not reach into a parent
// the caller opened and still owns.
func TestSetParentKeepsCallerOwnership(t *testing.T) {
	parentImg, childImg, _ := diffVHDPair("parent.vhd")

	parent, err := OpenVHD(bytes.NewReader(parentImg), int64(len(parentImg)))
	if err != nil {
		t.Fatalf("OpenVHD(parent): %v", err)
	}
	defer parent.Close()

	child, err := OpenVHD(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		t.Fatalf("OpenVHD(child): %v", err)
	}
	if err := child.SetParent(parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := child.Close(); err != nil {
		t.Fatalf("child.Close: %v", err)
	}

	// The caller's parent is untouched and still readable.
	buf := make([]byte, 512)
	if _, err := parent.ReadAt(buf, 0); err != nil {
		t.Errorf("parent unusable after child.Close: %v", err)
	}
}

// ============================================================================
// fs.FS resolver
// ============================================================================

func TestFSParentResolver(t *testing.T) {
	parentImg, childImg, _ := diffVHDPair("parent.vhd")

	fsys := fstest.MapFS{
		"parent.vhd": &fstest.MapFile{Data: parentImg},
	}

	d, err := Open(bytes.NewReader(childImg), &Options{
		ParentResolver:     FSParentResolver(fsys),
		RequireParentChain: true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	buf := make([]byte, 2*512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if buf[0] != 0xC0 {
		t.Errorf("sector 0 = %#x, want 0xC0 from child", buf[0])
	}
	if buf[512] != 0xA0 {
		t.Errorf("sector 1 = %#x, want 0xA0 from parent", buf[512])
	}
}

// TestParentResolverFuncReceivesMetadata checks the request handed to a custom
// resolver carries what it needs to find the image.
func TestParentResolverFuncReceivesMetadata(t *testing.T) {
	parentImg, childImg, parentID := diffVHDPair(`.\subdir\parent.vhd`)

	var got ParentRequest
	resolver := ParentResolverFunc(func(req ParentRequest) (ParentSource, error) {
		got = req
		return ParentSource{ReaderAt: bytes.NewReader(parentImg), Size: int64(len(parentImg))}, nil
	})

	d, err := Open(bytes.NewReader(childImg), &Options{
		ParentResolver:     resolver,
		RequireParentChain: true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if !strings.Contains(got.ParentFilename, "parent.vhd") {
		t.Errorf("ParentFilename = %q, want it to contain parent.vhd", got.ParentFilename)
	}
	if got.ParentIdentifier != parentID {
		t.Errorf("ParentIdentifier = %x, want %x", got.ParentIdentifier, parentID)
	}
	if got.Format != types.FileFormatVHD {
		t.Errorf("Format = %v, want VHD", got.Format)
	}
	if got.Depth != 0 {
		t.Errorf("Depth = %d, want 0", got.Depth)
	}
}

// ============================================================================
// Path normalisation
// ============================================================================

func TestNormalizeParentPath(t *testing.T) {
	cases := map[string]string{
		`parent.vhd`:             "parent.vhd",
		`.\parent.vhd`:           "parent.vhd",
		`..\..\disks\parent.vhd`: "../../disks/parent.vhd",
		`C:\VMs\parent.vhd`:      "C:/VMs/parent.vhd",
		`\\?\C:\VMs\parent.vhd`:  "C:/VMs/parent.vhd",
		`subdir\.\other\p.vhdx`:  "subdir/other/p.vhdx",
	}
	for in, want := range cases {
		if got := normalizeParentPath(in); got != want {
			t.Errorf("normalizeParentPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParentCandidatesPrefersRecordedPathThenBasename(t *testing.T) {
	rel, abs := parentCandidates(ParentRequest{
		ParentFilename: `..\disks\parent.vhd`,
		Locators: []types.ParentLocatorEntry{
			{Key: "W2ku", Value: `D:\Other\alt.vhd`},
		},
	})

	if len(rel) < 2 || rel[0] != "../disks/parent.vhd" || rel[1] != "parent.vhd" {
		t.Errorf("relative candidates = %v, want recorded path then basename", rel)
	}
	if len(abs) != 1 || abs[0] != "D:/Other/alt.vhd" {
		t.Errorf("absolute candidates = %v, want the locator's absolute path", abs)
	}
	// The absolute locator's basename must also be reachable.
	found := false
	for _, r := range rel {
		if r == "alt.vhd" {
			found = true
		}
	}
	if !found {
		t.Errorf("relative candidates = %v, want alt.vhd basename fallback", rel)
	}
}
