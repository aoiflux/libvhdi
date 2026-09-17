# Fuzz corpus

Checked-in seed inputs for the fuzz targets in this package. `go test` replays
every file here on an ordinary run, so anything the fuzzer once found stays
found.

## What these are

Each file is one input that expanded coverage during a fuzzing run — the
toolchain's own judgement of which mutations reached code the others did not.
They are not random bytes: the corpus is dominated by near-valid structures,
because that is what gets past the first length check and into the parsing that
is worth testing. Several carry a recognisable `conectix` cookie or `vhdxfile`
signature with one field corrupted.

## Why they are committed

A corpus that lives only in the build cache is discarded on a clean checkout and
on every CI runner, so each run starts from the hand-written seeds and has to
rediscover the same paths. Committing it means a 60-second smoke run in CI
begins where the last long run left off, and — the part that matters — an input
that once provoked a crash can never stop being tested. That is what makes a
fixed parser bug stay fixed.

## Adding to it

Fuzzing writes a failing input to `testdata/fuzz/<Target>/` automatically. Keep
it: it is the regression test for whatever it found, and its file name is the
one the failure message cites.

To refresh the coverage corpus after a long run, copy the toolchain's cache:

```
go test ./reader/ -run '^$' -fuzz FuzzOpen -fuzztime 5m
cp "$(go env GOCACHE)/fuzz/github.com/aoiflux/libvhdi/reader/FuzzOpen/"* \
   reader/testdata/fuzz/FuzzOpen/
```

Prune rather than let it grow without limit. Every file costs time on every
`go test` run in the repository, and an input that no longer reaches anything
the others do not is paying rent without earning it.
