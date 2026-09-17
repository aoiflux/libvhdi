// SPDX-License-Identifier: MIT

package reader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// A Hyper-V checkpoint is not a format. It is a differencing disk, conventionally
// named with an .avhdx extension, whose parent is the disk as it stood when the
// checkpoint was taken. Everything needed to read one already worked; what was
// missing was the ability to see the shape of a set of them.
//
// Chain walks upward from one image to its ancestors. That is the wrong
// direction for checkpoints, because checkpoints branch: applying an earlier
// checkpoint and then continuing produces two children of the same parent, and
// from a leaf you cannot see the sibling. DiscoverChain reads a directory and
// builds the whole parent-to-children tree.

// ErrNotFound is returned when a node named by identity is not in the tree.
var ErrNotFound = errors.New("libvhdi: no such disk in the chain tree")

// Node is one image in a chain tree.
type Node struct {
	// Info is what the header-only probe read. It is the zero value when the
	// image could not be read; Err then says why.
	Info DiskInfo

	// Path is the file this node came from. It is set even when the probe
	// failed, since naming the unreadable file is the point.
	Path string

	// Err is non-nil when the image could not be probed. Such a node is kept in
	// the tree rather than dropped: an unreadable file sitting in a checkpoint
	// directory is a finding, and silently omitting it would make the tree look
	// complete when it is not.
	Err error

	// Parent is the node this image differences against, or nil for a root or
	// for a child whose parent is not in the scanned set.
	Parent *Node

	// Children are the images that difference against this one, ordered by
	// path so a scan is reproducible.
	Children []*Node
}

// Readable reports whether the image could be probed.
func (n *Node) Readable() bool { return n != nil && n.Err == nil }

// Role classifies this node's part in the chain.
func (n *Node) Role() Role {
	if n == nil || n.Err != nil {
		return RoleUnknown
	}
	return n.Info.Role()
}

// IsLeaf reports whether nothing differences against this image.
//
// A leaf is a disk that could be written to. On a tree with several leaves,
// which one a virtual machine is actually using is recorded in its
// configuration, not in the disks -- see Tree.Leaves.
func (n *Node) IsLeaf() bool { return n != nil && len(n.Children) == 0 }

// Depth returns how many images lie between this one and its root.
func (n *Node) Depth() int {
	d := 0
	for cur := n; cur != nil && cur.Parent != nil; cur = cur.Parent {
		d++
	}
	return d
}

// String names the node for a log line.
func (n *Node) String() string {
	if n == nil {
		return "<nil>"
	}
	name := filepath.Base(n.Path)
	if n.Err != nil {
		return name + " (unreadable)"
	}
	return fmt.Sprintf("%s (%s, %s)", name, n.Info.Format, n.Role())
}

// Tree is the parent-to-children graph of a set of images.
type Tree struct {
	// Dir is the directory that was scanned.
	Dir string

	// Nodes holds every image found, ordered by path.
	Nodes []*Node

	// byLink indexes readable nodes by the identity a child would use to name
	// them, which is the image's own GUID for VHD and its data-write GUID for
	// VHDX. Those are different fields, and conflating them is what makes a
	// chain silently fail to link up.
	byLink map[[16]byte]*Node

	// Skipped records files that were not images at all, with the reason. A
	// directory holds .vmcx, .vmrs, .bin and .vsv files alongside the disks, and
	// listing them as failures would bury the real ones.
	Skipped []string
}

// Roots returns the images nothing in the tree differences from, ordered by
// path.
//
// A root is usually the base disk. It can also be a differencing disk whose own
// parent was not in the scanned directory, which Node.Info.IsDifferencing
// distinguishes -- and that distinction matters, because such a root cannot be
// read to completion.
func (t *Tree) Roots() []*Node {
	var out []*Node
	for _, n := range t.Nodes {
		if n.Parent == nil {
			out = append(out, n)
		}
	}
	return out
}

// Leaves returns the images nothing differences against, ordered by path.
//
// More than one leaf means the checkpoint tree has branched. This library
// reports the branches and declines to guess which one a virtual machine is
// using: that fact lives in the machine's configuration, not in the disks, and
// a library that guessed would be wrong silently.
func (t *Tree) Leaves() []*Node {
	var out []*Node
	for _, n := range t.Nodes {
		if n.IsLeaf() {
			out = append(out, n)
		}
	}
	return out
}

// Branched reports whether the tree has more than one leaf.
func (t *Tree) Branched() bool { return len(t.Leaves()) > 1 }

// Find returns the node a child would reach by recording id as its parent
// identifier.
func (t *Tree) Find(id [16]byte) (*Node, error) {
	if n, ok := t.byLink[id]; ok {
		return n, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, binaryutil.GUIDToString(id))
}

