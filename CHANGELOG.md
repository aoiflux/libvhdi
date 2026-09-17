# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html) with the
usual pre-1.0 caveat: **any 0.x release may break API**, and the Breaking
sections below — not the version number — are the compatibility promise.

## A note on version history

Only **v0.1.0** and **v0.2.0** were ever published as git tags. An earlier
changelog described releases numbered 0.3.0 through 0.6.0 and the library
carried a `Version = "0.6.0"` constant, but no such tags exist, so no consumer
could ever have resolved them — Go modules resolve by tag. That changelog was
removed rather than corrected, which left the library with no history at all.

The work those entries described is real and is in the code. It was simply never
released. It is therefore folded into **v0.3.0** below, which is the first tag
after v0.2.0.

To keep this from recurring, the `Version` constant is gone. `Version()` now
reads the module version out of the build information the Go toolchain embeds at
link time, so it reports what was actually compiled in and cannot be wrong.

## [Unreleased]

## [0.3.0] - 2026-09-17

Everything since v0.2.0. Breaking, but narrowly: the removals are API that could
never be satisfied, plus one silent foot-gun that is better as a loud one.

### Fixed

- **VHD absolute-path parent locators were undecodable.** The `W2ku` platform
  code was defined as `0x5732316B`, whose bytes spell `"W21k"`. The spec value is
  `0x57326B75`. Every real absolute-path locator therefore fell to the `default`
  branch of `readLocatorPath`, kept an empty `Value`, and was dropped from the
  parent candidate list. Only relative (`W2ru`) locators and the legacy
  512-byte `ParentFilename` field ever drove VHD parent resolution. No test
  caught it: the only `W2ku` coverage constructed a `ParentLocatorEntry` literal
  and never went through the parser.

- **VHDX parents were accepted on virtual-size match alone.** This is the most
  serious defect in the release, because it could silently produce a *wrong
  reconstruction of a disk* rather than an error.

  `MetadataValues.ParentIdentifier` was declared but never assigned. Its only
  source is the VHDX `parent_linkage` locator key, which the metadata parser
  discarded — it kept `relative_path`, `absolute_win32_path` and `volume_path`
  and nothing else. So `ParentIdentifier()` always returned the zero GUID for
  VHDX, and `verifyParent` short-circuited before it ever compared anything.
  Any same-sized VHDX in the search directory would be attached as the parent.

  `parent_linkage` is now parsed and the identity is verified. This makes the
  documented guarantee true; it was false before.

  The optional `parent_linkage2` key is deliberately left unparsed. It would
  only ever widen what counts as an acceptable parent, and widening the check
  this release exists to introduce -- on a reading of the specification this
  implementation cannot test against a producer that emits the key -- is not a
  trade worth making. It is recorded as a known deviation instead.

- **Unknown required region entries were ignored in the primary region table.**
  MS-VHDX requires an implementation to fail on a region it does not understand
  that is marked required. `locateRegions` implemented the rule correctly, but
  its error was discarded at the call site. A primary table carrying such a
  region *plus* valid BAT and Metadata entries yielded non-zero offsets, so the
  secondary table was never consulted and the mandated failure never surfaced.
  The rule was only ever enforced on the fallback path. This is precisely the
  forward-compatibility guard the specification exists to provide.

- **`Extents` over-reported mapped bytes for VHDX partially allocated blocks.**
  `blockFileOffset` treated any allocated BAT entry as fully mapped, and block
  state 7 sets `IsAllocated`. Unlike `VirtualToFileOffset`, it never consulted
  the sector bitmap. On a non-differencing VHDX with state-7 blocks,
  `Extents`, `AllExtents` and `MappedBytes` claimed a file offset for sectors
  that `ReadAt` correctly served as zeroes — so a sparse-aware imaging tool
  driven by the extent map would have copied uninitialised bytes as though they
  were data.

