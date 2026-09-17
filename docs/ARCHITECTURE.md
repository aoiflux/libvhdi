# Architecture

How libvhdi is laid out, and why the boundaries are where they are.

## The governing constraint

**The core module never acquires a dependency.**

A consumer who wants only VHD/VHDX parsing must be able to take libvhdi without
absorbing anything else into their module graph — not in `go.mod`, not in
`go.sum`, not transitively. Where this conflicts with any other goal, it wins.

This is not a stylistic preference. The library's output is evidence, and a
dependency is a supply-chain surface, a version-conflict risk and an audit
burden for every consumer. A parsing-only consumer should pay none of those for
features they do not use.

It is enforced mechanically rather than documented and hoped for. CI gates every
other job on five checks:

1. `go.mod` declares no requirements.
2. No `go.sum` exists.
3. `go list -m all` reports exactly one module.
4. No foreign `github.com/aoiflux/*` package is reachable.
5. Nothing outside the standard library is reachable at all.

The fifth is the one that states the promise. It tests whether the *first*
element of each import path contains a dot — which is how the Go toolchain
itself tells a domain from a standard library path. Testing the whole path is
wrong: `crypto/internal/entropy/v1.0.0` is standard library and contains a dot.

## Module layout

```
github.com/aoiflux/libvhdi              go.mod: no requires, ever
├── vhdi.go                             root re-exports; the API most callers use
├── types/                              on-disk structures, enums, error sentinels
├── internal/binaryutil/                endian reads, checksums, GUID formatting
├── internal/vhdxlog/                   VHDX log replay
├── internal/buildinfo/                 the library's own version
├── bat/                                block allocation tables
├── block/                              block and sector reads
├── metadata/                           VHDX metadata region
├── diff/                               differencing resolution
├── reader/                             the open path and everything on top of it
├── report/                             versioned JSON documents
└── vhdimap/                            Tier 2: interface declarations only

github.com/aoiflux/libvhdi/change       go.mod: filesystem libraries
└── adapters/{ntfs,ext,fat,exfat,hfs,xfs}
```

Two modules, not eight. Per-adapter modules were considered and rejected: the
filesystem libraries are small, pure Go and zero-dependency, so splitting
further buys nothing and multiplies release ceremony. The boundary that
matters — core versus anything-filesystem — is the one that gets a module.

`examples/offsets` is its own module for the same reason: it uses `libtable` and
`libext`, and folding it into the root would drag both into the core `go.mod`.
That means `go build ./...` at the repository root does not cover it, so CI
builds it separately. The same will apply to `examples/changes`.

## Dependency direction

Every arrow points inward. Nothing in an inner package imports an outer one.

```
report ──┐
         ├──> reader ──> diff ──> block ──> bat ──> types
vhdimap  │              (internal/vhdxlog, internal/binaryutil)
(none)   │
root ────┘
```

`types` is the innermost package and imports only the standard library. The
error sentinels live there for exactly that reason: a parser in `metadata` or
`bat` must be able to wrap `ErrCorruptImage` without importing `reader`, which
would be a cycle. Before v0.3.0 they lived in `reader`, so every error raised
below it was a bare string that `errors.Is` could not match.

`internal/buildinfo` exists so the root package and `report` can both report the
library's version without either importing the other.

`vhdimap` imports nothing but the standard library and is imported by nothing in
the core. It is a contract, not a participant.

## The three tiers of change tracking

The tiers are a layering of *dependency cost*, not of quality. Each is complete
and useful on its own.

### Tier 1 — block level, in the core, zero dependencies

`ChangedExtents` says which byte ranges of the device a checkpoint wrote. That
needs no filesystem knowledge, so it lives in the core and costs nothing. For
delta imaging, targeted acquisition, or driving analysis a caller writes
themselves, this is the whole answer.

The subtlety is deletion. A differencing disk that clears a region records
`PAYLOAD_BLOCK_ZERO` or `PAYLOAD_BLOCK_UNMAPPED` — a *write* that reads back
identically to a region nothing ever touched. `ExtentZeroedByChild` is that
distinction. Without it, change tracking under-reports deletions silently.

### Tier 2 — the bridge, in the core, still zero dependencies

`vhdimap` declares what a filesystem parser must provide and implements none of
it, names no particular library, and imports nothing outside the standard
library.

