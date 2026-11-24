# ffprobe forced-Range root-cause fix

## Task and context

Task 8 owner (rex-alt ffprobe pipeline). Partner evidence came from a live
Altmount mount:

- With `-headers "Authorization: Basic <token>\r\nRange: bytes=0-\r\n"`,
  ffprobe failed on many files:
  - MKV → `Stream ends prematurely at 0, should be 2735245234`
  - MP4 → `moov atom not found`
- Altmount truncates requested ranges: a 1,048,576-byte range came back with
  only 716,800 bytes. ffprobe therefore received incomplete container data.
- With ONLY the Authorization header (no forced Range), ffprobe succeeded on
  the same MKV: exit 0, 2 streams.
- 816 of Altmount's 877 files are MKV; only 61 are MP4. The forced Range was
  blocking the large majority of the library from ever being probed/pushed.

Approved baseline: `955fcc8` (`release: pin v0.1.7 (WWE daily show matching)`),
clean working tree. No push, tag, rebuild or publish was performed.

## Root cause

`probe.Probe` appended `Range: bytes=0-` to ffprobe's `-headers` value to make
Altmount return a 206 partial response. Altmount does not honour the full
requested span: it silently truncates it, so ffprobe's demuxer saw a short
stream. MKV needs its header/segment metadata and MP4 needs the `moov` atom;
when those bytes are missing the container cannot be parsed at all, hence the
two reported errors. Because 816/877 items are MKV, almost every probe failed.

## Change (TDD, tests first)

### Test changed before implementation

`internal/probe/probe_test.go`: `TestProbePassesRangeAndAuthToFFprobe` was
renamed to `TestProbePassesAuthAndBoundedProbeFlagsToFFprobe` and now asserts:

1. the exact pinned argv prefix, including
   `-probesize 33554432 -analyzeduration 10000000` immediately before
   `-headers`;
2. the verbatim `-headers` value is exactly the CRLF-terminated Authorization
   line (47 bytes for the fixture credentials) — `strings.Split(..., "\r\n")`
   yields exactly `["Authorization: Basic <token>", ""]`;
3. no `Range:` line appears anywhere in `-headers`;
4. no plaintext user/password appears in the captured header value.

Failure messages only print byte counts and flag names, never the token.

### RED stage 1 — new assertions against unmodified implementation

Command:

```
cd /opt/rex-alt && docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'go test ./internal/probe/ -run TestProbePassesAuthAndBoundedProbeFlagsToFFprobe -v 2>&1'
```

Output (exact):

```
=== RUN   TestProbePassesAuthAndBoundedProbeFlagsToFFprobe
    probe_test.go:93: got 12 ffprobe args, want 16 (argv not printed: carries the auth token)
--- FAIL: TestProbePassesAuthAndBoundedProbeFlagsToFFprobe (0.05s)
FAIL
FAIL	github.com/leleasley/rex-alt/internal/probe	0.053s
FAIL
```

Exit code 1: the two bounded-probe flags are absent from argv.

### RED stage 2 — flags added, forced Range still present

After adding `-probesize`/`-analyzeduration` to `runFFProbe` but before
removing the Range line, the same test failed on the header assertions:

```
=== RUN   TestProbePassesAuthAndBoundedProbeFlagsToFFprobe
    probe_test.go:112: ffprobe -headers value is not the auth line only (got 64 bytes, want 47)
    probe_test.go:116: ffprobe -headers lines are malformed: got 3 CRLF-separated parts, want Authorization / empty
    probe_test.go:119: ffprobe -headers still contains a forced Range line
--- FAIL: TestProbePassesAuthAndBoundedProbeFlagsToFFprobe (0.04s)
FAIL
FAIL	github.com/leleasley/rex-alt/internal/probe	0.048s
FAIL
```

Exit code 1. The 64 vs 47 byte difference is exactly the 17-byte
`Range: bytes=0-\r\n` suffix.

### GREEN — implementation

`internal/probe/probe.go`:

- `Probe` now uses `headers := auth`, i.e. only `basicHeader(user, pass)`
  (`Authorization: Basic <base64>\r\n`). The CRLF terminator is deliberately
  kept.
- `runFFProbe` pinned argv order is now:

  ```
  -v error
  -rw_timeout <floored micros>
  -print_format json
  -show_format -show_streams -show_chapters
  -probesize 33554432
  -analyzeduration 10000000
  -headers <Authorization only>
  <WebDAV URL>
  ```

  The bounds cap ffprobe at 32 MB / 10 s of analysis instead of a forced
  range. Unchanged: retry-on-cold-start, `Validate`, URL sanitisation,
  timeout handling and the `-rw_timeout` floor, `safeErrorText` redaction
  (the `auth` value is retained for that purpose), and `WaitDelay`.