- **VHD timestamps were decoded against the wrong epoch.** Both the footer's
  creation time and the dynamic disk header's parent modification time were read
  as Unix timestamps. VHD counts seconds from 1 January 2000, so every date the
  library reported was thirty years early: an image created in 2024 presented as
  1994. A raw zero now decodes as the zero `time.Time` rather than as midnight on
  1 January 2000, because a zero field means the producer recorded no timestamp
  and rendering it as an instant invents a fact.

- **A damaged VHD tail made an otherwise intact image unopenable.** Dynamic and
  differencing disks mirror the footer at offset 0 for exactly this case, and the
  library never read it. It also rejected the 511-byte footer Microsoft Virtual
  PC wrote before the format was documented. Both are now recovery paths, tried
  in order after the conformant footer fails, and when all three fail the error
  names each location. A footer is never accepted at offset 0 for a fixed disk,
  where offset 0 is payload rather than a mirror.

- **VHDX region offsets were checked for 512-byte alignment; the specification
  requires 1 MB.** The first megabyte holds the file identifier, both headers and
  both region tables, so the weaker check admitted a region placed on top of
  them. Region overlap was not checked at all - two regions on the same bytes
  both parse, and whichever is read second decodes bytes belonging to the other.
  Unknown region types participate in the overlap check: the library does not
  know what such a region holds, but it does know nothing else may occupy that
  space. Duplicate region types are refused.

  The unknown-required-region rule now runs before the structural checks, so a
  malformed unknown-required region can no longer be downgraded to "damaged
  table" and skipped by falling through to the secondary copy.

- **VHDX metadata item offsets and lengths were never bounds-checked** against
  the region containing them, so a crafted pair read outside it. Items must now
  fit the region, must not overlap the metadata table itself, must not exceed the
  format's 1 MB maximum, and must have a zero offset when they have zero length.
  Zero-length items are skipped rather than parsed - they were previously
  decoding the table's own signature bytes as a value.

- **The VHDX creator string decoded to a single character.** The file
  identifier's Creator field is 512 bytes of UTF-16 little-endian, and it was
  read as a NUL-terminated byte string — so it stopped at the high byte of the
  first character and "Microsoft Windows 10.0" came back as "M". It is now
  decoded correctly and surfaced through `Provenance`.

- **The VHD per-block sector bitmap computed to zero bytes** for any block
  smaller than 4096, and zero passed validation. A zero-sized bitmap puts every
  block's data one sector early and leaves a differencing disk with no
  sector-presence information, so every sector appears to come from the parent.
  It is now floored at one sector and rounds up rather than truncating.

### Changed

- **`vhdimap.ByteRange` offsets are disk-absolute, not volume-relative.** The
  package originally documented them the other way and told implementations to
  say which they returned. That was the wrong shape: relating two coordinate
  spaces is the one mistake in this design that produces a confident *wrong
  answer* rather than an error, because a volume-relative range compared against
  a whole-disk one still intersects, and every file appears changed or none does.

  Requiring one coordinate space removes the arithmetic instead of documenting
  it. It also matches the parsers rather than fighting them: all six adapted
  libraries already have a `BaseOffset` option that is added to every offset they
  report, so each adapter passes the partition's base and the result needs no
  adjustment. `Capabilities.BaseOffset` now records the base that *was* applied,
  for reporting, rather than one the caller is expected to add.

- **`vhdimap.Filesystem.ExtentsForFile` returns `[]FileExtent`, not
  `[]ByteRange`.** A disk range alone cannot say *where inside a file* a change
  fell, and deriving the position by accumulating run lengths is wrong for any
  file with a hole — a sparse run occupies file offsets while occupying no disk,
  so every offset after the first hole would be shifted. `FileExtent` carries
  both axes, which every one of the six libraries already had to hand.

- **`Version` is now a function, not a constant.** `Version()` derives the
  module version from `runtime/debug.ReadBuildInfo`. It returns `"(devel)"` when
  libvhdi is the main module and `"unknown"` when no build information is
  available, so a report generator can always record *something* about the
  producing tool.

