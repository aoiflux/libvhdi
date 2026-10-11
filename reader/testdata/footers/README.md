# Windows-written VHD footers

Each `.bin` file is the trailing 512 bytes, the hard disk footer, of a VHD
created by Windows. `TestWindowsFootersMatchTheCodecs` checks the exported
codecs against them on every `go test`: the checksum, the CHS geometry, and a
byte-for-byte round trip through `decodeFooter` and `MarshalBinary`.

Only the footers are committed. The images themselves were deleted after
capture; a 40 GiB fixed image has no place in the repository.

| File | Type | Virtual size | Recorded CHS |
| --- | --- | --- | --- |
| `fixed-3MiB.bin` | fixed | 3 MiB | 90/4/17 |
| `fixed-512MiB.bin` | fixed | 512 MiB | 1040/16/63 |
| `fixed-1GiB.bin` | fixed | 1 GiB | 2080/16/63 |
| `fixed-40GiB.bin` | fixed | 40 GiB | 20560/16/255 |
| `dynamic-127GiB.bin` | dynamic | 127 GiB | 65278/16/255 |

## Provenance

- **Host:** Windows 11 Pro 26H2, build 26300.9550, captured 2026-10-11.
- **Producer:** `CreateVirtualDisk` in `virtdisk.dll`, the Windows virtual disk
  API, called from a small Go program. The footers record Creator Application
  `win `, version `0x000A0000`, and Creator Host OS `Wi2k`.
- **Parameters:** `VIRTUAL_STORAGE_TYPE_DEVICE_VHD` with the Microsoft vendor
  GUID; `CREATE_VIRTUAL_DISK_PARAMETERS` version 1 with `MaximumSize` set to the
  virtual size, `SectorSizeInBytes` 512 and the default block size; access mask
  `VIRTUAL_DISK_ACCESS_CREATE`. Fixed disks pass
  `CREATE_VIRTUAL_DISK_FLAG_FULL_PHYSICAL_ALLOCATION`; the dynamic disk passes
  no flags.

`New-VHD` was not used because it needs an elevated session, and the capture
ran unelevated. `CreateVirtualDisk` is the documented Windows API for creating
a VHD, and the closest `New-VHD` command is:

```powershell
New-VHD -Path fixed-1GiB.vhd -SizeBytes 1GB -Fixed
```

`TestCorpusVHDFootersMatchTheCodecs` covers images made by `New-VHD` itself,
whenever `scripts/gen-corpus.ps1` has produced a corpus.

The 127 GiB disk is dynamic rather than fixed because a fixed image of that size
needed more free space than the capture machine had. The footer encodes the
geometry and checksum the same way for both types; only Disk Type and Data
Offset differ.

## Regenerating

From an elevated PowerShell session with the Hyper-V module:

```powershell
$sizes = @{ 'fixed-3MiB' = 3MB; 'fixed-512MiB' = 512MB; 'fixed-1GiB' = 1GB; 'fixed-40GiB' = 40GB }
foreach ($name in $sizes.Keys) {
    $vhd = Join-Path $env:TEMP "$name.vhd"
    New-VHD -Path $vhd -SizeBytes $sizes[$name] -Fixed | Out-Null
    $f = [IO.File]::OpenRead($vhd)
    $b = [byte[]]::new(512)
    $f.Seek(-512, 'End') | Out-Null
    $f.Read($b, 0, 512) | Out-Null
    $f.Dispose()
    [IO.File]::WriteAllBytes((Join-Path $PWD "$name.bin"), $b)
    Remove-Item $vhd
}
New-VHD -Path "$env:TEMP\dynamic-127GiB.vhd" -SizeBytes 127GB -Dynamic | Out-Null
# then copy the last 512 bytes of that image the same way
```

Each footer carries a fresh random Unique Id and a new timestamp, so the bytes
change on every run. The geometry and sizes must not.
