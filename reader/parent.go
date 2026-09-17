// SPDX-License-Identifier: MIT

package reader

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoiflux/libvhdi/types"
)

// ErrParentNotFound is returned by a ParentResolver that exhausted its search
// without locating the requested parent image.
var ErrParentNotFound = errors.New("libvhdi: parent image not found")

// ErrParentMismatch is returned when a located parent does not match the
// identity recorded in the child.
var ErrParentMismatch = errors.New("libvhdi: parent image does not match child metadata")

// ErrChainTooDeep is returned when a parent chain exceeds Options.MaxChainDepth.
var ErrChainTooDeep = errors.New("libvhdi: parent chain exceeds maximum depth")

// ErrChainCycle is returned when a parent chain refers back to a disk already
// in the chain.
var ErrChainCycle = errors.New("libvhdi: parent chain contains a cycle")

// ParentRequest describes the parent image a differencing disk is looking for.
type ParentRequest struct {
	// ChildPath is the filesystem path of the child, when it was opened from
	// one. Empty for disks opened from a bare io.ReaderAt.
	ChildPath string

	// ChildDir is the directory containing the child, when known. Parents are
	// conventionally stored alongside or near their children, so this is the
	// first place to look.
	ChildDir string

	// ParentFilename is the path recorded in the child's metadata. It is
	// typically a Windows path and may be relative or absolute.
	ParentFilename string

	// ParentIdentifier is the GUID the parent must carry. Callers should not
	// need to check this themselves; the chain builder verifies it.
	ParentIdentifier [16]byte

	// Locators carries the child's parent locator entries, which may hold
	// additional candidate paths.
	Locators []types.ParentLocatorEntry

	// Format is the child's format. A parent is normally the same format.
	Format types.FileFormat

	// Depth is 0 for the immediate parent of the disk the caller opened, 1 for
	// its grandparent, and so on.
	Depth int
}

// ParentSource is an opened parent image returned by a ParentResolver.
type ParentSource struct {
	// ReaderAt is the parent's backing reader. Required.
	ReaderAt io.ReaderAt

	// Size is the parent file's total size in bytes. Required for VHD, whose
	// footer is located from the end of the file.
	Size int64

	// Closer, if non-nil, is closed when the child disk is closed. Resolvers
	// that open files should set this so the chain owns the handle.
	Closer io.Closer

	// Name is a human-readable identifier used in diagnostics.
	Name string

	// ModTime is the parent file's modification time, when the resolver can
	// supply one. It is optional; the zero value means "not known".
	//
	// A differencing child records its parent's modification time, and the
	// only way to check that record is against the file the resolver actually
	// found. Resolvers that stat a file should set this so the check can
	// happen; those that cannot simply leave it zero and the check is skipped.
	ModTime time.Time
}

// ParentResolver locates the parent image of a differencing disk.
//
// Implementations should return ErrParentNotFound (possibly wrapped) when the
// parent cannot be located, so callers can distinguish a missing parent from an
// I/O failure.
type ParentResolver interface {
	ResolveParent(req ParentRequest) (ParentSource, error)
}

// ParentResolverFunc adapts a function to the ParentResolver interface.
type ParentResolverFunc func(req ParentRequest) (ParentSource, error)

// ResolveParent implements ParentResolver.
func (f ParentResolverFunc) ResolveParent(req ParentRequest) (ParentSource, error) {
	return f(req)
}

// ============================================================================
// Candidate path derivation
// ============================================================================

