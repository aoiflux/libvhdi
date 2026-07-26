# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.6.0] - 2026-07-26

Closes the outstanding items from the original plan except release tagging, and
validates the library against images produced by an independent implementation
for the first time.

### Added

- **4096-byte logical sector coverage.** Every VHDX fixture used 512-byte
  sectors, leaving the 4K path untested. The sector size feeds
  `chunkRatio = 2^23 × sectorSize / blockSize` and `sectorsPerBlock`, so moving to
  4096 relocates the chunk-0 sector bitmap entry from BAT index 4096 to 32768 and
  changes every bit position within the bitmap — genuinely separate arithmetic
  from the 512-byte path.

  `reader/vhdx4k_test.go` covers geometry, per-sector differencing fallthrough,
  the last sector of a block (index 255, the likeliest off-by-one), fail-closed
  behaviour, and extent mapping. The fixture's BAT region grew to 512 KB to hold
  the 256 KB table a 32 GB 4K-sector disk requires. No defects found.

- **Concurrency tests.** The documented guarantee that `ReadAt` is safe for
  concurrent use rested on inspection alone, and `go test -race` proved nothing
  because no test read in parallel. `reader/concurrent_test.go` now runs many
  goroutines against one `Disk` — across the plain block reader, a two- and
  three-deep differencing chain, VHDX partial blocks, the log replay overlay, and
  `Extents` concurrently with `ReadAt` — requiring every result to match a
  single-threaded reference.

  Verified to have teeth: injecting one unsynchronised counter into the resolver's
  read path makes the race detector fail the suite immediately.

- **`Chain()` and `ChainComplete()`**, returning a `ChainEntry` per link with its
  path, identifier, recorded parent identifier, format, disk type, virtual size
  and log state. This is the set of files that together constitute the device,
  which is what an evidence record has to name. `ChainComplete` distinguishes a
  fully resolved chain from one whose parent was never attached.

- **`NOTICE`** documenting the origin of the implementation, the specifications it
  was written against, the checksum distinction between the two formats, and the
  external oracles used for cross-validation. This is the substance behind the
  attribution fix in 0.2.0, which only corrected the `Author` string.

- SPDX identifiers on all Go files (previously 6 of 21 non-test files), and a
  `.gitattributes` (added in 0.5.0) so `gofmt -l` stops reporting every
  checked-out file on Windows.

### Fixed

- **VHDX region pointers are validated before they are dereferenced.** The
  metadata region offset was never checked against the file size, so a bad pointer
  surfaced as a bare `EOF` from inside the metadata parser rather than as a
  statement that the image is malformed. Both the BAT and metadata region
  pointers are now checked up front, before either is read, and report
  `ErrCorruptImage`.

### Removed

- **`diff.DifferencingDiskReader`.** A dead exported interface that nothing
  implemented and nothing could implement: its method set
  (`ReadSector`, `Parent`, `SetParent` over itself) does not correspond to
  anything in the library, and it could not be wired in without being redesigned.
  Keeping it only advertised an abstraction that does not exist. Removing an
  exported type is a breaking change; under semantic versioning for 0.x this is
  permitted in a minor release, and nothing in this module referenced it.

### Validated against real images

The library had never read an image it did not produce itself. It now has.

`scripts/gen-corpus-qemu.ps1` uses **qemu-img as an independent producer** and
needs no elevation, which is what made this possible: qemu can *create* VHD and
VHDX, so a known raw pattern is converted into an image and the pattern becomes
the ground truth. If this library decodes the image back to the original bytes,
two independently written implementations agree.

All 14 generated images decode byte for byte, sequentially and by random access
across block and sector boundaries:

| Coverage | Cases |
| --- | --- |
| Subformats | VHD fixed, VHD dynamic, VHDX fixed, VHDX dynamic |
| VHDX block sizes | 1 MB, 2 MB, 8 MB (qemu default), 32 MB |
| CHS-rounded VHD | 10 MB pattern in a 10514432-byte disk, tail decoding as zeroes |
| Block-unaligned | 5 MB + 512 virtual size, final block past the end of the device |
| All-sparse | 32 MB device stored in a 2560-byte VHD |
| Large | 512 MB device, 512 blocks |

**Interop finding.** On the one image where the two disagree, this library is the
spec-conformant one. For a fixed VHD, qemu-img 11.0.0's `vpc` driver reports the
virtual size as the *file* length, which includes the trailing 512-byte footer;
converting such an image to raw therefore yields 512 extra bytes ending in the
`conectix` footer signature, presented as disk contents. This library takes the
virtual size from the footer's own size field and clamps reads to it, so the
footer never enters the data stream — which is exactly the defect fixed as P0-3
in 0.2.0 and guarded by `TestFixedVHD_ReadAtDoesNotLeakFooter`.

