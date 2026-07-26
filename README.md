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
- Automatic format detection (`Open`, `OpenFile`)
- Automatic differencing parent chain resolution, verified against the identity
  recorded in each child — no manual `SetParent` wiring
- Sector-granular differencing resolution: partially-written blocks are resolved
  against the parent chain per sector, not per block
- Size derived from the reader; no explicit length needed for `*os.File`,
  `*bytes.Reader`, `io.SectionReader` or `fs.File`
- Sparse-aware extent mapping (`Extents`), reporting holes and per-sector
  provenance through a differencing chain
- Virtual-to-file offset resolution (`VirtualToFileOffset`)
- `io.ReaderAt`-compatible decoded stream for random-access reads
- VHDX log replay into a read-only in-memory overlay, so a captured image reads
  as its last committed state without being modified
- Fails closed on missing parents, unreplayable logs and malformed headers rather
  than substituting zeroes
- Hardware-accelerated CRC-32C (Castagnoli via SSE4.2 on x86) for VHDX
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

`Open` detects the format and derives the size from the reader, so no explicit
length is needed for anything exposing `Size`, `Stat` or `Seek` — which covers
`*os.File`, `*bytes.Reader`, `io.SectionReader` and `fs.File`:

```go
f, err := os.Open("disk.vhdx")
if err != nil {
    log.Fatal(err)
}
defer f.Close()

disk, err := libvhdi.Open(f, nil)
if err != nil {
    log.Fatal(err)
}
defer disk.Close()
```

A bare `io.ReaderAt` exposing none of those must supply the size explicitly for
VHD images. VHDX locates every structure from fixed offsets and needs no size:

```go
disk, err := libvhdi.Open(r, &libvhdi.Options{Size: totalBytes})
```

### Differencing chains resolve themselves

`OpenFileWith` searches the image's own directory for the parents a differencing
chain names, verifying each against the identifier and virtual size recorded in
its child. Opening only the newest disk yields a correct contiguous device:

```go
disk, err := libvhdi.OpenFileWith("snapshot-3.vhdx", nil)
if err != nil {
    log.Fatal(err)
}
defer disk.Close() // closes the parents it opened, too

if disk.NeedsParent() {
    // Best-effort by default: the chain is incomplete, and reads that need the
    // missing parent return ErrParentRequired rather than zeroes.
    log.Printf("incomplete chain: %v", disk.ParentResolveError())
}
fmt.Printf("chain depth: %d\n", disk.ChainDepth())
```

Set `Options.RequireParentChain` to make an unresolvable parent a hard error at
open, or supply a custom `Options.ParentResolver` to search elsewhere:

```go
disk, err := libvhdi.OpenFileWith("snapshot-3.vhdx", &libvhdi.Options{
    ParentResolver:     libvhdi.DirParentResolver("/evidence/base-images"),
    RequireParentChain: true,
})
```

`FSParentResolver` resolves against an `fs.FS`, and `ParentResolverFunc` adapts
any function for chains held in a database, object store or acquisition format.

### Manual parent wiring

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

### Sparse-aware acquisition with extents

`Extents` reports which ranges of the disk are backed by real data and which are
holes, so an imaging tool reads only what exists instead of reading and writing
megabytes of zeroes:

```go
extents, err := disk.AllExtents()
if err != nil {
    log.Fatal(err)
}
for _, e := range extents {
    switch e.Kind {
    case libvhdi.ExtentZero:
        out.Seek(e.Length, io.SeekCurrent) // hole: skip it
    case libvhdi.ExtentMapped:
        buf := make([]byte, e.Length)
        disk.ReadAt(buf, e.VirtualOffset)
        out.Write(buf)
    case libvhdi.ExtentUnresolved:
        log.Printf("cannot account for [%d, %d): %s", e.VirtualOffset, e.End(), e.Path)
    }
}

total, _ := disk.MappedBytes() // how much actually has to be read
```

For a differencing chain each mapped extent also says which disk supplies it,
resolved per sector, so a partially-written block reports alternating child and
parent runs:

```go
for _, e := range extents {
    if e.Kind == libvhdi.ExtentMapped {
        fmt.Printf("virtual %d..%d <- %s +%d (chain depth %d)
",
            e.VirtualOffset, e.End(), e.Path, e.FileOffset, e.ChainIndex)
    }
}
```

