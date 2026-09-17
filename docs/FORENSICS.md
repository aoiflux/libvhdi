# Forensic use

Written for examiners and for people building acquisition or analysis tooling on
libvhdi. It covers what the library guarantees, what it will not do for you, and
the places where a plausible-looking result can be wrong.

## What the library guarantees

**Nothing is written to an image.** No code path writes. VHDX log replay — the
one operation that conceptually changes state — goes into a read-only in-memory
overlay, so the file on disk stays byte-identical. You can point libvhdi at
original evidence.

**A read that cannot be satisfied fails.** A differencing disk whose parent is
missing returns `ErrParentRequired` for the affected ranges rather than zeroes.
Zeroes would be indistinguishable from genuine disk contents, and a tool that
silently substitutes padding for data produces evidence that cannot be trusted
anywhere.

**A parent is never accepted on size alone.** Before v0.3.0 a VHDX parent was
attached if its virtual size matched, so any same-sized image in the directory
would do. That could silently produce a wrong reconstruction of a disk. Parents
are now verified by recorded identity.

**Recovery is always visible.** An image opened from a fallback footer copy is
reported as recovered, in `FooterSource()`, in a warning, and in every report
that describes it.

## Checks to make before trusting a result

### 1. Is the chain complete?

```go
if !disk.ChainComplete() {
    // Ranges backed by the missing images cannot be read.
    log.Printf("chain incomplete: %v", disk.ParentResolveError())
}
```

An incomplete chain still opens, by design: a partial reconstruction is usually
better than none, and refusing would discard what you have. But the device is
not whole, and a report built from it describes only part of a disk.

### 2. Were there warnings?

```go
for _, w := range disk.Warnings() {
    log.Printf("%s", w)
}
```

Four kinds ship, and each changes how a result should be read:

| Kind | What it means |
| --- | --- |
| `footer_recovered` | The conformant footer was unreadable. The image is damaged; it opened from a fallback copy |
| `parent_timestamp_mismatch` | The parent file's modification time differs from what the child recorded. Often benign — timestamps do not survive a copy — but it can mean the parent was **written to after the child was created**, which makes the reconstruction wrong in a way nothing else detects |
| `parent_identity_unverifiable` | The child records no parent identifier, so its parent was matched on virtual size alone |
| `parent_identity_unchecked` | Verification was disabled by `AllowParentGUIDMismatch` |

Warnings cover the whole chain, since a differencing disk is only as trustworthy
as the disks behind it, and each names the image it concerns.

### 3. Is the image dirty?

```go
if disk.IsDirty() {
    // Opened with AllowDirtyImage: the log was not replayed.
}
```

A VHDX carrying an unreplayed log may not reflect its last committed state —
the block allocation table and metadata can be stale. By default such an image
is refused with `ErrDirtyImage`; `AllowDirtyImage` trades that hard failure for
a documented risk.

`HasLog()` being true with `LogReplayed()` also true is the ordinary case: the
image was captured mid-write and the log was applied cleanly. That is worth
recording — it means the image was taken from a *running* machine.

### 4. Was it saved from a running machine?

```go
if disk.Provenance().SavedState {
    // Crash-consistent, not cleanly unmounted.
}
```

A saved-state VHD holds a filesystem snapshot taken without the guest shutting
down. Journals may be unreplayed and files may be mid-write. That changes what
the contents can be taken to mean, and it is not something a filesystem parser
will tell you.

## Acquisition

### Sparse acquisition

Reading a device end to end moves its whole virtual size through memory, most of
it zeroes the image never stored. `Stream` reads only what is backed:

```go
err := disk.Stream(ctx, nil, func(r libvhdi.Run) error {
    // r.Extent.Kind is always ExtentMapped here by default.
    // r.Data is reused between calls -- copy anything you keep.
    return writeAt(r.Extent.VirtualOffset, r.Data)
})
```

A 4 TB device holding 8 GB of data costs 8 GB of reads.

Every run carries a single kind, backing file and chain index, so provenance can
be recorded per run rather than per disk — which is what lets an acquisition
record say *which image in the chain* supplied each range.

### Decide what an unresolved range means

```go
opts := &libvhdi.StreamOptions{FailOnUnresolved: true}
```

Neither answer is safe by default. Failing loses a partial acquisition that may
be all there is; continuing risks a consumer treating the gap as zeroes. The
kind is on every run, so make the decision with the facts rather than letting
the library guess.

### Hash the file, or hash the device?

They answer different questions.

- **The file's** SHA-256 (`report.Options{Hash: true}`) identifies the artefact
  you were given. Two images with identical contents but different block layouts
  have different file digests.
- **The device's** digest identifies the data. Compute it over
  `disk.SectionReader()`, which reads densely including sparse regions.

For chain of custody you want the file's. For establishing that two images hold
the same disk, you want the device's.

## Checkpoints

### Discovery reads headers only

```go
tree, err := libvhdi.DiscoverChain(ctx, dir, nil)
```

No block allocation table is read. A probe's cost is fixed — two headers, up to
two 64 KB region tables and the metadata region, so on the order of 150 KB per
image — while a full open additionally reads the whole BAT, which scales with the
virtual disk: a million entries for a 1 TB disk with 1 MB blocks. The saving is
in the term that grows, so it is negligible on small images and large on the
ones a checkpoint directory actually holds.

### A branched tree has no "current" disk

Applying an earlier checkpoint and continuing gives one parent two children.
`tree.Branched()` reports this, and `tree.Lineages()` returns one lineage per
leaf — each a complete, distinct device.