- **A nil `Options.ParentResolver` now means the default, not "do nothing".**
  `Open` and `OpenFile` documented near-identical parent resolution and then
  behaved differently: `OpenFile` searched the image's own directory, `Open` with
  nil options searched nowhere, and nothing reported the difference. A
  differencing disk opened the second way never found its parent, and the
  symptom - `ErrParentRequired` on read - is indistinguishable from a parent that
  is genuinely absent. `Open` also records the path when the reader names a file,
  which is what gives the chain search somewhere to look.

  Callers relying on nil to suppress resolution must now pass
  `NoParentResolution`.

### Added

- **Warnings.** Refusing to open a damaged image and opening it silently are both
  wrong for forensic use: the first loses recoverable evidence, the second
  presents a recovered or unverified reconstruction as an intact one.
  `Disk.Warnings()` returns the caveats for a disk and every parent attached
  below it, each carrying the path of the image it concerns. Four kinds ship:
  `footer_recovered`, `parent_timestamp_mismatch`, `parent_identity_unverifiable`
  and `parent_identity_unchecked`.

- **`FooterSource()` and `FooterRecovered()`**, reporting which copy of the VHD
  footer a disk was opened from. An image opened from a fallback is damaged, and
  an acquisition record has to say so rather than presenting it as an intact
  read.

- **Parent modification time is now compared.** A differencing child records its
  parent's modification time and nothing ever checked it. A parent written to
  since the child was created makes the reconstructed device wrong in a way no
  other check detects - the identifier still matches and the size still matches.
  It is a warning, not an error: filesystem timestamps do not survive a copy, so
  failing the open would refuse far more good images than bad ones.
  `ParentSource.ModTime` carries the value from the resolver.

- **`Geometry()` and `Provenance()`.** The parsers always decoded CHS geometry,
  the creating application, its version and host OS, the feature word, the
  saved-state flag and the size a disk had at creation — and then dropped every
  one of them. None was reachable through the public API, so none could appear in
  a report. `ParsedFileFooter` now retains them and two accessors surface them.

  `Geometry.HasCHS()` distinguishes a VHD that recorded a zero geometry from a
  VHDX, which has no such field at all. `Provenance` is deliberately per-image
  rather than per-chain: a child and its parents are often produced by different
  tools at different times, and flattening that loses the detail that makes a
  chain explicable.

- **`report`**, versioned JSON documents describing images: `DiskReport`,
  `ChainReport`, `CheckpointReport`, `AllocationReport`, `IntegrityReport` and
  `ChangeReport`.

  These are separate data transfer objects rather than json tags on the parse
  structs. Tagging `types.*` would tie the on-disk parse to the on-the-wire
  schema, so renaming a parser field would silently break every stored report.

  Every document carries a `schema_version`, so a consumer can refuse one it
  does not understand rather than misread it; provenance identifying the image,
  its size, an optional SHA-256 and the producing library version; and the
  caveats. GUIDs serialise as canonical strings rather than sixteen numbers.
  Sizes are exact numbers with a human-readable sibling, not instead of one. An
  image that recorded no creation time has the field omitted rather than filled
  with an epoch date.

  `IntegrityReport` is the one with no equivalent before: opening an image
  verifies its structures but a caller only learns pass or fail. It records each
  verdict separately, distinguishing *absent* from *failed* — a fixed VHD has no
  dynamic header and no mirror footer, and reporting those as failures would show
  every healthy fixed image failing checks. On an image that will not open, it
  says which structures survived, which is the difference between writing an
  image off and recovering it.

  `ChangeReport` lives here, file-level DTOs included, so a pipeline that reads
  and validates change reports never imports a filesystem library. Its `tier`
  field makes an absent `volumes` unambiguous: no filesystem analysis was run,
  rather than no files changed.

