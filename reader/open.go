// SPDX-License-Identifier: MIT

package reader

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// ErrSizeUnknown is returned when a disk's total file size can neither be
// derived from the supplied reader nor was given explicitly.
var ErrSizeUnknown = errors.New("libvhdi: cannot determine file size; set Options.Size")

// DefaultMaxChainDepth bounds how many disks a differencing chain may contain,
// counting the child itself.
const DefaultMaxChainDepth = 32

// Options controls how a disk is opened.
//
// The zero value is safe: parent GUIDs are verified, dirty images are refused,
// and differencing disks with no parent fail closed on read. Every field that
// relaxes a check is named so that false — the zero value — is the strict
// setting.
type Options struct {
	// ParentResolver locates parents for differencing disks.
	//
	// Nil means the default: a resolver that searches the image's own
	// directory, which is where a differencing chain's parents conventionally
	// live. Set it to NoParentResolution to suppress resolution entirely and
	// attach parents by hand with SetParent.
	ParentResolver ParentResolver

	// Size is the total size of the image in bytes. When zero, Open derives it
	// from the reader. Required for VHD images whose reader exposes neither a
	// size nor a seek nor a stat method.
	Size int64

	// MaxChainDepth bounds the length of a differencing chain, counting the
	// child. Zero means DefaultMaxChainDepth.
	MaxChainDepth int

	// AllowParentGUIDMismatch skips verification that a resolved parent's
	// identifier matches the one recorded in the child. Leaving this false
	// prevents an unrelated image of the right size from being read as the
	// parent.
	AllowParentGUIDMismatch bool

	// RequireParentChain makes Open fail when a differencing disk's parent
	// cannot be resolved. By default resolution is best-effort: the disk opens
	// with NeedsParent reporting true and reads that need the parent failing
	// with ErrParentRequired.
	RequireParentChain bool

	// AllowDirtyImage opens a VHDX image whose log could not be replayed.
	//
	// Log replay is implemented and is attempted automatically: a replayable log
	// is applied into a read-only in-memory overlay, leaving the file itself
	// byte-identical, and such an image is not dirty. This option covers the
	// remaining case, where the log is present but unreplayable -- a torn write,
	// a truncated buffer, a broken sequence. Then the block allocation table and
	// metadata may be stale and the contents may not reflect the last committed
	// state.
	//
	// Setting it trades a hard failure for a documented risk. The result reports
	// IsDirty, and LogReplayed distinguishes an image that needed no replay from
	// one whose replay was skipped. Leaving it false refuses the image with
	// ErrDirtyImage.
	AllowDirtyImage bool
}

func (o *Options) allowDirty() bool {
	return o != nil && o.AllowDirtyImage
}

// requireParentChain reports whether an unresolvable chain must fail the open.
//
// Every Options accessor has to tolerate a nil receiver: nil is a documented
// argument to Open and OpenFileWith, and resolveChain reaches these from both.
func (o *Options) requireParentChain() bool {
	return o != nil && o.RequireParentChain
}

func (o *Options) maxChainDepth() int {
	if o == nil || o.MaxChainDepth <= 0 {
		return DefaultMaxChainDepth
	}
	return o.MaxChainDepth
}

// NoParentResolution suppresses automatic parent chain resolution.
//
// It exists so that nil can mean "the default" in Options.ParentResolver
// without leaving a caller who genuinely wants no resolution unable to say so.
// Before v0.3.0 nil meant no resolution in Open but the default in OpenFile,
// which is the sort of difference that is discovered by a differencing disk
// quietly failing to resolve rather than by reading the documentation.
//
// A disk opened this way reports NeedsParent, and reads that resolve to the
// missing parent fail with ErrParentRequired rather than returning zeroes.
var NoParentResolution ParentResolver = noParentResolution{}

type noParentResolution struct{}

func (noParentResolution) ResolveParent(ParentRequest) (ParentSource, error) {
	return ParentSource{}, ErrParentNotFound
}

// resolver returns the resolver to use, applying the nil-means-default rule.
func (o *Options) resolver() ParentResolver {
	if o == nil || o.ParentResolver == nil {
		return DirParentResolver()
	}
	if _, suppressed := o.ParentResolver.(noParentResolution); suppressed {
		return nil
	}
	return o.ParentResolver
}

// ============================================================================
// Size derivation
// ============================================================================

