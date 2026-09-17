// SPDX-License-Identifier: MIT

package readonly_test

import (
	"io"
	"os"
	"testing"

	"github.com/aoiflux/libvhdi/change/internal/readonly"
)

// readWriter implements both halves, which is the shape that makes a library's
// opportunistic promotion fire.
type readWriter struct {
	data    []byte
	written bool
}

func (rw *readWriter) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(rw.data)) {
		return 0, io.EOF
	}
	return copy(p, rw.data[off:]), nil
}

func (rw *readWriter) WriteAt(p []byte, off int64) (int, error) {
	rw.written = true
	return len(p), nil
}

func (rw *readWriter) Size() int64 { return int64(len(rw.data)) }

func (rw *readWriter) Stat() (os.FileInfo, error) { return nil, nil }

// TestAWriterCannotBeRecoveredByTypeAssertion is the whole point of the
// package. libntfs promotes its argument when the argument implements
// io.WriterAt; after wrapping, the assertion must fail whatever was passed in.
func TestAWriterCannotBeRecoveredByTypeAssertion(t *testing.T) {
	rw := &readWriter{data: []byte("evidence")}
	wrapped := readonly.Wrap(rw)

	if _, ok := wrapped.(io.WriterAt); ok {
		t.Fatal("the wrapped reader still asserts to io.WriterAt")
	}
	if _, ok := wrapped.(io.ReaderFrom); ok {
		t.Error("the wrapped reader asserts to io.ReaderFrom")
	}
	if _, ok := wrapped.(*readWriter); ok {
		t.Error("the concrete type is still recoverable, so anything on it is too")
	}
	if rw.written {
		t.Error("a write reached the underlying reader")
	}
}

// TestSizeAndStatAreAlsoHidden: a library that probes for these gets nothing,
// which is why the adapters pass the disk's size explicitly rather than letting
// it be discovered.
func TestSizeAndStatAreAlsoHidden(t *testing.T) {
	wrapped := readonly.Wrap(&readWriter{data: make([]byte, 64)})

	if _, ok := wrapped.(interface{ Size() int64 }); ok {
		t.Error("Size() is still visible through the wrapper")
	}
	if _, ok := wrapped.(interface {
		Stat() (os.FileInfo, error)
	}); ok {
		t.Error("Stat() is still visible through the wrapper")
	}
}

func TestReadsPassThroughUnchanged(t *testing.T) {
	want := []byte("the quick brown fox")
	wrapped := readonly.Wrap(&readWriter{data: want})

	got := make([]byte, len(want))
	n, err := wrapped.ReadAt(got, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(want) || string(got) != string(want) {
		t.Errorf("read %q (%d bytes), want %q", got[:n], n, want)
	}
}

func TestWrappingIsIdempotent(t *testing.T) {
	once := readonly.Wrap(&readWriter{data: make([]byte, 8)})
	twice := readonly.Wrap(once)

	if once != twice {
		t.Error("wrapping an already-wrapped reader added another layer")
	}
}

// TestNilStaysNil so a caller's own nil check still works after wrapping.
func TestNilStaysNil(t *testing.T) {
	if got := readonly.Wrap(nil); got != nil {
		t.Errorf("Wrap(nil) = %v, want nil", got)
	}
}