- **`vhdimap`**, the bridge between libvhdi's block-level view of a disk and a
  filesystem's file-level view of it — interfaces only, still zero-dependency.
  It declares what a volume parser must provide for file-level change detection
  and implements none of it, naming no particular library.

  `Journal` and `OwnerIndex` are separate interfaces rather than methods
  returning `ErrNotSupported`, so a partial implementation is a compile-time
  fact a type assertion can see rather than a runtime surprise. `Capabilities`
  lets a caller distinguish "this format records no access time" from "this
  file's access time was zero" — without it a change report would claim every
  file's atime changed on a filesystem that has none.

  The package's own tests implement the whole contract over a map, which is the
  design's real test: if only a disk-backed parser could satisfy these
  interfaces, the diff engine could not be tested without an image.

- **Block-level change tracking**, the first of three tiers and the only one
  most consumers need. `ChangedExtents(ctx, sinceChainIndex)` returns the ranges
  of the virtual disk written by the disks nearer the leaf than the given index;
  `ChangedSince(ctx, path)` asks the same question by file, which is how a caller
  holding a checkpoint tree thinks about it. `ChangedBytes` sizes a delta
  acquisition. None of it needs filesystem knowledge, which is why it is in the
  core.

- **`ExtentZeroedByChild`**, without which change tracking silently
  under-reports deletions. A differencing disk that clears a region records
  `PAYLOAD_BLOCK_ZERO` or `PAYLOAD_BLOCK_UNMAPPED` — a *write* that reads back
  identically to a region nothing ever touched. `ExtentZero` could not tell the
  two apart, so clearing a region looked like an absence.

  This is VHDX-only, and the limitation is VHD's rather than this library's: a
  VHD differencing disk has no block state meaning zero, and its sector bitmap
  distinguishes only "mine" from "the parent's". A region cleared on a VHD chain
  reads back as the parent's old contents — the clearing did not happen at the
  format level. Deletion-aware change detection needs VHDX, and there is a test
  pinning that so it is not mistaken for a defect here.

  `ExtentKind.ReadsAsZero()` and `ExtentKind.IsWrite()` separate the two
  questions this raises.

- **Checkpoint discovery.** A Hyper-V checkpoint is not a format: it is a
  differencing disk, conventionally named `.avhdx`, whose parent is the disk as
  it stood when the checkpoint was taken. Everything needed to *read* one
  already worked. What was missing was any way to see the shape of a set of them.

  `Chain()` walks upward from one image to its ancestors, which is the wrong
  direction — checkpoints branch, and from a leaf a sibling branch is invisible.
  `DiscoverChain` scans a directory and builds the whole parent-to-children
  `Tree`, with `Roots()`, `Leaves()`, `Lineage()` and `Lineages()`.

  Only headers are read: `Probe`/`ProbeFile` stop short of the block allocation
  table, so surveying a folder of terabyte-scale checkpoints costs a few
  kilobytes per file rather than megabytes. Images are linked by recorded
  identity, never by virtual size or parent filename — either of those would
  build a plausible tree that is wrong. A VHDX child names its parent by the
  parent's *data-write* GUID rather than its virtual disk identifier, and
  `DiskInfo.LinkIdentity` keeps the two apart.

  A file that cannot be probed becomes a node carrying the error rather than
  failing the scan: one corrupt image must not stop the rest of the tree being
  described, and the unreadable file may be the link that would have joined two
  halves of it. Non-image files are recorded in `Tree.Skipped`, since a machine
  folder holds `.vmcx`, `.vmrs` and `.bin` files that would otherwise bury the
  real failures.

  A branched tree has several leaves and **no single current disk**. Which one a
  virtual machine is using is recorded in its configuration, not in the disks, so
  the library reports the branches and declines to guess. Parsing `.vmcx`/`.vmrs`
  is out of scope — they are undocumented proprietary formats — which means
  checkpoint *display names* are not recoverable from the disks alone.

