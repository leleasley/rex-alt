# Stale-entry cleanup: internal engineering note

Date: 2026-11-24
Scope: `internal/store`, `internal/sync` only. No public docs, image tags,
compose files or workflows were touched. Nothing was pushed, tagged, rebuilt or
published.

## Root cause

Altmount's library churns. Rex-Alt's SQLite catalogue can retain entries whose
WebDAV file no longer exists (for example
`movies/Driven.2018.../file.mkv` returns HTTP 404). Every pass re-probes those
entries, every probe fails, the entries are never pushed, and the pass stays
stuck. The fix removes such an entry from the local catalogue when the source
reports 404, so a later enumeration can add it back if the file returns.

## Changes

### internal/store/store.go

Added `func (s *Store) DeleteEntry(id string) error`: a direct `DELETE FROM
entries WHERE id = ?` with no truncation guard (this is one positively
identified missing file, not a bulk diff). Probe and TMDB caches are keyed by id
and are not touched; they are only written for validated probes.

Test: `TestDeleteEntryRemovesOnlyTheNamedEntry` inserts two entries, deletes
one, and asserts the deleted id is gone while the other row is unchanged.

### internal/sync/sync.go

- `probeMissing` now takes `errs []error`, indexed like `results`, `valid` and
  `fresh`. Both probe errors and probe-validation errors are recorded, so a 404
  from ffprobe *or* from the ranged WebDAV sample reader is visible to the
  caller. Concurrent workers write distinct slice indices, as they already do
  for results/valid/fresh.
- `SyncOnce` allocates `errs`, and after `probeMissing` iterates the misses:
  when `strings.Contains(strings.ToLower(err.Error()), "404")` it calls
  `s.db.DeleteEntry(work[i].ID)`. A delete failure aborts the pass with a
  wrapped error. Non-404 failures keep the previous held behaviour.
- 404 items remain invalid: they are neither cached nor pushed, and are still
  counted as held. No `rex.Delete` call was added; zero-fill validation, Diff
  truncation guards, the uncapped catalogue and all other behaviour are
  unchanged.

Test: `TestSyncOnceDropsEntryWhoseProbeReturns404` lists two movies, injects a
ProbeFunc that returns `404 Not Found` for one and a valid probe for the other,
and asserts: Probed=2, Pushed=1, Held=1; only the healthy entry remains in the
store; its probe is cached; only its item is pushed; and REX deletes stay at 0.

## TDD evidence

1. RED (tests first, before production code):
   - store: `internal/store/store_test.go:183:14: s.DeleteEntry undefined` —
     build failed because the method did not exist.
   - sync: `--- FAIL: TestSyncOnceDropsEntryWhoseProbeReturns404` — the 404
     entry was still in the catalogue after the pass.
2. GREEN (after minimal implementation):
   - `go test -count=1 ./internal/store/ ./internal/sync/` — both packages ok.
   - `go vet ./...` — clean (no diagnostics).

A mutation check: removing the `DeleteEntry` call, the `errs[idx]` writes, or
the 404 test each makes the new sync test fail; removing DeleteEntry makes the
store test fail to compile.

## Verification

Command requested by the caller:

```
docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine \
  sh -c 'go mod tidy && go test ./... && go vet ./...'
```

Result: all packages passed (`cmd/rex-alt`, `altmount`, `api`, `config`,
`library`, `models` no tests, `rex`, `store`, `sync`, `tmdb`) except the
pre-existing flaky `internal/probe` test `TestProbeTimesOutAfterOneRetry`
(`probe_test.go:217: ffprobe invoked 0 times, want exactly 2`). Because the
`&&` chain stops there, `go vet ./...` was run separately in the same image and
completed cleanly; a cached-module run of `go test -count=1 ./internal/store/
./internal/sync/` also passed. The failing probe test is untouched by this fix
and failed identically before the implementation (and on repeated standalone
runs with 0 or 1 invocations).

Commit: `ba16965 fix: drop catalogue entries whose probe reports HTTP 404`.

## Concerns

- `internal/probe` has a pre-existing timing-sensitive test,
  `TestProbeTimesOutAfterOneRetry`, which in this environment reports
  `ffprobe invoked 0 or 1 times, want exactly 2` (100 ms outer timeout with a
  shell-script fake). It fails independently of these changes and was not
  modified.
- During verification, `internal/tmdb/parser.go` and `parser_test.go` appeared
  as concurrent uncommitted edits from another actor in the shared workspace;
  they were left untouched and were not staged. They passed when retested
  after that actor settled.
- The 404 detection uses the caller-specified substring heuristic on the error
  text; an unrelated error message containing "404" (for example a filename)
  would also trigger deletion.
- Deleting the catalogue row does not delete a probe-cache row. In practice a
  404 entry is a probe miss, so it has no fresh cached probe; older cached rows
  (if any) are left by design so a returning file can be reused.
