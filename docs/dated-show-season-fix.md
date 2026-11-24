# Dated WWE show season/episode fix (internal report)

**Commit:** `96d6117 fix: derive WWE brand season and episode from air dates`
(base `40ce68f`). Only `internal/tmdb/parser.go` and
`internal/tmdb/parser_test.go` were changed. `client.go` — including
`tvSearchAliases` and `tvSearchTitle` — was deliberately left untouched. No
push, tag, rebuild or publish.

## Problem

Items such as `WWE.Monday.Night.RAW.2026.11.23...` were parsed with
`season=0, episode=0`, so every Raw episode landed in season 1 after the sync
fallback. The air date was correctly truncated from the title and `year` stayed
0, but no brand-specific numbering was derived from it.

Verified TMDB facts for Raw (id 4656): season 34 covers 2026 with 52 episodes;
2026-11-16 is episode 46 (ISO week 47), 2026-11-23 is episode 47 (ISO week 48).
Hence `season = airYear - firstAirYear + 1` and `episode = ISOWeek(airDate) - 1`.

ISO weeks were confirmed with a temporary in-package probe (created, run and
removed inside the container):

```
$ docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c '... go test -v -run TestZZWeeks ./internal/tmdb/; rm internal/tmdb/zz_week_test.go'
=== RUN   TestZZWeeks
2026-11-23 -> ISO year=2026 week=48 episode=47
2026-11-20 -> ISO year=2026 week=47 episode=46
2026-11-16 -> ISO year=2026 week=47 episode=46
2026-11-18 -> ISO year=2026 week=47 episode=46
1993-01-11 -> ISO year=1993 week=2 episode=1
--- PASS: TestZZWeeks (0.00s)
PASS
ok  	github.com/leleasley/rex-alt/internal/tmdb	0.454s
EXIT=0
```

## Changes

- `internal/tmdb/parser.go`:
  - Added `wweFirstAirYears`, keyed by the same normalised parsed show names as
    the documented TV search aliases: Raw variants → 1993, SmackDown variants →
    1999, NXT variants → 2010.
  - Captured the matched air-date text (`YYYY.MM.DD`, `YYYY-MM-DD` or spaced)
    when the TV date branch truncates the title, so the existing truncation and
    `year=0` behaviour are unchanged.
  - At the end of `ParseRelease`, when a dated TV release has no `SxxExx`
    marker and its final parsed title is a known brand, set
    `season = airYear - firstAirYear + 1` and `episode = ISOWeek - 1`, both
    clamped to at least 1. A new `parseAirDate` helper rejects invalid calendar
    dates; non-WWE dated shows keep 0/0.
- `internal/tmdb/parser_test.go`: extended `TestParseReleaseDatedDailyShow`
  with expected season/episode for every case: Raw 2026-11-23 → 34/47,
  SmackDown 2026-11-20 → 28/46, Raw 2026-11-16 (dotted and dashed) → 34/46,
  NXT 2026-11-18 → 17/46, Raw 1993-01-11 → 1/1, and a non-WWE control
  (`Some Other Daily Show`) → 0/0.

## TDD — RED then GREEN

```
$ docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'go test -v -run TestParseReleaseDatedDailyShow ./internal/tmdb/'
=== RUN   TestParseReleaseDatedDailyShow
=== RUN   TestParseReleaseDatedDailyShow/WWE.Monday.Night.RAW.2026.11.23.1080p.WEBRip.h265-Star.mp4
    parser_test.go:255: season/episode = 0/0, want 34/47
=== RUN   TestParseReleaseDatedDailyShow/WWE.Friday.Night.SmackDown.2026.11.20.1080p.WEBRip.AAC2.0.HFR.H.265-SkY.mkv
    parser_test.go:255: season/episode = 0/0, want 28/46
=== RUN   TestParseReleaseDatedDailyShow/WWE.RAW.2026.11.16.1080p.WEBRip.x264-DEF.mkv
    parser_test.go:255: season/episode = 0/0, want 34/46
=== RUN   TestParseReleaseDatedDailyShow/WWE.RAW.2026-11-16.1080p.WEBRip.x264-DEF.mkv
    parser_test.go:255: season/episode = 0/0, want 34/46
=== RUN   TestParseReleaseDatedDailyShow/WWE.NXT.2026_11_18.1080p.WEB-DL.x264-GRP.mkv
    parser_test.go:255: season/episode = 0/0, want 17/46
=== RUN   TestParseReleaseDatedDailyShow/WWE.Monday.Night.Raw.1993.01.11.1080p.WEBRip.x264-GRP.mkv
    parser_test.go:255: season/episode = 0/0, want 1/1
=== RUN   TestParseReleaseDatedDailyShow/Some.Other.Daily.Show.2026.11.23.1080p.WEB-DL-GRP.mkv
--- FAIL: TestParseReleaseDatedDailyShow (0.00s)
    [six WWE subtests FAIL; non-WWE control PASS]
FAIL	github.com/leleasley/rex-alt/internal/tmdb	0.557s
EXIT=1
```