- **`ErrSplitImage`.** Neither specification defines a split or segmented
  multi-file layout; in VHD and VHDX a multi-file disk is always a differencing
  chain. A file named like a VMDK extent is a different format, so it is now
  rejected by name with an explanation rather than reported as an invalid
  signature.

- **Sparse streaming.** The README described sparse acquisition in prose and
  gave callers no way to do it: the only whole-device read was `ReadAt` in a
  loop, which moves the entire virtual size through memory including the zeroes
  the image never stored. `Disk.Stream` walks the extent map and reads only what
  is backed, so a 4 TB device holding 8 GB of data costs 8 GB of reads.

  Runs never span an extent, so each carries one kind, one backing file and one
  chain index — which is what lets a caller record provenance per run rather than
  per disk. `StreamOptions.FailOnUnresolved` chooses what an unattached parent
  means, because neither answer is safe by default: failing loses a partial
  acquisition that may be all there is, and continuing risks a consumer treating
  the gap as zeroes.

  `Disk.SectionReader()` is the other direction — a dense `io.SectionReader` for
  handing the device to a hasher, an archiver or a filesystem parser.

- **Context support**: `ReadAtContext`, `ExtentsContext`, `AllExtentsContext` and
  `MappedBytesContext`. `ReadAt` itself deliberately keeps its signature, since
  being an `io.ReaderAt` is what makes a disk composable with the standard
  library and with a filesystem parser layered on top. `AllExtentsContext` walks
  in chunks so cancellation is checked during the walk rather than only at its
  start; it returns exactly what `AllExtents` returns.

- **`StructuralError` and `ErrUnsupportedFeature`.** A bare "invalid VHD footer
  signature" told an examiner that something failed and nothing else: not which
  copy of the footer, not at what offset. Once the library gives up, examining
  the image by hand is the only recourse, and an error that names a location is
  where that starts. Parser errors now carry the operation, the format, the byte
  offset and the specification's own name for the field at fault.

  `ErrUnsupportedFeature` separates a well-formed image this library cannot read
  — a reserved VHD disk type, an unknown required VHDX metadata item — from a
  damaged one. One says the evidence is broken; the other says to try a different
  tool, and conflating them sends an examiner looking for damage that is not
  there.

  The sentinels moved to `types`, which every other package imports, so a parser
  in `metadata` or `bat` can wrap them without an import cycle back to `reader`.
  `reader.ErrCorruptImage` is now the same value rather than a second one, so
  `errors.Is` answers the same regardless of which layer noticed.

- **`GUIDString` and `ParseGUID`** in the root package. Both formats store the
  first three fields of a GUID little-endian and the rest big-endian, so printing
  the bytes in order yields a string that looks like a GUID and names a different
  one. `examples/open` was making exactly that mistake.

- `NoParentResolution`, a `ParentResolver` that suppresses automatic resolution.

- `ModulePath`, the module path `Version()` keys its build-information lookup on.

- **The `github.com/aoiflux/libvhdi/change` module — file-level change
  tracking.** libvhdi says which byte *ranges* a checkpoint wrote;
  this module says which *files*. It is a separate module, so importing nothing
  costs nothing and the core keeps its zero-dependency guarantee.

  Six adapters ship, over the sibling libraries: NTFS (libntfs), ext2/3/4
  (libext), XFS (libxfs), FAT12/16/32 (libfat), exFAT (libxfat) and HFS+/HFSX
  (libhfs). `change.Compare` takes a disk and a chain position and returns the
  same schema-versioned `report.ChangeReport` the core produces, with the file
  detail filled in.

  The comparison is **block-guided**, not an inventory diff. The changed ranges
  are intersected with the partition table, then with each file's extent map, so
  only files that overlap something written are examined. Inventorying both
  states would mean reading a terabyte to find a megabyte of changes.

  Every finding carries a confidence, and the three levels record which evidence
  was available rather than how good the answer is. `proven` means a journal
  recorded the event, which only NTFS's USN journal supplies. `identified` means
  the filesystem keeps a real reuse counter — ext's generation, XFS's `di_gen`,
  NTFS's sequence number — so the file was followed by an identity the
  filesystem itself maintains. `inferred` means the identity had to be
  synthesised because the format records none, which is FAT, exFAT and HFS+.

  ext and XFS both have journals and both expose them, and neither implements
  `vhdimap.Journal`. jbd2 and the XFS log record block writes, not rename
  events: recovering a rename would mean parsing old directory blocks out of
  journal copies and diffing their entries, which reconstructs the rename rather
  than reading it. Calling that journal-proven would overstate what happened.

  Every reader handed to a filesystem library is wrapped so that it implements
  `io.ReaderAt` and nothing else, so a library that opportunistically
  type-asserts to `io.WriterAt` cannot find one. libntfs is additionally opened
  with its own `ReadOnly` option and the volume is refused if it still reports
  itself writable.

