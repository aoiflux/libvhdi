// SPDX-License-Identifier: MIT

package change_test

import (
	"context"
	"fmt"

	"github.com/aoiflux/libvhdi/vhdimap"
)

// fakeFS implements vhdimap.Filesystem over a map.
//
// It exists so the diff engine can be tested without an image of any kind. If
// only a disk-backed parser could satisfy these interfaces, every test of the
// classification logic would need a real filesystem to be built first, and the
// interesting cases -- a reused file number, a rename with no journal, a volume
// identity that disagrees -- are exactly the ones that are hard to produce on
// demand on a real volume.
type fakeFS struct {
	name     string
	identity string
	caps     vhdimap.Capabilities
	files    map[vhdimap.FileID]fakeFile

	identityErr error
	extentErr   map[vhdimap.FileID]error
}

type fakeFile struct {
	entry   vhdimap.FileEntry
	extents []vhdimap.FileExtent
}

var _ vhdimap.Filesystem = (*fakeFS)(nil)

func newFake(name, identity string) *fakeFS {
	return &fakeFS{
		name:     name,
		identity: identity,
		files:    make(map[vhdimap.FileID]fakeFile),
		caps: vhdimap.Capabilities{
			Name:           name,
			StableIdentity: true,
			HasGeneration:  true,
		},
	}
}

// add places a file on the volume at one contiguous disk range.
func (f *fakeFS) add(number, generation uint64, path string, parent uint64, diskOffset, length int64) *fakeFS {
	id := vhdimap.FileID{Number: number, Generation: generation}
	file := fakeFile{
		entry: vhdimap.FileEntry{
			ID:       id,
			ParentID: vhdimap.FileID{Number: parent},
			Name:     baseOf(path),
			Path:     path,
			Size:     length,
		},
	}
	if length > 0 {
		file.extents = []vhdimap.FileExtent{{
			ByteRange:  vhdimap.ByteRange{Offset: diskOffset, Length: length},
			FileOffset: 0,
		}}
	}
	f.files[id] = file
	return f
}

// addSparse places a file whose first run is a hole, so that file-relative
// offsets cannot be derived by accumulating run lengths.
func (f *fakeFS) addSparse(number, generation uint64, path string, holeLen, diskOffset, length int64) *fakeFS {
	id := vhdimap.FileID{Number: number, Generation: generation}
	f.files[id] = fakeFile{
		entry: vhdimap.FileEntry{
			ID: id, Name: baseOf(path), Path: path, Size: holeLen + length,
		},
		extents: []vhdimap.FileExtent{
			{FileOffset: 0},
			{
				ByteRange:  vhdimap.ByteRange{Offset: diskOffset, Length: length},
				FileOffset: holeLen,
			},
		},
	}
	return f
}

func (f *fakeFS) markDir(number, generation uint64) *fakeFS {
	id := vhdimap.FileID{Number: number, Generation: generation}
	file := f.files[id]
	file.entry.IsDir = true
	f.files[id] = file
	return f
}

func (f *fakeFS) Capabilities() vhdimap.Capabilities { return f.caps }

func (f *fakeFS) VolumeIdentity(ctx context.Context) (string, error) {
	if f.identityErr != nil {
		return "", f.identityErr
	}
	return f.identity, nil
}

func (f *fakeFS) WalkFiles(ctx context.Context, fn func(vhdimap.FileEntry) error) error {
	for _, file := range f.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(file.entry); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeFS) FileByID(ctx context.Context, id vhdimap.FileID) (vhdimap.FileEntry, error) {
	file, ok := f.files[id]
	if !ok {
		return vhdimap.FileEntry{}, fmt.Errorf("fake: no file %d.%d", id.Number, id.Generation)
	}
	return file.entry, nil
}

func (f *fakeFS) ExtentsForFile(ctx context.Context, id vhdimap.FileID) ([]vhdimap.FileExtent, error) {
	if err, ok := f.extentErr[id]; ok {
		return nil, err
	}
	file, ok := f.files[id]
	if !ok {
		return nil, fmt.Errorf("fake: no file %d.%d", id.Number, id.Generation)
	}
	return file.extents, nil
}

// journalFake is a fakeFS that also records renames, so the corroboration path
// can be exercised without an NTFS volume.
type journalFake struct {
	*fakeFS
	state   string
	renames []vhdimap.RenameEvidence
}

var (
	_ vhdimap.Filesystem = (*journalFake)(nil)
	_ vhdimap.Journal    = (*journalFake)(nil)
)

func (j *journalFake) VolumeState(ctx context.Context) (string, error) { return j.state, nil }

func (j *journalFake) RenamesBetween(ctx context.Context, from, to string) ([]vhdimap.RenameEvidence, error) {
	return j.renames, nil
}

func baseOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
