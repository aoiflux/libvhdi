# libvhdi

libvhdi is a pure Go library for reading VHD and VHDX virtual disk images.

It is designed to be easy to embed in forensic tools, analysis pipelines, and
any application that needs to inspect the contents of virtual hard disk files
without an external C dependency.

## Features

- Pure Go (no CGO dependency)
- Root import API: `github.com/aoiflux/libvhdi`
- VHD format support:
  - Fixed disks
  - Dynamic (sparse) disks
  - Differencing (child) disks
- VHDX format support:
  - Fixed disks
  - Dynamic (sparse) disks
  - Differencing (child) disks
  - Partial block reads via sector bitmap (state-7 blocks)
- Automatic format detection (`OpenFile`)
- Differencing disk parent chaining (`SetParent`)
- Virtual-to-file offset resolution (`VirtualToFileOffset`)
- `io.ReaderAt`-compatible decoded stream for random-access reads
- Hardware-accelerated CRC32 (Castagnoli via SSE4.2 on x86)
- Bulk BAT reads — single `ReadAt` call for the full allocation table

## Install

```
go get github.com/aoiflux/libvhdi
```

## Quick Start

### Open any VHD or VHDX file

```go
package main

import (
	"fmt"
	"log"

	libvhdi "github.com/aoiflux/libvhdi"
)

func main() {
	disk, err := libvhdi.OpenFile("disk.vhdx")
	if err != nil {
		log.Fatal(err)
	}
	defer disk.Close()

	fmt.Printf("format       : %v\n", disk.Format())
	fmt.Printf("virtual_size : %d bytes\n", disk.Size())
	fmt.Printf("block_size   : %d bytes\n", disk.BlockSize())
	fmt.Printf("sector_size  : %d bytes\n", disk.SectorSize())
	fmt.Printf("identifier   : %s\n", disk.GUIDString())

	buf := make([]byte, 512)
	n, err := disk.ReadAt(buf, 0)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("read %d bytes from offset 0\n", n)
}
```

### Open from an io.ReaderAt

```go
f, err := os.Open("disk.vhd")
if err != nil {
    log.Fatal(err)
}
defer f.Close()

fi, _ := f.Stat()
disk, err := libvhdi.OpenVHD(f, fi.Size())
if err != nil {
    log.Fatal(err)
}
defer disk.Close()
```

### Differencing disk parent chaining

```go
parent, err := libvhdi.OpenFile("base.vhdx")
if err != nil {
    log.Fatal(err)
}
defer parent.Close()

child, err := libvhdi.OpenFile("snapshot.vhdx")
if err != nil {
    log.Fatal(err)
}
defer child.Close()

if err := child.SetParent(parent); err != nil {
    log.Fatal(err)
}

// Reads now transparently fall back to parent for unallocated blocks.
buf := make([]byte, 512)
child.ReadAt(buf, 0)
```

### Virtual-to-file offset resolution

```go
virtualOffset := int64(0)
fileOffset, mapped, err := disk.VirtualToFileOffset(virtualOffset)
if err != nil {
    log.Fatal(err)
}
if mapped {
    fmt.Printf("virtual %d → file %d\n", virtualOffset, fileOffset)
} else {
    fmt.Println("region is sparse / parent-backed")
}
```

## API

### Open functions

| Function                                                 | Description                                            |
| -------------------------------------------------------- | ------------------------------------------------------ |
| `OpenFile(path string) (*Disk, error)`                   | Auto-detect format and open a VHD or VHDX file by path |
| `OpenVHD(r io.ReaderAt, fileSize int64) (*Disk, error)`  | Open a VHD image from an `io.ReaderAt`                 |
| `OpenVHDX(r io.ReaderAt, fileSize int64) (*Disk, error)` | Open a VHDX image from an `io.ReaderAt`                |

### Disk methods