Optional capabilities are **separate interfaces** rather than methods returning
`ErrNotSupported`. A partial implementation is then a compile-time fact a type
assertion can see, instead of a runtime surprise. A parser implementing only
`Filesystem` gives a working if less precise result; one that also implements
`Journal` gives a better one; neither lies about what it can do.

The package's tests implement the whole contract over a map. That is the real
test of the design: if only a disk-backed parser could satisfy these interfaces,
the diff engine could never be tested without an image.

### Tier 3 — adapters, separate module, opt-in

`libvhdi/change` implements `vhdimap.Filesystem` over the sibling filesystem
libraries and runs the block-guided diff. Not importing it costs nothing.

## Why change detection is block-guided

The naive approach — inventory both volume states and compare — hashes a
terabyte to find a megabyte of changes. libvhdi already knows which ranges
changed, so the algorithm inverts:

1. Open the leaf and the checkpoint being compared against.
2. `ChangedExtents` gives the exact virtual ranges the child wrote.
3. Partition table parsing intersects those with partitions.
4. For each affected partition, open the filesystem at both chain states.
5. Intersect the changed ranges with each file's extent map. **Only the
   overlapping files are examined.**
6. Classify added, deleted, modified and renamed using stable file identity.
7. Where a journal exists, cross-check the renames against it.

Step 5 is what makes it viable, and it is why per-file extent enumeration is the
decisive capability a filesystem library must provide.

### The base-offset hazard

A volume's byte ranges are volume-relative; libvhdi's changed ranges are
whole-disk absolute. Relating them requires the partition's base offset, and
getting it wrong produces a confident **wrong answer** rather than an error —
every file appears changed, or none does, and nothing downstream can tell.

This is why `vhdimap.Capabilities` carries `BaseOffset` and why `ByteRange`'s
documentation states which of the two an implementation returns.

## The `report` package

Report types are separate data transfer objects rather than json tags hung on
the parse structs in `types`. Tagging those would tie the on-disk parse to the
on-the-wire schema, so renaming a parser field would silently break every stored
report.

The *file-level* `ChangeReport` types live here too, in the core, even though
only the `change` module can populate them. Keeping every schema in one place is
what makes versioning coherent, and it means a pipeline that reads and validates
change reports never imports a filesystem library.

## Error design

Three layers, each answering a different question.

- **Sentinels** answer "what kind of problem is this?" — `ErrCorruptImage`,
  `ErrUnsupportedFeature`, `ErrParentRequired`, `ErrDirtyImage`, `ErrSplitImage`.
- **`StructuralError`** answers "where?" — the operation, the format, the byte
  offset and the specification's own name for the field at fault. Once the
  library has given up, examining the image by hand is the only recourse, and an
  error that names a location is where that starts.
- **Warnings** answer "what should qualify this result?" They do not prevent a
  read. Refusing to open a damaged image and opening it silently are both wrong
  for forensic use: the first loses recoverable evidence, the second presents a
  recovered reconstruction as an intact one.

`ErrCorruptImage` and `ErrUnsupportedFeature` are deliberately distinct. One
says the file is broken; the other says to try a different tool. Conflating them
sends an examiner looking for damage that is not there.

## Read-only by construction

No code path writes to an image. VHDX log replay — the one operation that
conceptually modifies state — is applied into a read-only in-memory overlay, so
the file on disk stays byte-identical.

The `change` module will additionally wrap every reader it passes to a
filesystem library in a shim implementing *only* `io.ReaderAt`, so a library
that opportunistically type-asserts to `io.WriterAt` cannot find one.

## Conventions

**`false` is strict.** Every `Options` field that relaxes a check is named so
that the zero value is the safe setting: `AllowParentGUIDMismatch`,
`AllowDirtyImage`, `RequireParentChain`. A caller who does not think about
options gets the careful behaviour.

**Fail closed, never substitute.** A read that cannot be satisfied returns an
error. Returning zeroes would be indistinguishable from genuine disk contents,
and a tool that silently substitutes padding for data is worse than one that
refuses.

**Absence is a fact.** A zero timestamp means the producer recorded none, and is
reported as absent rather than as an epoch date. `Geometry.HasCHS()` separates
"recorded a zero geometry" from "the format has no such field". Reports omit
what an image did not say rather than inventing a value to fill the slot.