- Stale package/function comments about forcing a Range were corrected.

`internal/sync/sync.go`: corrected the `NewProbeFunc` doc comment that still
claimed a Range request was sent. Code behaviour there is unchanged.

Targeted GREEN output (exact):

```
=== RUN   TestProbePassesAuthAndBoundedProbeFlagsToFFprobe
--- PASS: TestProbePassesAuthAndBoundedProbeFlagsToFFprobe (0.05s)
PASS
ok  	github.com/leleasley/rex-alt/internal/probe	0.053s
```

## Full verification

Command (exact, as instructed):

```
docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'go mod tidy && go test ./... && go vet ./...'
```

Output (exact; `go vet` is silent on success):

```
go: downloading modernc.org/sqlite v1.61.1
go: downloading modernc.org/libc v1.79.0
go: downloading golang.org/x/sys v0.49.0
go: downloading github.com/google/pprof v0.0.0-20261016150608-1facf8f91b50
go: downloading modernc.org/fileutil v1.4.0
go: downloading modernc.org/mathutil v1.7.1
go: downloading github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec
go: downloading github.com/google/uuid v1.6.0
go: downloading github.com/dustin/go-humanize v1.1.0
go: downloading github.com/mattn/go-isatty v0.0.24
go: downloading github.com/ncruces/go-strftime v1.1.0
go: downloading modernc.org/memory v1.12.1
go: downloading golang.org/x/tools v0.52.0
go: downloading modernc.org/cc/v4 v4.30.3
go: downloading modernc.org/ccgo/v4 v4.37.0
go: downloading modernc.org/goabi0 v0.2.0
go: downloading modernc.org/sortutil v1.2.1
go: downloading modernc.org/strutil v1.2.1
go: downloading modernc.org/opt v0.2.0
go: downloading modernc.org/token v1.1.0
go: downloading golang.org/x/sync v0.24.0
go: downloading golang.org/x/mod v0.42.0
go: downloading modernc.org/gc/v2 v2.6.5
go: downloading modernc.org/gc/v3 v3.2.0
go: downloading github.com/hashicorp/golang-lru/v2 v2.0.7
ok  	github.com/leleasley/rex-alt/cmd/rex-alt	0.005s
ok  	github.com/leleasley/rex-alt/internal/altmount	0.029s
ok  	github.com/leleasley/rex-alt/internal/api	0.061s
ok  	github.com/leleasley/rex-alt/internal/config	0.011s
ok  	github.com/leleasley/rex-alt/internal/library	0.006s
?   	github.com/leleasley/rex-alt/internal/models	[no test files]
ok  	github.com/leleasley/rex-alt/internal/probe	8.924s
ok  	github.com/leleasley/rex-alt/internal/rex	0.415s
ok  	github.com/leleasley/rex-alt/internal/store	0.212s
ok  	github.com/leleasley/rex-alt/internal/sync	0.713s
ok  	github.com/leleasley/rex-alt/internal/tmdb	0.616s
```

`go mod tidy` made no changes to `go.mod`/`go.sum` (verified with
`git status --short`: only the three files below are modified).

## Credential handling

- `-headers` holds only the Base64 Basic token, never plaintext.
- No argv is ever logged; the tests' mismatch messages print flag names and
  byte counts only.
- `safeErrorText` still receives the exact `auth` string and redacts it (plus
  token, user:pass, raw URL) before any error escapes, and
  `TestProbeErrorsNeverLeakCredentials` remains green.

## Concerns

- Verification used the repository's `fake-ffprobe.sh` test double; no real
  ffprobe binary or live Altmount mount was available on the host, so the live
  MKV/MP4 recovery is supported by the partner's evidence and by argv/header
  assertions, not by a live re-probe.
- `-probesize 33554432` is a 32 MB cap: it bounds reads but is still larger
  than some pathological files' metadata window. It can be tuned later via
  the pinned argv if monitoring shows slow probes; changing it requires
  updating the test's exact prefix.
- Removing the forced Range means ffprobe controls its own ranged requests;
  Altmount truncation could still affect ffprobe's internally issued ranges,
  but the partner's live test showed success without the forced header.

## Files changed

- `internal/probe/probe.go`
- `internal/probe/probe_test.go`
- `internal/sync/sync.go` (comment only)
- `docs/probe-range-fix.md` (this report)