### Removed

Every item here is public surface that could never be satisfied. None of it had
any caller, inside the library or out.

- **`diff/chain.go` in its entirety** — `Chain`, `NewChain`, `AddParent`,
  `Depth`, `Reader`, `BuildVHDResolver`. Zero references and no tests.
  `Disk.Chain()` is the working equivalent.

- **`diff.NewVHDResolver` and `diff.NewVHDXResolver`.** Both were deprecated and
  both returned a resolver with `blockSize == 0` when configuration failed, so a
  caller ignoring the absence of an error got an object that could not resolve
  anything.

- **Six interfaces with no implementations**: `bat.BAT`, `block.BlockReader`,
  `metadata.MetadataReader`, `metadata.ItemReader`, and with them
  `bat.SectorRange` and `block.Block`. `ReadSector` was implemented by nobody and
  `WriteMetadata` was a write method in a read-only library.

- **`VHDXImageHeaderParser.VerifyImageHeaderChecksum`** and its helper. It was
  never called and it was also wrong: it computed the CRC over 4024 bytes, where
  the format specifies all 4096. `ReadImageHeaderAt` does it correctly inline.

- **`types.IOHandle`, `types.AccessFlag`, `types.ParentLocatorHeader`,
  `types.CRC32Polynomial`, `types.CRC32Initial`, `types.CRC32FinalXOR`.**
  Declarations with no referents; CRC-32C lives in `internal/binaryutil`.

- The `Version` string constant. See Changed above.

`VHDXFileInfoParser` was on the same list and is **kept**: it decodes the VHDX
creator string, which is real provenance a forensic report wants. It is wired
into the open path rather than deleted.

### Tests

- **Cycle detection was never tested**, despite a fixture comment saying it
  should be. A two-image A→B→A chain, a self-referential disk, and the
  `RequireParentChain` variant now cover it.
- **VHDX header selection** where the *first* header carries the higher sequence
  number. Every fixture wrote 1 then 2, so the second always won and the
  selection logic never ran. A companion test confirms a higher sequence does
  *not* win when that header fails its checksum — preferring a damaged structure
  over an intact one would be worse than not choosing at all.
- Five-deep chains, a parent whose virtual size disagrees with its child, a
  parent carrying an unreplayable log, and the property that a parent's format is
  detected rather than inherited.
- The `diff` and `metadata` packages had no tests at all; both now have them.
  `diff`'s cover the misconfiguration paths the reader cannot reach, which matter
  because a wrongly-built resolver does not fail loudly — it returns the wrong
  disk.
- **Benchmarks**, of which there were none: the extent walk on a single disk and
  a chain, dense and sparse whole-device reads, random sector reads, and open
  versus probe.
- Fuzz seeds for the footer recovery paths. Recovery code runs precisely when
  the conformant structures have already failed, which makes it the easiest part
  of a parser to trick.