`ChainIndex` is 0 for the disk you opened, 1 for its parent, and so on. This is
provenance `VirtualToFileOffset` cannot express, since a single file offset cannot
describe bytes coming from several files.

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

| Function                                                    | Description                                                              |
| ----------------------------------------------------------- | ------------------------------------------------------------------------ |
| `Open(r io.ReaderAt, opts *Options) (*Disk, error)`         | Auto-detect format, derive size from the reader, resolve parent chains   |
| `OpenFileWith(path string, opts *Options) (*Disk, error)`   | As above, by path; defaults to resolving parents from the image's dir    |
| `OpenFile(path string) (*Disk, error)`                      | Auto-detect format and open a VHD or VHDX file by path                   |
| `OpenVHD(r io.ReaderAt, fileSize int64) (*Disk, error)`     | Open a VHD image from an `io.ReaderAt`                                   |
| `OpenVHDX(r io.ReaderAt, fileSize int64) (*Disk, error)`    | Open a VHDX image from an `io.ReaderAt`                                  |

### Options

The zero value is safe. Every field that relaxes a check is named so that
`false` is the strict setting.

```go
type Options struct {
    ParentResolver          ParentResolver // locate parents; nil disables auto-resolution
    Size                    int64          // explicit image size; 0 derives from the reader
    MaxChainDepth           int            // 0 means DefaultMaxChainDepth (32)
    AllowParentGUIDMismatch bool           // skip parent identity verification
    RequireParentChain      bool           // fail at open if the chain is incomplete
    AllowDirtyImage         bool           // open a VHDX with an unreplayed log
}

type ParentResolver interface {
    ResolveParent(req ParentRequest) (ParentSource, error)
}

func DirParentResolver(dirs ...string) ParentResolver
func FSParentResolver(fsys fs.FS) ParentResolver
```

### Errors

```go
ErrParentRequired  // read resolved to a parent that is not attached
ErrParentNotFound  // resolver exhausted its search
ErrParentMismatch  // located image is not the parent the child records
ErrChainTooDeep    // chain exceeds MaxChainDepth
ErrChainCycle      // chain refers back to a disk already in it
ErrSizeUnknown     // size neither derivable nor supplied
ErrDirtyImage      // VHDX log could not be replayed; image may be stale
ErrCorruptImage    // headers are inconsistent or describe structures the file cannot hold
```

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
func (d *Disk) HasLog() bool
func (d *Disk) LogReplayed() bool
func (d *Disk) IsDirty() bool
func (d *Disk) LogReplayStats() (LogReplayStats, bool)
func (d *Disk) GUIDString() string
func (d *Disk) Identifier() [16]byte
func (d *Disk) Path() string

// Differencing disks
func (d *Disk) ParentFilename() string
func (d *Disk) ParentIdentifier() [16]byte
func (d *Disk) ParentLocators() []types.ParentLocatorEntry
func (d *Disk) SetParent(parent *Disk) error
func (d *Disk) NeedsParent() bool
func (d *Disk) Parent() *Disk
func (d *Disk) ChainDepth() int
func (d *Disk) ParentResolveError() error

// Offset resolution
func (d *Disk) VirtualToFileOffset(virtualOffset int64) (fileOffset int64, mapped bool, err error)

// Extent mapping
func (d *Disk) Extents(virtualOffset, length int64) ([]Extent, error)
func (d *Disk) AllExtents() ([]Extent, error)
func (d *Disk) MappedBytes() (int64, error)
```

```go
type Extent struct {
    VirtualOffset int64      // start in the virtual disk address space
    Length        int64
    Kind          ExtentKind // ExtentZero, ExtentMapped or ExtentUnresolved
    FileOffset    int64      // offset within the backing file, if mapped
    ChainIndex    int        // 0 = this disk, 1 = its parent, ...
    Path          string     // backing file, for provenance
}