// normalizeParentPath converts a path recorded by a Windows tool into slash
// form. VHD and VHDX both store parent paths in Windows convention regardless
// of the host that reads them.
func normalizeParentPath(s string) string {
	s = strings.ReplaceAll(s, `\`, "/")
	// Trim a UNC or extended-length prefix if present; only the tail is usable
	// for a local search.
	s = strings.TrimPrefix(s, "//?/")
	return path.Clean(s)
}

// isWindowsAbsolute reports whether p looks like an absolute Windows path
// ("C:/..." or "//server/share/..."), which cannot be joined onto a search
// directory.
func isWindowsAbsolute(p string) bool {
	if strings.HasPrefix(p, "//") {
		return true
	}
	if len(p) >= 3 && p[1] == ':' && p[2] == '/' {
		return true
	}
	return false
}

// parentCandidates returns the relative paths to try, most specific first, and
// the absolute paths to try as-is.
func parentCandidates(req ParentRequest) (relative, absolute []string) {
	seenRel := make(map[string]bool)
	seenAbs := make(map[string]bool)

	add := func(raw string) {
		if raw == "" {
			return
		}
		p := normalizeParentPath(raw)
		if p == "" || p == "." {
			return
		}

		if isWindowsAbsolute(p) || path.IsAbs(p) {
			if !seenAbs[p] {
				seenAbs[p] = true
				absolute = append(absolute, p)
			}
			// An absolute path recorded on another machine is usually stale, so
			// also try its basename relative to the search directories.
			if base := path.Base(p); base != "" && base != "." && !seenRel[base] {
				seenRel[base] = true
				relative = append(relative, base)
			}
			return
		}

		if !seenRel[p] {
			seenRel[p] = true
			relative = append(relative, p)
		}
		if base := path.Base(p); base != p && base != "" && base != "." && !seenRel[base] {
			seenRel[base] = true
			relative = append(relative, base)
		}
	}

	// The metadata filename is the primary hint; locator values are fallbacks.
	add(req.ParentFilename)
	for _, loc := range req.Locators {
		add(loc.Value)
	}

	return relative, absolute
}

// ============================================================================
// Filesystem-backed resolvers
// ============================================================================

type dirResolver struct {
	dirs []string
}

// DirParentResolver returns a ParentResolver that searches the child's own
// directory first, then each of the supplied directories, for the parent named
// in the child's metadata.
//
// For each directory it tries the recorded relative path and then the bare
// filename, so a chain that has been moved as a unit still resolves.
func DirParentResolver(dirs ...string) ParentResolver {
	return &dirResolver{dirs: dirs}
}

// ResolveParent implements ParentResolver.
func (d *dirResolver) ResolveParent(req ParentRequest) (ParentSource, error) {
	relative, absolute := parentCandidates(req)

	var attempts int
	try := func(p string) (ParentSource, bool) {
		attempts++
		f, err := os.Open(p)
		if err != nil {
			return ParentSource{}, false
		}
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			f.Close()
			return ParentSource{}, false
		}
		return ParentSource{
			ReaderAt: f,
			Size:     info.Size(),
			Closer:   f,
			Name:     p,
			ModTime:  info.ModTime(),
		}, true
	}

	// Absolute paths recorded in the image, in case the chain never moved.
	for _, abs := range absolute {
		if src, ok := try(filepath.FromSlash(abs)); ok {
			return src, nil
		}
	}

	for _, dir := range d.searchDirs(req) {
		for _, rel := range relative {
			if src, ok := try(filepath.Join(dir, filepath.FromSlash(rel))); ok {
				return src, nil
			}
		}
	}

	return ParentSource{}, fmt.Errorf("%w: %q (tried %d location(s))",
		ErrParentNotFound, req.ParentFilename, attempts)
}

func (d *dirResolver) searchDirs(req ParentRequest) []string {
	dirs := make([]string, 0, len(d.dirs)+1)
	seen := make(map[string]bool)
	appendDir := func(dir string) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}

	// The child's own directory is the conventional location.
	appendDir(req.ChildDir)
	for _, dir := range d.dirs {
		appendDir(dir)
	}
	return dirs
}

type fsResolver struct {
	fsys fs.FS
}

// FSParentResolver returns a ParentResolver that searches an fs.FS. Paths are
// interpreted as slash-separated and relative to the root of fsys, matching
// io/fs conventions, which makes it usable with embedded, archived or
// test filesystems.
func FSParentResolver(fsys fs.FS) ParentResolver {
	return &fsResolver{fsys: fsys}
}

// ResolveParent implements ParentResolver.
func (r *fsResolver) ResolveParent(req ParentRequest) (ParentSource, error) {
	relative, absolute := parentCandidates(req)

	// fs.FS paths are always relative and slash-separated, so absolute
	// candidates contribute only their basename, which parentCandidates has
	// already added to relative.
	_ = absolute

	var attempts int
	dirs := []string{""}
	if req.ChildDir != "" {
		dirs = []string{req.ChildDir, ""}
	}

	for _, dir := range dirs {
		for _, rel := range relative {
			name := rel
			if dir != "" {
				name = path.Join(dir, rel)
			}
			if !fs.ValidPath(name) {
				continue
			}
			attempts++

			f, err := r.fsys.Open(name)
			if err != nil {
				continue
			}
			ra, ok := f.(io.ReaderAt)
			if !ok {
				f.Close()
				continue
			}
			info, err := f.Stat()
			if err != nil || info.IsDir() {
				f.Close()
				continue
			}
			return ParentSource{
				ReaderAt: ra,
				Size:     info.Size(),
				Closer:   f,
				Name:     name,
				ModTime:  info.ModTime(),
			}, nil
		}
	}

	return ParentSource{}, fmt.Errorf("%w: %q (tried %d location(s))",
		ErrParentNotFound, req.ParentFilename, attempts)
}
