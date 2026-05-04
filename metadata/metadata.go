// Package metadata provides metadata extraction and decoding for VHDX files.
package metadata

import "github.com/aoiflux/libvhdi/types"

// MetadataReader provides interface for reading and decoding VHDX metadata.
type MetadataReader interface {
	// ReadMetadata extracts all metadata from the virtual disk.
	ReadMetadata() (*types.MetadataValues, error)

	// GetValue retrieves a specific metadata value by its identifier.
	GetValue(identifier [16]byte) (interface{}, error)

	// WriteMetadata writes metadata values back (reserved for future use).
	WriteMetadata(values *types.MetadataValues) error
}

// ItemReader provides interface for reading individual metadata items.
type ItemReader interface {
	// Read reads a metadata item by its GUID identifier.
	Read(identifier [16]byte) ([]byte, error)

	// Count returns the total number of metadata items.
	Count() uint32
}