**Which leaf a machine is using is not determinable from the disks.** It lives
in the machine's `.vmcx` configuration, an undocumented proprietary format that
is out of scope. The library reports the branches and declines to guess, because
a guess here would be wrong silently. So are checkpoint *display names*: the
disks do not carry them.

### Unreadable files stay in the tree

A file that cannot be probed becomes a node carrying the error. One corrupt
image must not stop the rest of the tree being described — and the unreadable
file may be exactly the link that would have joined two halves of it.

```go
for _, n := range tree.Unreadable() {
    log.Printf("%s: %v", n.Path, n.Err)
}
```

### Role follows the disk type, not the filename

`.avhdx` is Hyper-V's convention, not the format's. A renamed differencing disk
is still a checkpoint; a base disk given the extension is not one.
`HasCheckpointExtension()` reports the convention separately, so a disagreement
between the two can be seen rather than quietly resolved.

## Change tracking

### What block level can and cannot tell you

`ChangedExtents` says which byte ranges a checkpoint wrote. It does not say
which files, and it cannot: that needs a filesystem parser.

```go
changed, err := disk.ChangedExtents(ctx, 1)   // what the leaf wrote
```

Two kinds appear. `ExtentMapped` is data written. `ExtentZeroedByChild` is a
region explicitly cleared, which is how a deletion shows up — **filtering on
`ExtentMapped` alone under-reports deletions.**

`ExtentUnresolved` is neither: the image that would say is missing, and calling
it unchanged would be a guess in the direction that loses evidence.

### Deletion detection requires VHDX

A VHD differencing disk has no block state meaning "zero". Its sector bitmap
distinguishes only *this sector is mine* from *read from the parent*, so a
cleared region reads back as the parent's old contents.

The clearing did not happen at the format level. This is not a gap in libvhdi —
there is nothing to detect. On a VHD chain, `ChangedExtents` will report no
change for a region the guest cleared.

### File-level tracking is a separate module

`libvhdi/change` implements the bridge over the sibling filesystem libraries.
Not importing it costs nothing: the core has no filesystem dependency and never
will.

```go
doc, err := change.Compare(ctx, disk, 1, nil)   // since the immediate parent
for _, v := range doc.Volumes {
    for _, f := range v.Files {
        fmt.Printf("%-9s %s (%s)\n", f.Change, f.Path, f.Confidence)
    }
}
```

**Read the confidence, not just the verdict.** Three levels ship, and they say
which evidence was available rather than how good the answer is:

| Confidence | What produced it |
| --- | --- |
| `proven` | A journal recorded the event. NTFS's USN journal is the only source, and it is circular, so a rename that has aged out of it drops to the level below |
| `identified` | The filesystem keeps a real reuse counter — ext's generation, XFS's `di_gen`, NTFS's sequence number — so the file was followed across both states by an identity the filesystem maintains. Not a guess; what is missing is the event |
| `inferred` | The identity was synthesised because the format records none. FAT, exFAT and HFS+ are here, and on them a reused directory slot reads as a modification and a rearranged directory reads as a delete plus an add |

**A document that reports no files is not a document that found no changes.**
Check `Tier` first: `block` means no filesystem was read at all, which happens
when the older image is missing, when the volume holds a format no adapter
reads, or when `SkipVolumes` was set. The warnings say which.

**The two states must be the same volume.** Comparing two different ones reports
every file as changed, and nothing in the output would say the comparison was
meaningless, so it is refused by default with `ErrVolumeMismatch`.
`AllowVolumeIdentityMismatch` overrides it and leaves a note in the document
saying so.

**Deletions still need VHDX**, for the reason above: a VHD chain cannot record
that a region was cleared.

## Reports

Six document types, each schema-versioned so a consumer can refuse a document it
does not understand rather than misread it.

The one worth knowing about is `IntegrityReport`, because it is the only way to
learn something from an image that **will not open**:

```go
r, err := report.Integrity(ctx, path, nil)
// err is nil even for a badly damaged image.
for _, c := range r.Checks {
    fmt.Printf("%-24s %s %s\n", c.Name, c.Status, c.Detail)
}
```

It records each structure's verdict separately, distinguishing *absent* from
*failed* — a fixed VHD has no dynamic header and no mirror footer, and treating
those as failures would show every healthy fixed image failing checks. On a
damaged image it says which structures survived, which is the difference between
writing an image off and recovering it.

`CheckpointReport` carries a `limitations` field stating what it cannot say, so
its silence about display names is not mistaken for a finding.

## Limitations worth stating in a report

- **No filesystem is parsed.** The library presents a block device.
- **ReFS is unsupported**, and no Go implementation is known. Windows Server
  hosts commonly format VM storage as ReFS, so you may meet a disk libvhdi
  decodes correctly and no available tool can interpret.
- **`.vmcx` and `.vmrs` are not parsed**, so checkpoint names and the current
  leaf are unavailable.
- **`.rct` and `.mrt` sidecars are not parsed.** Changed ranges are derived from
  the differencing chain itself.
- **VHD cannot express an explicit zero**, so deletions are invisible at block
  level on VHD chains.
- **`parent_linkage2` is not parsed**, so a VHDX child recording only that key
  would not verify against its parent. No producer is known to do this.

## Reproducibility

Record, alongside any result:

- `libvhdi.Version()` — derived from build information, so it cannot drift from
  the published tag the way a hand-maintained constant did before v0.3.0. It may
  be `"(devel)"` or `"unknown"`, which are honest answers.
- The file path, size and digest of every image in the chain. `report.Chain`
  gives the set of files that together constitute the device, which is what an
  evidence record has to name.
- Every warning.
- The options used, particularly `AllowDirtyImage` and
  `AllowParentGUIDMismatch`, since both relax a check that exists for a reason.

Every report type records the first, and carries the rest.
