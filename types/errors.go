// SPDX-License-Identifier: MIT

package types

import (
	"errors"
	"fmt"
	"strconv"
)

// The sentinels live here, in the package every other one imports, so that a
// parser deep in the metadata or block layer can wrap them without depending on
// the reader that calls it.

var (
	// ErrCorruptImage is returned when an image's structures are internally
	// inconsistent, fail their checksum, or describe something that does not fit
	// the file.
	//
	// It says the image is damaged or was never valid. It does not say the
	// library is missing a feature, which is what ErrUnsupportedFeature is for --
	// and the difference matters to whoever is holding the evidence. One means
	// the file is broken; the other means to try a different tool.
	ErrCorruptImage = errors.New("libvhdi: corrupt or malformed image")

	// ErrUnsupportedFeature is returned when an image is well-formed but uses
	// something this library does not implement.
	//
	// Refusing such an image is deliberate. The alternative -- decoding the parts
	// that are understood and ignoring the rest -- produces a device that is
	// wrong in a way nothing downstream can detect.
	ErrUnsupportedFeature = errors.New("libvhdi: unsupported feature")
)

// NoOffset marks a StructuralError that is not about a particular byte.
const NoOffset int64 = -1

// StructuralError says what was being parsed, where, and what was wrong with it.
//
// A bare "invalid VHD footer signature" tells an examiner that something failed
// and nothing else. Which of the two footer copies? At what offset? Carrying
// the location turns an error into a starting point for a manual examination of
// the image, which is the only recourse once the library has given up.
//
// It wraps one of ErrCorruptImage or ErrUnsupportedFeature, so errors.Is
// continues to answer the coarse question.
type StructuralError struct {
	// Op is what was being attempted, in the imperative: "read VHD footer",
	// "parse VHDX metadata table".
	Op string

	// Format is the image format being parsed, or FileFormatUnknown before the
	// format has been established.
	Format FileFormat

	// Offset is the byte offset in the image the failure concerns. It is
	// NoOffset when the failure is not about a particular byte, such as two
	// fields disagreeing with each other.
	Offset int64

	// Field names the structure field at fault, spelled as the specification
	// spells it, so it can be looked up.
	Field string

	// Err is the underlying cause, wrapping ErrCorruptImage or
	// ErrUnsupportedFeature.
	Err error
}

// Error implements error.
func (e *StructuralError) Error() string {
	var b []byte
	b = append(b, e.Op...)

	if e.Format != FileFormatUnknown {
		b = append(b, " ("...)
		b = append(b, e.Format.String()...)
		b = append(b, ')')
	}
	if e.Field != "" {
		b = append(b, " field "...)
		b = append(b, e.Field...)
	}
	if e.Offset >= 0 {
		b = append(b, " at offset "...)
		b = strconv.AppendInt(b, e.Offset, 10)
	}
	if e.Err != nil {
		b = append(b, ": "...)
		b = append(b, e.Err.Error()...)
	}
	return string(b)
}

// Unwrap returns the underlying cause, so errors.Is reaches the sentinel.
func (e *StructuralError) Unwrap() error { return e.Err }

// Corruptf reports a malformed image, recording where the problem is.
//
// Pass NoOffset for offset when the failure is not about a particular byte, and
// "" for field when no single field is at fault.
func Corruptf(op string, format FileFormat, offset int64, field, msg string, args ...any) error {
	return &StructuralError{
		Op:     op,
		Format: format,
		Offset: offset,
		Field:  field,
		Err:    fmt.Errorf("%w: %s", ErrCorruptImage, fmt.Sprintf(msg, args...)),
	}
}

// Unsupportedf reports a well-formed image this library cannot read.
func Unsupportedf(op string, format FileFormat, offset int64, field, msg string, args ...any) error {
	return &StructuralError{
		Op:     op,
		Format: format,
		Offset: offset,
		Field:  field,
		Err:    fmt.Errorf("%w: %s", ErrUnsupportedFeature, fmt.Sprintf(msg, args...)),
	}
}
