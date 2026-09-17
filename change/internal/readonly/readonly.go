// SPDX-License-Identifier: MIT

// Package readonly wraps a reader so that nothing can write through it.
//
// It exists because a filesystem library may opportunistically promote its
// argument: libntfs's Open does exactly that, taking a writer if the reader it
// was handed happens to implement io.WriterAt. Every library here is asked for
// a read-only mode explicitly, so the promotion should never fire -- but a
// forensic pipeline should not depend on every library keeping that promise
// forever, and the cost of removing the possibility entirely is this file.
package readonly

import "io"

// at implements io.ReaderAt and nothing else.
//
// It is deliberately a struct with an unexported field rather than an embedded
// interface. Embedding would forward whatever else the underlying value
// implements, which is the exact promotion this is here to prevent, and it
// would do so invisibly.
type at struct {
	r io.ReaderAt
}

func (a at) ReadAt(p []byte, off int64) (int, error) { return a.r.ReadAt(p, off) }

// Wrap returns a reader that can only be read from.
//
// A type assertion to io.WriterAt, io.Seeker, *os.File or anything else fails
// on the result, whatever r turns out to be. Wrapping an already-wrapped reader
// returns it unchanged. A nil reader is returned as nil, so a caller's own nil
// check still works.
func Wrap(r io.ReaderAt) io.ReaderAt {
	if r == nil {
		return nil
	}
	if _, ok := r.(at); ok {
		return r
	}
	return at{r: r}
}
