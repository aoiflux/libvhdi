// Package diff provides differencing disk logic for child-parent virtual disk relationships.
package diff

// DifferencingDiskReader provides interface for reading differencing (child) disks.
type DifferencingDiskReader interface {
	// ReadSector reads a sector, resolving to parent if needed.
	ReadSector(index uint32) ([]byte, error)

	// Parent returns the parent disk reader, or nil if this is not a differencing disk.
	Parent() DifferencingDiskReader

	// SetParent sets the parent disk reader for this differencing disk.
	SetParent(parent DifferencingDiskReader) error
}