// FindPath returns the node read from the given file.
func (t *Tree) FindPath(path string) (*Node, error) {
	want := normalizePath(path)
	for _, n := range t.Nodes {
		if normalizePath(n.Path) == want {
			return n, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
}

// Unreadable returns every node whose image could not be probed.
//
// These are kept rather than dropped because an unreadable file in a checkpoint
// directory is itself a finding: it may be the link that would have joined two
// halves of the tree.
func (t *Tree) Unreadable() []*Node {
	var out []*Node
	for _, n := range t.Nodes {
		if n.Err != nil {
			out = append(out, n)
		}
	}
	return out
}

// Lineage is one path through the tree, ordered from the root outwards.
//
// This is the set of files that together constitute one device, which is what
// an evidence record has to name -- and, read in order, the sequence of states
// the disk passed through.
type Lineage struct {
	// Nodes is the path, Nodes[0] being the root and the last entry the leaf.
	Nodes []*Node
}

// Leaf returns the outermost image, or nil for an empty lineage.
func (l Lineage) Leaf() *Node {
	if len(l.Nodes) == 0 {
		return nil
	}
	return l.Nodes[len(l.Nodes)-1]
}

// Root returns the innermost image, or nil for an empty lineage.
func (l Lineage) Root() *Node {
	if len(l.Nodes) == 0 {
		return nil
	}
	return l.Nodes[0]
}

// Complete reports whether the lineage starts at a disk that needs no parent.
//
// A false result means the chain is missing its base: the images present cannot
// reconstruct the device, and reads into the missing ranges fail with
// ErrParentRequired rather than returning zeroes.
func (l Lineage) Complete() bool {
	root := l.Root()
	if root == nil || root.Err != nil {
		return false
	}
	return !root.Info.IsDifferencing()
}

// Checkpoints returns the differencing images in the lineage, root-most first.
func (l Lineage) Checkpoints() []*Node {
	var out []*Node
	for _, n := range l.Nodes {
		if n.Role() == RoleCheckpoint {
			out = append(out, n)
		}
	}
	return out
}

// Paths returns each image's file path, root first.
func (l Lineage) Paths() []string {
	out := make([]string, 0, len(l.Nodes))
	for _, n := range l.Nodes {
		out = append(out, n.Path)
	}
	return out
}

// Open opens the lineage's leaf, which presents the merged view of every image
// below it.
//
// No new read logic is involved: opening the leaf resolves the chain and the
// result is the device as it stood at that point. This exists as a named entry
// point because "open the leaf" is not obvious from a tree, and because a
// branched tree has several leaves and no single current disk.
func (l Lineage) Open(opts *Options) (*VirtualDisk, error) {
	leaf := l.Leaf()
	if leaf == nil {
		return nil, errors.New("libvhdi: empty lineage has no disk to open")
	}
	if leaf.Err != nil {
		return nil, fmt.Errorf("opening lineage leaf %q: %w", leaf.Path, leaf.Err)
	}
	return OpenFileWith(leaf.Path, opts)
}

// Lineage returns the path from the tree's root down to n.
func (t *Tree) Lineage(n *Node) Lineage {
	if n == nil {
		return Lineage{}
	}
	var reversed []*Node
	for cur := n; cur != nil; cur = cur.Parent {
		reversed = append(reversed, cur)
	}
	nodes := make([]*Node, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		nodes = append(nodes, reversed[i])
	}
	return Lineage{Nodes: nodes}
}

// Lineages returns one lineage per leaf, so a branched tree yields every
// distinct device the directory describes.
func (t *Tree) Lineages() []Lineage {
	leaves := t.Leaves()
	out := make([]Lineage, 0, len(leaves))
	for _, leaf := range leaves {
		out = append(out, t.Lineage(leaf))
	}
	return out
}

// DiscoverOptions controls a directory scan. The zero value is usable.
type DiscoverOptions struct {
	// Recursive descends into subdirectories. Hyper-V keeps a machine's disks
	// in one folder by default, so the default is a single level.
	Recursive bool

	// Extensions limits which files are probed. Nil means the four disk
	// extensions: .vhd, .vhdx, .avhd and .avhdx.
	//
	// A renamed image is still a valid image, so pass the extensions in use
	// rather than expecting the library to sniff every file in the directory --
	// which on a machine folder would mean reading memory dumps.
	Extensions []string

	// MaxFiles bounds how many files are probed, guarding against being pointed
	// at a directory of a hundred thousand files. Zero means 4096.
	MaxFiles int
}

func (o *DiscoverOptions) extensions() []string {
	if o == nil || len(o.Extensions) == 0 {
		return []string{".vhd", ".vhdx", ".avhd", ".avhdx"}
	}
	out := make([]string, 0, len(o.Extensions))
	for _, e := range o.Extensions {
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, strings.ToLower(e))
	}
	return out
}

func (o *DiscoverOptions) maxFiles() int {
	const defaultMaxFiles = 4096
	if o == nil || o.MaxFiles <= 0 {
		return defaultMaxFiles
	}
	return o.MaxFiles
}

// DiscoverChain scans a directory of images and builds the parent-to-children
// tree they form.
//
// Only headers are read. A full open parses the block allocation table, whose
// size scales with the virtual disk -- a million entries for a 1 TB disk with
// 1 MB blocks -- and discovery needs none of it. A probe's own cost is fixed at
// two headers, up to two 64 KB region tables and the metadata region, so the
// saving is negligible on small images and large on the ones a checkpoint
// directory actually holds.
//
// A file that cannot be probed becomes a node carrying the error rather than
// failing the scan. One corrupt image in a directory must not prevent the rest
// of the tree from being described, and the unreadable file may be exactly what
// the examiner is looking for.
//
// Files without a disk extension are recorded in Tree.Skipped. A Hyper-V machine
// folder holds .vmcx, .vmrs, .bin and .vsv files alongside the disks, and
// reporting those as failures would bury the real ones.
func DiscoverChain(ctx context.Context, dir string, opts *DiscoverOptions) (*Tree, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("libvhdi: %q is not a directory", dir)
	}

	files, skipped, err := collectImageFiles(ctx, dir, opts)
	if err != nil {
		return nil, err
	}

	tree := &Tree{
		Dir:     dir,
		byLink:  make(map[[16]byte]*Node, len(files)),
		Skipped: skipped,
	}

	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		node := &Node{Path: path}
		probed, err := ProbeFile(path)
		if err != nil {
			node.Err = err
		} else {
			node.Info = probed
		}
		tree.Nodes = append(tree.Nodes, node)

		// Only readable nodes can be linked to, and a zero link identity names
		// nothing -- indexing it would make every image with no identifier
		// appear to be the parent of every other.
		if node.Err == nil && node.Info.LinkIdentity != ([16]byte{}) {
			if _, clash := tree.byLink[node.Info.LinkIdentity]; !clash {
				tree.byLink[node.Info.LinkIdentity] = node
			}
		}
	}

	linkTree(tree)
	return tree, nil
}

