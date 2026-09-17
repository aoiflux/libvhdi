# Migrating to v0.3.0

v0.3.0 is the first release after v0.2.0 and it is breaking, but narrowly. The
removals are API that could never be satisfied; the one behaviour change is a
foot-gun becoming a choice.

**Most consumers need no changes.** A parsing-only consumer using `OpenFile`,
`ReadAt`, `Format`, `Size`, `BlockSize`, `SectorSize` and `IsDifferencing` —
which is the whole of the known consumer's usage — compiles unchanged.

Start here: build against v0.3.0 and see whether anything breaks. If nothing
does, you are done.

## Breaking changes

### `Version` is now a function

```go
// Before
fmt.Println(libvhdi.Version)

// After
fmt.Println(libvhdi.Version())
```

The constant said `"0.6.0"` while the newest tag was `v0.2.0`, so it was simply
wrong and no consumer could have resolved the version it claimed. `Version()`
reads the module version out of the build information the Go toolchain embeds at
link time, so it reports what was actually compiled in and cannot drift again.

It can return `"(devel)"` when libvhdi is the main module, or `"unknown"` when no
build information is available. Both are honest answers, and a report generator
must be able to record that it could not determine its own version rather than
fail or invent one. **Do not compare the result against a version string
without parsing it first.**

`libvhdi.ModulePath` is new and is the module path the lookup keys on.

### A nil `Options.ParentResolver` now means "the default"

```go
// Before: nil suppressed resolution in Open, but was the default in OpenFile.
disk, err := libvhdi.Open(r, nil)   // did NOT resolve parents

// After: nil means the default in both.
disk, err := libvhdi.Open(r, nil)   // resolves parents, like OpenFile

// To suppress resolution, say so:
disk, err := libvhdi.Open(r, &libvhdi.Options{
    ParentResolver: libvhdi.NoParentResolution,
})
```

`Open` and `OpenFile` documented near-identical behaviour and then behaved
differently, and nothing reported the difference. A differencing disk opened
through `Open` never found its parent, and the symptom — `ErrParentRequired` on
read — is indistinguishable from a parent that is genuinely absent.

**Who this affects:** only code that passed `nil` options to `Open` *and*
intended no resolution. If you were attaching parents by hand with `SetParent`
afterwards, pass `NoParentResolution`.

`Open` also now records the path when the reader names one — an `*os.File`
does — which is what gives the chain search somewhere to look.

### Removed API

Every item here had no caller inside the library or out, and in most cases no
possible caller.

| Removed | Use instead |
| --- | --- |
| `diff.Chain`, `NewChain`, `AddParent`, `Depth`, `Reader`, `BuildVHDResolver` | `Disk.Chain()` |
| `diff.NewVHDResolver`, `diff.NewVHDXResolver` | `diff.New(diff.Config{...})` |
| `bat.BAT`, `bat.SectorRange` | — no implementations existed |
| `block.Block`, `block.BlockReader` | — `ReadSector` was implemented by nobody |
| `metadata.MetadataReader`, `metadata.ItemReader` | — `WriteMetadata` was a write method in a read-only library |
| `VHDXImageHeaderParser.VerifyImageHeaderChecksum` | `ReadImageHeaderAt`, which verifies inline and correctly |
| `types.IOHandle`, `types.AccessFlag`, `types.ParentLocatorHeader` | — declarations with no referents |
| `types.CRC32Polynomial`, `CRC32Initial`, `CRC32FinalXOR` | — CRC-32C lives in `internal/binaryutil` |

`VerifyImageHeaderChecksum` is worth calling out: it was not merely unused but
wrong, computing the CRC over 4024 bytes where the format specifies all 4096. If
you were calling it, it was telling you valid headers were corrupt.

### `ExtentKind` has a new value

```go
switch e.Kind {
case libvhdi.ExtentMapped:
    // ...
case libvhdi.ExtentZero:
    // ...
case libvhdi.ExtentUnresolved:
    // ...
case libvhdi.ExtentZeroedByChild:   // NEW
    // A differencing disk explicitly cleared this range.
}
```

`ExtentZeroedByChild` reads as zeroes exactly like `ExtentZero`, so code that
treats unrecognised kinds as "not mapped" stays correct. Code that enumerated
the kinds exhaustively needs the new case.

The distinction matters for change tracking: clearing a region is a *write*, and
a deletion that zeroes its blocks shows up only this way. Two predicates make
the two questions separable:

```go
e.Kind.ReadsAsZero()   // true for ExtentZero and ExtentZeroedByChild
e.Kind.IsWrite()       // true for ExtentMapped and ExtentZeroedByChild
```