func (e Extent) End() int64
```

`ReadAt` exposes the fully decoded logical stream at any byte offset. Reads are
clamped to the virtual disk size: a request straddling the end of the device
returns the available bytes and `io.EOF`.

For dynamic disks, unallocated regions read as zeroes. For differencing disks,
resolution happens at **sector** granularity in both formats: a partially-written
block carries a sector bitmap, and sectors whose bit is clear are served from the
parent chain rather than zero-filled. VHDX `PAYLOAD_BLOCK_NOT_PRESENT` (state 0)
resolves wholly to the parent, while `UNDEFINED`, `ZERO` and `UNMAPPED` read as
zeroes.

Differencing disks fail closed. Reading one before attaching a parent with
`SetParent` returns `ErrParentRequired` — zeroes would be indistinguishable from
genuine disk contents. Use `NeedsParent` to check.

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
| `internal/vhdxlog`           | VHDX log parsing and read-only replay overlay                 |
| `internal/binaryutil`        | Endian-aware parsing utilities, checksums                     |

## Robustness

The library is read-only and never writes to an image.

Structures read from an image are cross-checked before they drive any allocation
or read: block counts, table offsets and region sizes must be internally
consistent and must fit within the file. Malformed input produces an error
wrapping `ErrCorruptImage` rather than a panic or an oversized allocation.

Checksums are verified per format, and the two formats do not share an
algorithm: VHD footers and dynamic disk headers use the specification's
one's-complement byte sum, while VHDX headers, region tables and metadata use
CRC-32C.

A VHDX image whose active header carries a log GUID holds journalled writes that
may not have reached their final locations, so its block allocation table and
metadata can describe an earlier state than the last committed one. The log is
**replayed into an in-memory overlay** before anything else is parsed, so the disk
presents the committed state while the file on disk stays byte-identical — a
forensic copy is never modified.

A log whose entries do not form a complete, consecutively numbered sequence is
refused with `ErrDirtyImage` rather than partially applied, since replaying part
of a transaction could produce a state the writer never committed.
`Options.AllowDirtyImage` reads such an image as-is instead.

```go
if disk.HasLog() {
    if stats, ok := disk.LogReplayStats(); ok {
        // How much of the image was still in flight when it was captured.
        log.Printf("replayed %d entries over %d sectors", stats.Entries, stats.Sectors)
    }
}
```

| Method          | Meaning                                                  |
| --------------- | -------------------------------------------------------- |
| `HasLog()`      | the image carries journalled writes                      |
| `LogReplayed()` | the log was replayed; reads show the committed state      |
| `IsDirty()`     | carries a log that was **not** replayed; may be stale     |

`ReadAt` on a `Disk` is safe for concurrent use when the underlying
`io.ReaderAt` is, which holds for `*os.File` and `*bytes.Reader`.

## Development

Run all tests:

```
go test ./...
go test -race ./...
```

Fuzz the parsers. `FuzzVHDXStructures` splices fuzzer bytes into the header,
region table, metadata and BAT of a valid image and repairs the checksums, so the
budget goes on field values instead of payload bytes no parser reads:

```
go test ./reader/          -run '^$' -fuzz FuzzOpen           -fuzztime 60s
go test ./reader/          -run '^$' -fuzz FuzzVHDFooter      -fuzztime 60s
go test ./reader/          -run '^$' -fuzz FuzzVHDXStructures -fuzztime 60s
go test ./internal/vhdxlog -run '^$' -fuzz FuzzAnalyze        -fuzztime 60s
```

Test images are generated in-process by the builders in
`reader/vhdfixture_test.go` and `reader/vhdxfixture_test.go`, so the repository
carries no binary fixtures. They are written to the published specifications
rather than to whatever the current parser accepts, which is what surfaced the
VHD checksum defect fixed in v0.2.0.

### Validating against real images

Synthetic fixtures cannot prove agreement with what real producers emit. To
cross-validate, generate authentic images with Hyper-V and raw ground-truth dumps
with `qemu-img`, then point the corpus tests at them:

```
# Requires an elevated session (New-VHD) and qemu-img on PATH.
pwsh -File scripts/gen-corpus.ps1 -OutputDir C:\corpus

$env:LIBVHDI_CORPUS = "C:\corpus"
go test ./reader/ -run TestCorpus -v
```

The corpus tests compare the decoded stream byte for byte against the raw dump
and probe block and sector boundaries with random-access reads. They skip when
`LIBVHDI_CORPUS` is unset. `TestCorpusHarnessSelfCheck` keeps the harness itself
covered in environments that lack the tooling.