```
$ docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'gofmt -w internal/tmdb/ && go test -v -run TestParseReleaseDatedDailyShow ./internal/tmdb/'
=== RUN   TestParseReleaseDatedDailyShow
=== RUN   TestParseReleaseDatedDailyShow/WWE.Monday.Night.RAW.2026.11.23.1080p.WEBRip.h265-Star.mp4
=== RUN   TestParseReleaseDatedDailyShow/WWE.Friday.Night.SmackDown.2026.11.20.1080p.WEBRip.AAC2.0.HFR.H.265-SkY.mkv
=== RUN   TestParseReleaseDatedDailyShow/WWE.RAW.2026.11.16.1080p.WEBRip.x264-DEF.mkv
=== RUN   TestParseReleaseDatedDailyShow/WWE.RAW.2026-11-16.1080p.WEBRip.x264-DEF.mkv
=== RUN   TestParseReleaseDatedDailyShow/WWE.NXT.2026_11_18.1080p.WEB-DL.x264-GRP.mkv
=== RUN   TestParseReleaseDatedDailyShow/WWE.Monday.Night.Raw.1993.01.11.1080p.WEBRip.x264-GRP.mkv
=== RUN   TestParseReleaseDatedDailyShow/Some.Other.Daily.Show.2026.11.23.1080p.WEB-DL-GRP.mkv
--- PASS: TestParseReleaseDatedDailyShow (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/WWE.Monday.Night.RAW.2026.11.23.1080p.WEBRip.h265-Star.mp4 (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/WWE.Friday.Night.SmackDown.2026.11.20.1080p.WEBRip.AAC2.0.HFR.H.265-SkY.mkv (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/WWE.RAW.2026.11.16.1080p.WEBRip.x264-DEF.mkv (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/WWE.RAW.2026-11-16.1080p.WEBRip.x264-DEF.mkv (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/WWE.NXT.2026_11_18.1080p.WEB-DL.x264-GRP.mkv (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/WWE.Monday.Night.Raw.1993.01.11.1080p.WEBRip.x264-GRP.mkv (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/Some.Other.Daily.Show.2026.11.23.1080p.WEB-DL-GRP.mkv (0.00s)
PASS
ok  	github.com/leleasley/rex-alt/internal/tmdb	0.005s
EXIT=0
```

## Full verification

```
$ docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'go mod tidy && go test ./... && go vet ./...'
go: downloading modernc.org/sqlite v1.61.1
go: downloading golang.org/x/sys v0.49.0
go: downloading modernc.org/libc v1.79.0
go: downloading github.com/google/pprof v0.0.0-20261016150608-1facf8f91b50
go: downloading modernc.org/fileutil v1.4.0
go: downloading modernc.org/mathutil v1.7.1
go: downloading github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec
go: downloading github.com/google/uuid v1.6.0
go: downloading github.com/mattn/go-isatty v0.0.24
go: downloading github.com/dustin/go-humanize v1.1.0
go: downloading github.com/ncruces/go-strftime v1.1.0
go: downloading modernc.org/memory v1.12.1
go: downloading golang.org/x/tools v0.52.0
go: downloading modernc.org/cc/v4 v4.30.3
go: downloading modernc.org/ccgo/v4 v4.37.0
go: downloading modernc.org/goabi0 v0.2.0
go: downloading modernc.org/opt v0.2.0
go: downloading modernc.org/sortutil v1.2.1
go: downloading modernc.org/strutil v1.2.1
go: downloading modernc.org/token v1.1.0
go: downloading golang.org/x/mod v0.42.0
go: downloading modernc.org/gc/v3 v3.2.0
go: downloading modernc.org/gc/v2 v2.6.5
go: downloading github.com/hashicorp/golang-lru/v2 v2.0.7
go: downloading golang.org/x/sync v0.24.0
ok  	github.com/leleasley/rex-alt/cmd/rex-alt	0.015s
ok  	github.com/leleasley/rex-alt/internal/altmount	0.040s
ok  	github.com/leleasley/rex-alt/internal/api	1.874s
ok  	github.com/leleasley/rex-alt/internal/config	1.926s
ok  	github.com/leleasley/rex-alt/internal/library	1.732s
?   	github.com/leleasley/rex-alt/internal/models	[no test files]
ok  	github.com/leleasley/rex-alt/internal/probe	10.107s
ok  	github.com/leleasley/rex-alt/internal/rex	0.663s
ok  	github.com/leleasley/rex-alt/internal/store	0.680s
ok  	github.com/leleasley/rex-alt/internal/sync	1.166s
ok  	github.com/leleasley/rex-alt/internal/tmdb	3.064s
EXIT=0
```

`go vet ./...` produced no output (pass). `gofmt -l internal/tmdb/` produced no
output. `go mod tidy` left `go.mod`/`go.sum` unchanged. The pinned mismatch
guard (`hitMatches`, ±1 year and exact normalised title) and 429 backoff in
`client.go` were not modified and remain covered by their existing tests.

## Concerns

1. The week-based formula assumes one brand episode per ISO week. Preemptions,
   double tapings or holiday gaps can make the derived episode number differ
   from TMDB's; the strict title guard does not validate the episode itself.
2. Only the seven documented alias strings are recognised. A release named
   differently (for example `Raw.Dated.2026.11.23`) keeps 0/0.
3. Season/episode are derived from the calendar year in the filename; an air
   date near New Year belongs to the ISO week-year, which can differ from the
   calendar year. The season uses the calendar year as specified.
4. Existing TMDB cache rows keyed with the old 0/0 values remain until the next
   sync overwrites them; no cache migration was performed.