This kind is VHDX-only. A VHD differencing disk has no block state meaning
zero, so a cleared region on a VHD chain reads back as the parent's old
contents — see `docs/SUPPORT-MATRIX.md`.

## Behaviour changes that are not API changes

These need no code edits, but they change what you get back.

### VHD timestamps move forward thirty years

Both the footer's creation time and the dynamic header's parent modification
time were decoded with `time.Unix`. VHD counts seconds from **1 January 2000**,
not the Unix epoch, so every date the library reported was thirty years early —
an image created in 2024 presented as 1994.

If you stored timestamps produced by v0.2.0 or earlier, they are wrong by
946,684,800 seconds. Adding that offset corrects them.

A raw zero now decodes as the zero `time.Time` rather than as midnight on
1 January 2000, because a zero field means the producer recorded no timestamp and
rendering it as an instant invents a fact. Test with `IsZero()`.

### VHDX parents are now verified

Before v0.3.0, `MetadataValues.ParentIdentifier` was never assigned, so a VHDX
parent was accepted on virtual size alone — any same-sized VHDX in the search
directory would be attached. **This could silently produce a wrong
reconstruction of a disk.**

A chain that opened before may now fail with `ErrParentMismatch`. That is the
fix working: the image that was being attached was not the parent. If you need
the old behaviour for a damaged chain, `Options.AllowParentGUIDMismatch` is the
opt-out, and it now raises a `parent_identity_unchecked` warning so the
resulting device is not mistaken for a verified one.

### Damaged VHDs may now open

A dynamic or differencing VHD whose trailing footer is unreadable is now
recovered from its mirror at offset 0, and a 511-byte Virtual PC footer is
accepted. Images that previously failed with `invalid VHD footer signature` may
now open.

Check `FooterRecovered()` or the warnings before treating such an image as
intact.

### Stricter VHDX validation

Some malformed images that previously opened are now refused:

- Region offsets must be 1 MB aligned, not 512-byte aligned.
- Regions must not overlap, and no region type may appear twice.
- Metadata items must fit their region and must not overlap the metadata table.
- An unknown *required* region entry or metadata item is refused even when the
  secondary region table is intact.

Each of these admitted an image that would decode into plausible-looking wrong
data. If a real image is refused, that is a bug — please report it with the
image.

### Errors carry more

Parser errors are now `*StructuralError`, carrying the operation, format, byte
offset and field name. They still wrap the same sentinels, so `errors.Is` checks
keep working:

```go
if errors.Is(err, libvhdi.ErrCorruptImage) { /* unchanged */ }
```

New: `ErrUnsupportedFeature`, distinct from `ErrCorruptImage`. A reserved VHD
disk type or an unknown required VHDX metadata item is now the former. If you
were matching on `ErrCorruptImage` to mean "cannot read this image", match on
both:

```go
if errors.Is(err, libvhdi.ErrCorruptImage) || errors.Is(err, libvhdi.ErrUnsupportedFeature) {
    // cannot read
}
```

They are separate because one says the file is broken and the other says to try
a different tool, and conflating them sends an examiner looking for damage that
is not there.

The error sentinels moved from `reader` to `types`, but `reader.ErrCorruptImage`
is now the same value rather than a second one — so `errors.Is` answers the same
regardless of which layer noticed, which it did not before.

## New things worth adopting

None of these are required.

- **`Geometry()` and `Provenance()`** surface CHS geometry, creator application,
  version and host OS, creation time, the saved-state flag and the original size.
  All were parsed and then discarded before.
- **`Stream`** reads only what is backed. A 4 TB device holding 8 GB costs 8 GB
  of reads rather than 4 TB.
- **`DiscoverChain`** builds the parent→children tree of a checkpoint directory
  from headers alone.
- **`ChangedExtents`** says which byte ranges a checkpoint wrote.
- **`report`** produces schema-versioned JSON documents.
- **`GUIDString` / `ParseGUID`**. Both formats store a GUID's first three fields
  little-endian, so printing the bytes in order yields a string that looks like
  a GUID and names a different one.
- **Context variants** of every whole-disk operation. `ReadAt` deliberately
  keeps its `io.ReaderAt` signature.

## Your module graph does not change

libvhdi has no dependencies and v0.3.0 adds none. Upgrading cannot pull anything
into your build, and CI fails the library's own build if that ever stops being
true.

Filesystem-aware features live in the separate `github.com/aoiflux/libvhdi/change`
module. Not importing it costs nothing.