// linkTree joins children to parents by recorded identity.
//
// Identity is the only safe key. Matching on virtual size would attach any
// same-sized image, and matching on the recorded parent filename would follow a
// name that may have been reused -- both produce a plausible tree that is wrong.
// A child whose parent identifier names nothing in the set stays a root, which
// is the honest answer: its parent was not in the directory.
func linkTree(t *Tree) {
	for _, child := range t.Nodes {
		if child.Err != nil || !child.Info.IsDifferencing() {
			continue
		}
		want := child.Info.ParentIdentifier
		if want == ([16]byte{}) {
			continue
		}
		parent, ok := t.byLink[want]
		if !ok || parent == child {
			continue
		}
		child.Parent = parent
		parent.Children = append(parent.Children, child)
	}

	for _, n := range t.Nodes {
		sort.Slice(n.Children, func(i, j int) bool {
			return n.Children[i].Path < n.Children[j].Path
		})
	}
}

// collectImageFiles lists the files worth probing, plus those skipped.
func collectImageFiles(ctx context.Context, dir string, opts *DiscoverOptions) (files, skipped []string, err error) {
	exts := opts.extensions()
	max := opts.maxFiles()
	recursive := opts != nil && opts.Recursive

	wanted := func(name string) bool {
		got := strings.ToLower(filepath.Ext(name))
		for _, e := range exts {
			if got == e {
				return true
			}
		}
		return false
	}

	walk := func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && !recursive {
				return filepath.SkipDir
			}
			return nil
		}
		if !wanted(d.Name()) {
			skipped = append(skipped, path)
			return nil
		}
		if len(files) >= max {
			return fmt.Errorf("libvhdi: more than %d image files under %q; raise DiscoverOptions.MaxFiles",
				max, dir)
		}
		files = append(files, path)
		return nil
	}

	if err := filepath.WalkDir(dir, walk); err != nil {
		return nil, nil, err
	}

	sort.Strings(files)
	sort.Strings(skipped)
	return files, skipped, nil
}

// normalizePath renders a path for comparison, accounting for Windows's
// case-insensitive filesystem.
//
// This is the same normalisation chainKey applies, and for the same reason: the
// same file reached by differently-cased names has to produce one key, or a
// cycle looks like two distinct disks.
func normalizePath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// ============================================================================
// Split and segmented layouts
// ============================================================================

// ErrSplitImage is returned for a file that looks like one piece of a
// split or segmented disk.
//
// Neither the VHD nor the VHDX specification defines such a layout: a
// multi-file disk in these formats is always a differencing chain, and split
// images are a VMDK and VDI concept. A file named like a VMDK extent is
// therefore not a VHD this library is failing to read -- it is a different
// format, and saying so is more useful than "invalid signature".
var ErrSplitImage = fmt.Errorf("%w: split or segmented multi-file images are not a VHD/VHDX layout",
	types.ErrUnsupportedFeature)

// looksLikeSplitSegment reports whether a filename follows one of the
// conventions used by formats that do split a disk across files.
func looksLikeSplitSegment(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	switch ext {
	case ".vmdk", ".vdi", ".vmdk-s001", ".001", ".002":
		return true
	}

	// VMware writes "disk-s001.vmdk" for sparse segments and "disk-f001.vmdk"
	// for flat ones.
	if i := strings.LastIndex(stem, "-s"); i >= 0 && isAllDigits(stem[i+2:]) && len(stem[i+2:]) >= 3 {
		return true
	}
	if i := strings.LastIndex(stem, "-f"); i >= 0 && isAllDigits(stem[i+2:]) && len(stem[i+2:]) >= 3 {
		return true
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