Differencing chains remain synthetic-only: qemu-img cannot create them for either
format ("Backing file not supported"), so `scripts/gen-corpus.ps1` and Hyper-V —
which need an elevated session — are still the only route to a
producer-generated chain. Chain correctness rests on the spec-derived fixtures.

### Notes

- `Options.AllowMissingParent` was planned in 0.2.0 to let callers opt back into
  the pre-0.2.0 behaviour of returning zeroes for an unresolved parent. It is
  deliberately **not** implemented: it would re-enable the silent-corruption mode
  that release fixed. `ExtentUnresolved` gives a caller the same information
  without the risk of mistaking padding for data.

## [0.5.0] - 2026-07-26

### Added

- **Extent mapping.** `Extents`, `AllExtents` and `MappedBytes` describe which
  ranges of the virtual disk are backed by real data and which are sparse, so an
  acquisition or carving tool can read only what exists instead of reading and
  writing megabytes of zeroes. On the sparse VHDX fixture — a 4 GB device holding
  one allocated megabyte — the map reduces to two extents.

  Each mapped extent carries the backing file offset, the disk's position in the
  differencing chain, and its path. That is provenance `VirtualToFileOffset`
  cannot express: it returns `mapped = false` for every differencing disk, since
  a single call cannot say which of several files supplies a byte. Extents resolve
  per sector, so a partially-written block reports alternating child and parent
  runs with the correct `ChainIndex` at each step.

  A range resolving to a missing parent is reported as `ExtentUnresolved` rather
  than failing the whole request, so a caller can see exactly which parts of a
  chain it cannot account for.

  Adjacent runs merge only when they genuinely continue — for mapped runs, that
  means the same file *and* contiguous file offsets, or the merged extent's
  `FileOffset` would describe bytes that are not there.

- `diff.BlockKind` and `diff.SectorRuns` expose block and sector classification
  so the extent map reuses the resolver's logic rather than reimplementing it.

- `.gitattributes` normalising the repository to LF. Without it `gofmt -l`
  reports every checked-out file on Windows as unformatted, which buries real
  findings. Applying it re-normalises existing files on the next checkout, so
  expect one whitespace-only commit; `git add --renormalize .` does it in one go.

### Fixed

- **The differencing resolver no longer reads one bitmap byte per sector.**
  Resolving a partially-written block issued a separate one-byte read for every
  sector it touched, turning a single 2 MB block read into thousands of I/O
  operations. The bitmap bytes covering a sector range are now read once, and
  contiguous runs sharing a backing are served by one read each.

  This was noted as a known gap in 0.3.0. The fix keeps the resolver free of
  shared mutable state, so `ReadAt` remains safe for concurrent use without
  locking — a cache would have required a mutex and serialised readers.

### Changed

- `VirtualToFileOffset` is unchanged, but its documentation now points to
  `Extents` for differencing chains, where a single file offset cannot describe
  the answer.

## [0.4.0] - 2026-07-26

### Added

- **VHDX log replay.** A VHDX file whose active header carries a log GUID holds
  journalled writes that may not have reached their final locations, so its block
  allocation table and metadata can describe an earlier state than the last
  committed one. v0.3.0 detected this and refused the image; the log is now
  parsed and replayed, and the disk presents the committed state.

  Replay is **read-only**: entries are applied to an in-memory overlay layered
  over the backing reader, so a forensic copy stays byte-identical. The region
  table, metadata and BAT are all parsed through that overlay, and payload reads
  go through it too, so a replayed sector is visible in the data stream and not
  only in the metadata.

  Implemented in `internal/vhdxlog`, covering the circular log region including
  entries that wrap its end, `desc` and `zero` descriptors, data-sector
  reconstruction from the descriptor's displaced leading and trailing bytes, and
  the split sequence number that detects a torn write within a sector. Entries
  are accepted only with a matching log GUID, a valid CRC-32C and a non-zero
  sequence number, and are applied in ascending sequence order so later writes
  win.

  A log whose entries do not form a complete, consecutively numbered sequence is
  **refused** rather than partially applied, since replaying part of a
  transaction could produce a state the writer never committed.

- `HasLog`, `LogReplayed` and `LogReplayStats` alongside the existing `IsDirty`.
  `IsDirty` now means "carries a log that was *not* replayed". The replayed
  sector count is an evidentiary signal in its own right: it measures how much of
  the image was still in flight when the file was captured.

