// SPDX-License-Identifier: MIT

package reader

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

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
	// ParentResolver locates parents for differencing disks. When nil, no
	// automatic resolution is attempted and the caller must use SetParent.
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

	// AllowDirtyImage opens a VHDX image whose log has not been replayed. Such
	// an image's block allocation table and metadata may be stale, so its
	// contents may not reflect the last committed state. Log replay is not
	// implemented, so this trades a hard failure for a documented risk; check
	// IsDirty on the result. Leaving this false refuses the image with
	// ErrDirtyImage.
	AllowDirtyImage bool
}

func (o *Options) allowDirty() bool {
	return o != nil && o.AllowDirtyImage
}

func (o *Options) maxChainDepth() int {
	if o == nil || o.MaxChainDepth <= 0 {
		return DefaultMaxChainDepth
	}
	return o.MaxChainDepth
}

func (o *Options) resolver() ParentResolver {
	if o == nil {
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
// When opts.ParentResolver is set, differencing parent chains are resolved
// automatically so the returned disk presents a correct contiguous device with
// no manual SetParent wiring.
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

	if err := d.resolveChain(opts); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
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
		return nil, err
	}
	d.closer = f
	d.path = name

	effective := opts
	if effective == nil {
		effective = &Options{}
	}
	if effective.ParentResolver == nil {
		// Copy so a caller's Options value is not mutated.
		clone := *effective
		clone.ParentResolver = DirParentResolver()
		effective = &clone
	}

	if err := d.resolveChain(effective); err != nil {
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

	// Track identifiers already in the chain so a self-referential or looping
	// set of images cannot be walked forever.
	seen := map[[16]byte]bool{d.Identifier(): true}

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
			if opts.RequireParentChain {
				return err
			}
			// Leave the chain incomplete; reads needing the parent fail closed.
			d.parentErr = err
			return nil
		}
		if src.ReaderAt == nil {
			err := fmt.Errorf("libvhdi: resolver returned a nil reader for %q", req.ParentFilename)
			if opts.RequireParentChain {
				return err
			}
			d.parentErr = err
			return nil
		}

		parent, err := openParent(src, current.Format(), opts.allowDirty())
		if err != nil {
			closeSource(src)
			if opts.RequireParentChain {
				return fmt.Errorf("opening parent %q: %w", src.Name, err)
			}
			d.parentErr = fmt.Errorf("opening parent %q: %w", src.Name, err)
			return nil
		}
		parent.closer = src.Closer
		parent.path = src.Name
		parent.ownedByChild = true

		if err := verifyParent(current, parent, opts); err != nil {
			parent.Close()
			if opts.RequireParentChain {
				return err
			}
			d.parentErr = err
			return nil
		}

		if seen[parent.Identifier()] {
			parent.Close()
			err := fmt.Errorf("%w: %s", ErrChainCycle, parent.GUIDString())
			if opts.RequireParentChain {
				return err
			}
			d.parentErr = err
			return nil
		}
		seen[parent.Identifier()] = true

		if err := current.SetParent(parent); err != nil {
			parent.Close()
			if opts.RequireParentChain {
				return err
			}
			d.parentErr = err
			return nil
		}

		current = parent
	}

	return nil
}

// openParent opens a resolved parent source, preferring the child's format but
// falling back to signature detection so a VHDX child with a VHD parent (or the
// reverse, which some tools produce) still opens.
func openParent(src ParentSource, childFormat types.FileFormat, allowDirty bool) (*VirtualDisk, error) {
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

// verifyParent checks that a resolved image really is the parent the child
// expects. Without this, any image of the right virtual size would be accepted,
// which would produce a plausible but wrong device.
func verifyParent(child, parent *VirtualDisk, opts *Options) error {
	if parent.Size() != child.Size() {
		return fmt.Errorf("%w: parent virtual size %d != child %d",
			ErrParentMismatch, parent.Size(), child.Size())
	}

	if opts != nil && opts.AllowParentGUIDMismatch {
		return nil
	}

	want := child.ParentIdentifier()
	if want == ([16]byte{}) {
		// The child records no parent identifier; nothing further to check.
		return nil
	}

	if got := parent.Identifier(); got != want {
		return fmt.Errorf("%w: parent identifier %s != expected %s",
			ErrParentMismatch,
			binaryutil.GUIDToString(got),
			binaryutil.GUIDToString(want))
	}
	return nil
}

func closeSource(src ParentSource) {
	if src.Closer != nil {
		src.Closer.Close()
	}
}