```go
type Disk struct { /* ... */ }

func (d *Disk) ReadAt(p []byte, offset int64) (int, error)
func (d *Disk) Close() error

func (d *Disk) Size() uint64
func (d *Disk) Format() types.FileFormat
func (d *Disk) DiskType() types.DiskType
func (d *Disk) BlockSize() uint32
func (d *Disk) SectorSize() uint32
func (d *Disk) IsDifferencing() bool
func (d *Disk) GUIDString() string
func (d *Disk) Identifier() [16]byte

// Differencing disks
func (d *Disk) ParentFilename() string
func (d *Disk) ParentIdentifier() [16]byte
func (d *Disk) SetParent(parent *Disk) error

// Offset resolution
func (d *Disk) VirtualToFileOffset(virtualOffset int64) (fileOffset int64, mapped bool, err error)
```

`ReadAt` exposes the fully decoded logical stream at any byte offset. For
dynamic and differencing disks, unallocated regions read as zeroes (or are
satisfied from the parent chain). For VHDX partially-allocated blocks (state 7),
the sector bitmap is consulted and individual sectors are read or zeroed
accordingly.

`VirtualToFileOffset` translates a byte offset in the virtual disk's address
space to the corresponding byte offset in the `.vhd` / `.vhdx` backing file.
Returns `mapped = false` for sparse, unallocated, or parent-backed regions.

### Format and DiskType constants

```go
const (
    FormatUnknown = types.FileFormatUnknown
    FormatVHD     = types.FileFormatVHD
    FormatVHDX    = types.FileFormatVHDX
)

const (
    DiskTypeFixed        = types.DiskTypeFixed
    DiskTypeDynamic      = types.DiskTypeDynamic
    DiskTypeDifferential = types.DiskTypeDifferential
)
```

## Included Example Programs

### 1) Partition table and filesystem detection

```
go run ./examples/offsets [flags] <disk.vhd|disk.vhdx>
```

Auto-detects partition tables (MBR, GPT) using
[libtable](https://github.com/aoiflux/libtable) and probes for filesystems
(NTFS, FAT, exFAT, ext2/3/4, HFS+). When an ext2/3/4 filesystem is found,
superblock details are printed via [libext](https://github.com/aoiflux/libext).

Optional flags:

- `-pt-offset <bytes>` — byte offset where partition table parsing starts
  (default 0)
- `-verbose-detect` — print all detection attempts
- `-dump-sectors N` — hex dump first N sectors (512 bytes each) from the decoded
  stream
- `-map-first-mib N` — print a per-MiB virtual-to-file mapping table for the
  first N MiB

**Example output on a dynamic VHDX containing an ext4 root filesystem:**

```
file             : disk.vhdx
format           : VHDX
disk_type        : dynamic
virtual_size     : 1.00 TB
block_size       : 1.00 MB
sector_size      : 512 bytes
identifier       : 43efe041-6fc7-42eb-89a1-085575a361c3

Filesystem detected at virtual offset 0: ext2/3/4 (no partition table)
Filesystem backing file offset: 12582912

--- filesystem info (libext) ---
  kind             : ext4
  uuid             : 08c19b1f-c199-4750-bffc-7e6ff300e6db
  block_size       : 4096 bytes
  inode_size       : 256 bytes
  total_blocks     : 268435456
  free_blocks      : 255846154
  total_inodes     : 67108864
  free_inodes      : 66600919
  groups           : 8192
  last_mount_time  : 2026-05-01 22:36:06 UTC
  last_write_time  : 2026-05-01 22:41:15 UTC
  last_mounted_at  : /distro
  mount_count      : 40 / 65535
  features         : ...
---
```

## Package Structure

| Package                      | Description                                                   |
| ---------------------------- | ------------------------------------------------------------- |
| `github.com/aoiflux/libvhdi` | Root API — `OpenFile`, `Disk`, format/type constants          |
| `reader`                     | `VirtualDisk` implementation, open functions, parent chaining |
| `bat`                        | VHD and VHDX Block Allocation Table parsers                   |
| `block`                      | Virtual-to-physical block readers for VHD and VHDX            |
| `diff`                       | Differencing disk resolver (parent chain read-through)        |
| `metadata`                   | VHDX metadata table and region table parsing                  |
| `types`                      | Data structure definitions, constants, GUIDs                  |
| `internal/binaryutil`        | Endian-aware parsing utilities, CRC32                         |

## Development

Run all tests:

```
go test ./...
```