// sizer matches readers that report their own length, such as *bytes.Reader,
// *strings.Reader and io.SectionReader.
type sizer interface {
	Size() int64
}

// statter matches readers that expose file metadata, such as *os.File and
// fs.File.
type statter interface {
	Stat() (fs.FileInfo, error)
}

// deriveSize determines the total byte length of r.
//
// VHDX locates every structure from fixed offsets and does not need this, but a
// VHD footer lives in the final 512 bytes and cannot be found without it.
func deriveSize(r io.ReaderAt) (int64, error) {
	if s, ok := r.(sizer); ok {
		if n := s.Size(); n > 0 {
			return n, nil
		}
	}

	if s, ok := r.(statter); ok {
		if info, err := s.Stat(); err == nil {
			if n := info.Size(); n > 0 {
				return n, nil
			}
		}
	}

	// Seeking is a last resort: it mutates the reader's own offset, which
	// ReadAt does not use, so restore it afterwards.
	if s, ok := r.(io.Seeker); ok {
		if cur, err := s.Seek(0, io.SeekCurrent); err == nil {
			if end, err := s.Seek(0, io.SeekEnd); err == nil {
				if _, err := s.Seek(cur, io.SeekStart); err == nil && end > 0 {
					return end, nil
				}
			}
		}
	}

	return 0, ErrSizeUnknown
}

// ============================================================================
// Open
// ============================================================================

// detectFormat sniffs the signature at offset 0.
func detectFormat(r io.ReaderAt) (types.FileFormat, error) {
	sig := make([]byte, 8)
	if _, err := r.ReadAt(sig, 0); err != nil {
		return types.FileFormatUnknown, err
	}
	if string(sig) == types.VHDXFileSignature {
		return types.FileFormatVHDX, nil
	}
	return types.FileFormatVHD, nil
}