- **A checked-in fuzz corpus**, at `reader/testdata/fuzz/` and
  `internal/vhdxlog/testdata/fuzz/`: 165 inputs the toolchain identified as
  expanding coverage, harvested from its own cache. A corpus that lives only in
  the build cache is discarded on every clean checkout and every CI runner, so
  each run starts from the hand-written seeds and rediscovers the same paths —
  and, more importantly, an input that once provoked a crash stops being tested.
- **The change module's diff engine is tested against an in-memory map**, not
  against filesystem images. That is the real test of the `vhdimap` design: if
  only a disk-backed parser could satisfy those interfaces, the classification
  logic could not be tested without six images existing first. The cases that
  matter are the ones that are hard to produce on demand on a real volume — a
  reused file number reported as a modification, a rename with no journal to
  prove it, a file-relative offset across a hole, two volumes that disagree on
  their own identity.
- The read-only shim is tested by handing it a value implementing both
  `io.ReaderAt` and `io.WriterAt` and asserting the writer cannot be recovered.

### Documentation

- **`docs/MIGRATION.md`** — upgrading from v0.2.0. Most consumers need no
  changes: the only known consumer's call sites compile unchanged, verified by
  reproducing every one of them against this tree.
- **`docs/SUPPORT-MATRIX.md`** — what is supported, what is not, and what is not
  a VHD/VHDX concept at all. Split and segmented multi-file disks are in the
  third category: neither specification defines one.
- **`docs/SPEC-COMPLIANCE.md`** — the five deliberate deviations, each with its
  reasoning, plus the recovery paths that go beyond the specification and the
  qemu disagreement over a fixed VHD's virtual size. Replaces the dangling
  pointer the README used to carry.
- **`docs/ARCHITECTURE.md`** — module layout, the zero-dependency constraint and
  how it is enforced, the three tiers of change tracking, and the base-offset
  hazard that turns a wrong partition offset into a confident wrong answer.
- **`docs/FORENSICS.md`** — what the library guarantees, the four checks to make
  before trusting a result, and the limitations worth stating in a report. Now
  also how to read a change report's confidence levels, and why a document
  reporting no files is not a document that found no changes.
- The support matrix gains a per-filesystem table for the change module, naming
  what each adapter can prove and what it can only infer, plus what a change
  report deliberately does not claim.

### Examples

- **`examples/chain`** — prints the checkpoint tree a directory forms, or the
  differencing chain behind one image. Reports a branched tree as several
  devices and says plainly that which is current cannot be determined from the
  disks.
- **`examples/report`** — emits any of the five document kinds.
- **`examples/changes`** — reports which files a checkpoint wrote, with each
  finding's confidence. `-blocks` skips the filesystem layer and reports the
  changed byte ranges alone, which is what the core module gives you without the
  change module at all. A separate module, like `examples/offsets`.
- `examples/readat` now computes the SHA-256 it previously only imported the
  package for.
- `examples/open` uses the exported `GUIDString` instead of a hand-rolled
  formatter that ignored the format's field ordering.

### Notes

- `Options.AllowMissingParent` was planned in 0.2.0 to let callers opt back into
  the pre-0.2.0 behaviour of returning zeroes for an unresolved parent. It is
  deliberately **not** implemented: it would re-enable the silent-corruption
  mode that release fixed. `ExtentUnresolved` gives a caller the same
  information without the risk of mistaking padding for data.

## [0.2.0] - 2026-07-26

The first release with the fail-closed differencing semantics the library
relies on today: a read that resolves to an unattached parent returns
`ErrParentRequired` rather than zeroes, because zeroes are indistinguishable
from genuine disk contents. A fixed VHD's trailing footer no longer leaks into
the data stream, guarded by `TestFixedVHD_ReadAtDoesNotLeakFooter`.

## [0.1.0]

Initial release. VHD and VHDX parsing, fixed and dynamic disks, checksum
verification.

[Unreleased]: https://github.com/aoiflux/libvhdi/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/aoiflux/libvhdi/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/aoiflux/libvhdi/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/aoiflux/libvhdi/releases/tag/v0.1.0