- **Real-image corpus harness.** `scripts/gen-corpus.ps1` generates authentic
  images with Hyper-V's `New-VHD` — fixed, dynamic and three-deep differencing
  chains, in both formats, plus a 4096-byte-logical-sector VHDX and an all-sparse
  disk — and raw ground-truth dumps with `qemu-img`, an independent
  implementation. Setting `LIBVHDI_CORPUS` to the output directory enables
  `TestCorpusMatchesRawDumps` and `TestCorpusRandomAccessMatchesSequential`,
  which compare byte for byte and probe block and sector boundaries.

  The generator needs an elevated session and qemu-img, so it cannot run
  everywhere. `TestCorpusHarnessSelfCheck` therefore exercises the scanning,
  pairing and comparison logic against synthetic images with known contents, and
  `TestCorpusHarnessDetectsMismatch` verifies the comparison fails on a
  single flipped byte, so a green corpus run means something.

- `FuzzAnalyze` over the log parser.

### Notes

- The Hyper-V corpus generator was **not** executed when this release was
  written: `New-VHD` requires elevation, which was unavailable. A qemu-based
  generator needing no elevation was added in 0.6.0 and has since validated the
  non-differencing paths; see that entry.

## [0.3.0] - 2026-07-26

Adds the integration surface needed to use this library as a read-only image
layer, and hardens the parsers against malformed input.

### Added

- **`Open(io.ReaderAt, *Options)`** derives the image size from the reader,
  handling anything that exposes `Size`, `Stat` or `Seek` — `*os.File`,
  `*bytes.Reader`, `*strings.Reader`, `io.SectionReader`, `fs.File`. A bare
  `io.ReaderAt` with none of those must set `Options.Size` for VHD; VHDX locates
  every structure from fixed offsets and needs no size at all.

- **Automatic differencing parent chain resolution.** `ParentResolver` locates a
  child's parent; `DirParentResolver` searches the child's own directory and any
  additional directories, and `FSParentResolver` searches an `fs.FS`.
  `OpenFileWith` defaults to searching the image's own directory, so a caller
  opening only the child gets a correct contiguous device with no manual
  `SetParent` wiring.
  - Windows-style recorded paths are normalised, and a stale absolute path falls
    back to the basename, so a chain copied off its original machine resolves.
  - A resolved parent is verified against the identifier and virtual size
    recorded in the child, so an unrelated image of the right size is rejected
    rather than read as the parent. `Options.AllowParentGUIDMismatch` overrides.
  - Chains are bounded by `Options.MaxChainDepth` (default 32) and checked for
    cycles.
  - Resolution is best-effort by default: an unresolvable parent still opens,
    reports why via `ParentResolveError`, and fails closed on reads that need it.
    `Options.RequireParentChain` makes it a hard error at open.

- **`Options`** with a safe zero value; every field that relaxes a check is named
  so that `false` is the strict setting.

- **VHD parent locator platform data is now read.** v0.2.0 parsed the 24-byte
  locator descriptors but never followed `PlatformDataOffset`, so the paths they
  carry were unavailable. `W2ru` and `W2ku` entries are now decoded from UTF-16LE
  and exposed via `ParentLocators`, giving the resolver fallback candidates.
  VHDX parent locators likewise retain every recorded path rather than only the
  first.

- **`ErrDirtyImage` and `IsDirty`.** A VHDX image whose active header carries a
  non-zero log GUID has an unreplayed log, so its BAT and metadata may be stale.
  Such an image is now refused rather than silently read as though clean. Log
  replay is still not implemented, so `Options.AllowDirtyImage` trades the hard
  failure for a documented risk.

- `Close` now releases parents opened by automatic resolution. Parents attached
  by hand with `SetParent` remain the caller's to close.

- `ChainDepth`, `Parent`, `Path`, `ParentLocators` and `ParentResolveError` for
  inspecting chain state.

- Fuzz targets `FuzzOpen`, `FuzzVHDFooter` and `FuzzVHDXStructures`, plus
  truncation and garbage-input tests.

### Fixed

- **Unbounded allocations driven by image fields.** Several counts and sizes were
  trusted directly from headers:
  - A VHD `NumberOfBlocks` of `0xFFFFFFFF` sized the BAT buffer at 16 GB, and the
    multiplication overflowed `int` on 32-bit platforms.
  - A VHDX virtual disk size of 2^62 sized the BAT at terabytes, and the block
    count silently truncated through a `uint32` conversion.
  - A VHDX parent locator declaring 65535 entries with maximal key and value
    lengths drove roughly 8.5 GB of reads and allocations from a few bytes of
    input. The item's declared size was passed to the parser but never used.
    Found by `FuzzVHDXStructures`, which stalled on it.
  - A VHDX metadata table entry count was not bounded by its region.
  - The VHDX region table entry count was unbounded, and the per-entry bounds
    check used `uint32` offset arithmetic that wrapped for large counts. It is
    now capped at the specification's 2047.

  BAT and metadata structures are now cross-checked against the declared region
  size and the file size before anything is allocated.