// Open opens a VHD or VHDX image from r, detecting the format automatically.
//
// The image size is taken from opts.Size when set, and otherwise derived from
// r: readers exposing Size, Stat or Seek are all handled, which covers
// *os.File, *bytes.Reader, *strings.Reader, io.SectionReader and fs.File. A
// bare io.ReaderAt with none of those must supply opts.Size for VHD images.
//
// Differencing parent chains are resolved automatically, exactly as OpenFile
// does. When r names a file -- an *os.File does -- its directory is searched,
// since that is where a chain's parents conventionally live. Set
// opts.ParentResolver to change where the search looks, or to
// NoParentResolution to suppress it and attach parents by hand.
//
// Resolution is best-effort unless opts.requireParentChain() is set: a chain that
// cannot be completed still opens, with NeedsParent reporting true and reads
// that need the parent failing with ErrParentRequired rather than returning
// zeroes.
//
// opts may be nil, which selects the documented defaults.
func Open(r io.ReaderAt, opts *Options) (*VirtualDisk, error) {
	if r == nil {
		return nil, errors.New("libvhdi: reader must not be nil")
	}

	format, err := detectFormat(r)
	if err != nil {
		return nil, err
	}

	size := int64(0)
	if opts != nil {
		size = opts.Size
	}
	if size <= 0 {
		derived, err := deriveSize(r)
		if err != nil {
			// VHDX does not need a size; only VHD locates data from the end.
			if format == types.FileFormatVHD {
				return nil, err
			}
		} else {
			size = derived
		}
	}

	var d *VirtualDisk
	if format == types.FileFormatVHDX {
		d, err = openVHDX(r, size, opts.allowDirty())
	} else {
		d, err = OpenVHD(r, size)
	}
	if err != nil {
		return nil, err
	}

	// A reader that names a file gives the chain search somewhere to look, and
	// is what makes Open(f, nil) behave the same as OpenFile(f.Name()).
	d.path = readerPath(r)

	if err := d.resolveChain(opts); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// namer matches readers that know their own path, which *os.File does.
type namer interface {
	Name() string
}

// readerPath reports the file r was opened from, or "" when r cannot say.
//
// A path is not required for reading -- every structure is located by offset --
// but without one a differencing disk has no directory to search for its
// parents, and cycle detection loses its most faithful key.
func readerPath(r io.ReaderAt) string {
	n, ok := r.(namer)
	if !ok {
		return ""
	}
	return n.Name()
}

// OpenFileWith opens a VHD or VHDX file by path with explicit options.
//
// When opts.ParentResolver is nil, a resolver searching the image's own
// directory is used, since that is where a differencing chain's parents
// conventionally live.
func OpenFileWith(name string, opts *Options) (*VirtualDisk, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	format, err := detectFormat(f)
	if err != nil {
		f.Close()
		return nil, err
	}

	var d *VirtualDisk
	if format == types.FileFormatVHDX {
		d, err = openVHDX(f, info.Size(), opts.allowDirty())
	} else {
		d, err = OpenVHD(f, info.Size())
	}
	if err != nil {
		f.Close()
		// A file named like a segment of a split disk is not a VHD this library
		// is failing to read: neither specification defines a multi-file layout,
		// so it is a different format entirely. Saying that is more useful than
		// reporting an invalid signature and leaving the caller to work out why
		// their "disk" will not open.
		if looksLikeSplitSegment(name) {
			return nil, fmt.Errorf("%w: %q", ErrSplitImage, filepath.Base(name))
		}
		return nil, err
	}
	d.closer = f
	d.path = name

	if err := d.resolveChain(opts); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// ============================================================================
// Parent chain construction
// ============================================================================

// resolveChain walks the differencing chain from d outwards, attaching each
// parent as it is resolved.
//
// Resolution is best-effort unless Options.RequireParentChain is set: a chain
// that cannot be completed leaves the disk readable wherever the child has data
// and failing with ErrParentRequired everywhere else, rather than silently
// substituting zeroes.
func (d *VirtualDisk) resolveChain(opts *Options) error {
	resolver := opts.resolver()
	if resolver == nil {
		return nil
	}

	maxDepth := opts.maxChainDepth()

	// Track disks already in the chain so a self-referential or looping set of
	// images cannot be walked forever.
	seen := map[string]bool{}
	if k := chainKey(d); k != "" {
		seen[k] = true
	}

	current := d
	for depth := 0; current.IsDifferencing(); depth++ {
		if depth+2 > maxDepth {
			return fmt.Errorf("%w: %d", ErrChainTooDeep, maxDepth)
		}

		req := ParentRequest{
			ChildPath:        current.path,
			ParentFilename:   current.ParentFilename(),
			ParentIdentifier: current.ParentIdentifier(),
			Locators:         current.ParentLocators(),
			Format:           current.Format(),
			Depth:            depth,
		}
		if current.path != "" {
			req.ChildDir = filepath.Dir(current.path)
		}

		src, err := resolver.ResolveParent(req)
		if err != nil {
			if opts.requireParentChain() {
				return err
			}
			// Leave the chain incomplete; reads needing the parent fail closed.
			d.parentErr = err
			return nil
		}
		if src.ReaderAt == nil {
			err := fmt.Errorf("libvhdi: resolver returned a nil reader for %q", req.ParentFilename)
			if opts.requireParentChain() {
				return err
			}
			d.parentErr = err
			return nil
		}

		parent, err := openParent(src, opts.allowDirty())
		if err != nil {
			closeSource(src)
			if opts.requireParentChain() {
				return fmt.Errorf("opening parent %q: %w", src.Name, err)
			}
			d.parentErr = fmt.Errorf("opening parent %q: %w", src.Name, err)
			return nil
		}
		parent.closer = src.Closer
		parent.path = src.Name
		parent.ownedByChild = true

		if err := verifyParent(current, parent, src, opts); err != nil {
			parent.Close()
			if opts.requireParentChain() {
				return err
			}
			d.parentErr = err
			return nil
		}

		if key := chainKey(parent); key != "" && seen[key] {
			parent.Close()
			err := fmt.Errorf("%w: %s", ErrChainCycle, key)
			if opts.requireParentChain() {
				return err
			}
			d.parentErr = err
			return nil
		} else if key != "" {
			seen[key] = true
		}

		if err := current.SetParent(parent); err != nil {
			parent.Close()
			if opts.requireParentChain() {
				return err
			}
			d.parentErr = err
			return nil
		}

		current = parent
	}

	return nil
}

// openParent opens a resolved parent source.
//
// The format is taken from the parent's own signature rather than assumed from
// the child's, so a VHDX child with a VHD parent -- which some tools produce --
// still opens.
func openParent(src ParentSource, allowDirty bool) (*VirtualDisk, error) {
	format, err := detectFormat(src.ReaderAt)
	if err != nil {
		return nil, err
	}

	size := src.Size
	if size <= 0 {
		if derived, err := deriveSize(src.ReaderAt); err == nil {
			size = derived
		} else if format == types.FileFormatVHD {
			return nil, ErrSizeUnknown
		}
	}

	if format == types.FileFormatVHDX {
		return openVHDX(src.ReaderAt, size, allowDirty)
	}
	return OpenVHD(src.ReaderAt, size)
}

// chainKey identifies a disk for cycle detection.
//
// The disk GUID alone is not a safe key. A VHDX that omits the Virtual Disk
// Identifier metadata item reports the zero GUID, and images cloned by copying a
// file share one, so two genuinely different disks can collide and be reported
// as a cycle that is not there. A cycle means revisiting the same file, so the
// backing path is the more faithful signal wherever one exists.
//
// An empty result means the disk cannot be identified; the caller then relies on
// MaxChainDepth to bound the walk rather than guessing.
func chainKey(d *VirtualDisk) string {
	if d.path != "" {
		path := d.path
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		path = filepath.Clean(path)
		if runtime.GOOS == "windows" {
			// Windows paths are case-insensitive, so the same file reached by
			// differently-cased names must produce one key.
			path = strings.ToLower(path)
		}
		return "path:" + path
	}
	if id := d.Identifier(); id != ([16]byte{}) {
		return "guid:" + binaryutil.GUIDToString(id)
	}
	return ""
}

// verifyParent checks that a resolved image really is the parent the child
// expects. Without this, any image of the right virtual size would be accepted,
// which would produce a plausible but wrong device.
//
// src carries what the resolver learned about the file itself, which is where
// the parent's modification time comes from. Checks that cannot be decisive are
// recorded as warnings on the child rather than failing the open, so a caller
// gets the image and the caveat instead of one or the other.
func verifyParent(child, parent *VirtualDisk, src ParentSource, opts *Options) error {
	if parent.Size() != child.Size() {
		return fmt.Errorf("%w: parent virtual size %d != child %d",
			ErrParentMismatch, parent.Size(), child.Size())
	}

	child.checkParentTimestamp(src)

	if opts != nil && opts.AllowParentGUIDMismatch {
		child.warn(WarningParentIdentityUnchecked,
			"parent %q accepted without identity verification, because AllowParentGUIDMismatch is set",
			src.Name)
		return nil
	}

	want := child.ParentIdentifier()
	if want == ([16]byte{}) {
		// The child records no parent identifier, so the only check this parent
		// could be held to was virtual size -- which any image of the same size
		// satisfies. That is exactly the condition that let a wrong parent be
		// attached before v0.3.0, so it is worth saying out loud even when it
		// is the image's own fault rather than the library's.
		child.warn(WarningParentIdentityUnverifiable,
			"child records no parent identifier, so parent %q was matched on virtual size alone",
			src.Name)
		return nil
	}

	if got := parent.parentLinkIdentity(); got != want {
		return fmt.Errorf("%w: parent %s identifier %s != the %s the child records",
			ErrParentMismatch,
			parent.Format(),
			binaryutil.GUIDToString(got),
			binaryutil.GUIDToString(want))
	}
	return nil
}

// checkParentTimestamp compares the parent modification time a VHD child
// records against the file the resolver actually found.
//
// A mismatch means the parent has been written to since the child was created,
// which makes the reconstructed device wrong in a way no other check detects:
// the identifier still matches, the size still matches, and the blocks the
// child does not override now hold different data than they did.
//
// It is only ever a warning. Filesystem modification times do not survive a
// copy, so a chain moved between machines mismatches routinely and is still the
// right chain. Failing the open here would refuse far more good images than bad
// ones.
func (d *VirtualDisk) checkParentTimestamp(src ParentSource) {
	if d.dynHeader == nil {
		// VHDX records no parent timestamp; there is nothing to compare.
		return
	}
	recorded := d.dynHeader.ParentModTime
	if recorded.IsZero() || src.ModTime.IsZero() {
		return
	}

	// VHD stores whole seconds, so compare at that resolution.
	actual := src.ModTime.UTC().Truncate(time.Second)
	if recorded.Equal(actual) {
		return
	}

	d.warn(WarningParentTimestampMismatch,
		"child records parent %q modified at %s, but the file found is dated %s",
		src.Name,
		recorded.Format(time.RFC3339),
		actual.Format(time.RFC3339))
}

func closeSource(src ParentSource) {
	if src.Closer != nil {
		src.Closer.Close()
	}
}