- **Divide-by-zero panics on malformed input.** A VHDX image missing the required
  File Parameters metadata item left the block size at zero, which the block
  count calculation then divided by. The block readers likewise divided by the
  block size and the chunk ratio without guarding either. All now return errors.

- The BAT region size from the VHDX region table was parsed and discarded
  (`_ = batSize`); it is now used to bound the table.

- A fixed VHD larger than 4 GiB no longer reports a `BlockSize` truncated
  through `uint32`, and a fixed disk claiming more payload than its file holds is
  rejected.

### Known gaps

- VHDX log replay is not implemented in this release. Dirty images are detected
  and refused by default rather than read as clean, but cannot be reconciled.
  (Implemented in 0.4.0.)
- The differencing resolver reads one sector bitmap byte per sector run and does
  not cache them, so heavily fragmented differencing reads do more I/O than
  necessary. Correctness first. (Fixed in 0.5.0.)

## [0.2.0] - 2026-07-26

This release corrects read-path defects that produced **silently wrong data**.
Any output produced by v0.1.0 for VHD images or for differencing disks of either
format should be treated as unreliable and regenerated.

### Fixed

- **VHD checksums are no longer computed as CRC-32C.** The VHD specification
  defines the file footer and dynamic disk header checksum as a one's complement
  of the sum of the structure's bytes. v0.1.0 applied CRC-32C — a VHDX
  construct — to both, so `OpenVHD` rejected every specification-conformant VHD
  image with a checksum mismatch. The entire VHD code path was unreachable for
  real files. CRC-32C remains in use for VHDX headers, region tables and
  metadata, where it is correct.

- **Differencing disks now resolve at sector granularity.** Both formats record
  partially-written blocks with a sector bitmap: sectors whose bit is set live
  in the child, and sectors whose bit is clear must be served from the parent.
  v0.1.0 resolved at whole-block granularity, so a partially-written block
  returned the child's zero-filled holes in place of the parent's data.
  - VHD: an allocated block in a child disk is still sector-sparse; its block
    bitmap is now consulted.
  - VHDX: `PAYLOAD_BLOCK_PARTIALLY_PRESENT` (state 7) occurs only in
    differencing files and is now resolved against the parent instead of being
    zero-filled. `PAYLOAD_BLOCK_NOT_PRESENT` (state 0) resolves to the parent;
    `UNDEFINED`, `ZERO` and `UNMAPPED` read as zeroes, which is correct.

- **The VHD block bitmap is read most-significant-bit first**, matching the
  specification, the reference C implementation and qemu's `vpc` driver. The
  previous least-significant-bit-first reading inverted which sectors resolved
  to the parent within each group of eight.

- **Reads are clamped to the virtual disk size.** `ReadAt` previously bounded
  only the starting offset, so a request straddling the end of the device
  returned trailing block padding as if it were disk content — and for fixed
  VHDs, whose 512-byte footer directly follows the payload, it returned the
  footer itself. Such reads now return the available bytes and `io.EOF`.

- **Differencing disks fail closed.** Reading a differencing disk before a
  parent is attached previously returned zeroes with a `nil` error, which is
  indistinguishable from genuine disk contents. It now returns
  `ErrParentRequired`.

### Added

- `ErrParentRequired`, exported from both `libvhdi` and `reader`.
- `(*Disk).NeedsParent`, `(*Disk).Parent` and `(*Disk).ChainDepth` for
  inspecting differencing chain state.
- `diff.Config` and `diff.New`, which take the child's backing reader so sector
  bitmaps can be read.

### Changed

- `Author` is now `"aoiflux"`. v0.1.0 declared `"libewf contributors"`, which
  misattributed authorship of an independent implementation.
- The package doc no longer claims parity with the libvhdi C library; this
  library is written against the published format specifications and shares no
  code with it.
- `Version` is now `"0.2.0"`.

### Deprecated

- `diff.NewVHDResolver` and `diff.NewVHDXResolver`. Neither receives the child's
  backing reader, so neither can read a sector bitmap. Blocks that need
  per-sector resolution now return `diff.ErrNoBitmapSource` rather than silently
  dropping parent data. Use `diff.New` with `Config.Source` set.

### Known gaps

These are unchanged in this release and remain tracked:

- VHDX log replay is not implemented. A file whose active header carries a
  non-zero log GUID is dirty, and its BAT and metadata may be stale; such files
  are currently read as though clean.
- Header-derived sizes and offsets are not validated against the file size, so
  a crafted or truncated image can cause an oversized allocation or a
  divide-by-zero panic. The library is not yet safe against untrusted input.
- Differencing parent chains must still be wired manually with `SetParent`;
  there is no automatic parent resolver.

## [0.1.0]

- Initial release.
