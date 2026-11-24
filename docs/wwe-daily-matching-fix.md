# WWE daily release-name parsing and TMDB matching fix

## Task and context

Task 7 owner, follow-up to the scored TMDB matching fix
(`bad2953`, `release: pin v0.1.6`). The partner reports that WWE RAW and
SmackDown items in the mount are not detected or matched and instead get
associated with unrelated "daily" shows.

Approved baseline: `bad2953`, clean working tree. No push, tag, rebuild or
publish was performed.

## Root cause (partner evidence, live TMDB search)

`ParseRelease(..., "tv")` on a dated daily release treated the episode air
date as the show's first-air year:

| Release name | Parsed title before fix | Parsed year |
| --- | --- | --- |
| `WWE.Monday.Night.RAW.2026.11.23.1080p.WEBRip.h265-Star.mp4` | `WWE Monday Night RAW 11 23` | 2026 |
| `WWE.Friday.Night.SmackDown.2026.11.20.1080p...` | `WWE Friday Night SmackDown 11 20 AAC2 0 HFR H 265-SkY` | 2026 |
| `WWE.RAW.2026.11.16...` | `WWE RAW 11 16 DEF` | 2026 |

Live `/3/search/tv` results:

- `WWE Monday Night Raw` → `Raw` (first_air_date `1993-01-11`).
- `WWE Friday Night SmackDown` → exact match.
- `WWE Raw` → `WWE Raw Classics`, `WWE Raw Top 10`, `Raw`, `Raw Talk`.

Consequences: the ±1 year gate rejected the real 1993 show because the parsed
"year" was the 2026 episode date, and the leftover date/codec tokens lowered
the Dice score so decoys such as `Raw Classics` / `Raw Talk` could win.

## Change (TDD, tests first)

### Tests added before implementation (RED)

`internal/tmdb/parser_test.go`:

- `TestParseReleaseDatedDailyShow` — the three reported WWE names plus dash
  and space date variants must parse to clean show titles with year 0 and
  season/episode 0/0.
- `TestParseReleaseDatedEpisodeKeepsMarkerBehaviour` — a dated release with an
  `SxxExx` marker keeps the existing marker truncation and season/episode.

`internal/tmdb/client_test.go` (httptest fixtures):

- `TestEnrichResolvesDailyWrestlingShows` — RAW / SmackDown / NXT resolve to
  ids 8104 / 8203 / 8302 with decoys (`Raw Classics`, `Raw Talk`, `WWE Raw Top
  10`, `WWE SmackDown Live`, `SmackDown Talk`, `NXT UK`) ranked before the real
  hit; asserts the alias query, absent `first_air_date_year`, and no decoy
  endpoint fetch.
- `TestEnrichRejectsUnrelatedTVHitWithNoYear` — with year 0 the gate is open;
  a low-scoring lookalike and unrelated shows must still return ok=false and
  no id.

New fixtures in `internal/tmdb/testdata/`: `tv_search_raw.json`,
`tv_search_smackdown.json`, `tv_search_nxt.json`, `tv_search_unrelated.json`.

### RED output (exact, downloads abridged only where noted)

Command:

```
cd /opt/rex-alt && docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'go test ./internal/tmdb -run "TestParseReleaseDated|TestEnrichResolvesDailyWrestlingShows|TestEnrichRejectsUnrelatedTVHitWithNoYear" -v'
```

Output:

```
go: downloading modernc.org/sqlite v1.61.1
go: downloading modernc.org/libc v1.79.0
go: downloading golang.org/x/sys v0.49.0
go: downloading github.com/dustin/go-humanize v1.1.0
go: downloading modernc.org/mathutil v1.7.1
go: downloading github.com/google/uuid v1.6.0
go: downloading modernc.org/memory v1.12.1
go: downloading github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec
=== RUN   TestEnrichResolvesDailyWrestlingShows
=== RUN   TestEnrichResolvesDailyWrestlingShows/monday_night_raw
    client_test.go:729: Enrich returned ok=false for "WWE Monday Night RAW"; want TMDB id 8104
=== RUN   TestEnrichResolvesDailyWrestlingShows/friday_night_smackdown
=== RUN   TestEnrichResolvesDailyWrestlingShows/nxt
    client_test.go:741: query = "WWE NXT", want alias "NXT"
--- FAIL: TestEnrichResolvesDailyWrestlingShows (0.14s)
    --- FAIL: TestEnrichResolvesDailyWrestlingShows/monday_night_raw (0.05s)
    --- PASS: TestEnrichResolvesDailyWrestlingShows/friday_night_smackdown (0.05s)
    --- FAIL: TestEnrichResolvesDailyWrestlingShows/nxt (0.05s)
=== RUN   TestEnrichRejectsUnrelatedTVHitWithNoYear
--- PASS: TestEnrichRejectsUnrelatedTVHitWithNoYear (0.05s)
=== RUN   TestParseReleaseDatedDailyShow
=== RUN   TestParseReleaseDatedDailyShow/WWE.Monday.Night.RAW.2026.11.23.1080p.WEBRip.h265-Star.mp4
    parser_test.go:221: title = "WWE Monday Night RAW 11 23", want "WWE Monday Night RAW"
    parser_test.go:224: year = 2026, want 0 (air date is not a first-air year)
=== RUN   TestParseReleaseDatedDailyShow/WWE.Friday.Night.SmackDown.2026.11.20.1080p.WEBRip.AAC2.0.HFR.H.265-SkY.mkv
    parser_test.go:221: title = "WWE Friday Night SmackDown 11 20 AAC2 0 HFR H 265-SkY", want "WWE Friday Night SmackDown"
    parser_test.go:224: year = 2026, want 0 (air date is not a first-air year)
=== RUN   TestParseReleaseDatedDailyShow/WWE.RAW.2026.11.16.1080p.WEBRip.x264-DEF.mkv
    parser_test.go:221: title = "WWE RAW 11 16", want "WWE RAW"
    parser_test.go:224: year = 2026, want 0 (air date is not a first-air year)
=== RUN   TestParseReleaseDatedDailyShow/WWE.RAW.2026-11-16.1080p.WEBRip.x264-DEF.mkv
    parser_test.go:221: title = "WWE RAW -11-16", want "WWE RAW"
    parser_test.go:224: year = 2026, want 0 (air date is not a first-air year)
=== RUN   TestParseReleaseDatedDailyShow/WWE.NXT.2026_11_18.1080p.WEB-DL.x264-GRP.mkv
    parser_test.go:221: title = "WWE NXT 11 18", want "WWE NXT"
    parser_test.go:224: year = 2026, want 0 (air date is not a first-air year)
--- FAIL: TestParseReleaseDatedDailyShow (0.00s)
    --- FAIL: TestParseReleaseDatedDailyShow/WWE.Monday.Night.RAW.2026.11.23.1080p.WEBRip.h265-Star.mp4 (0.00s)
    --- FAIL: TestParseReleaseDatedDailyShow/WWE.Friday.Night.SmackDown.2026.11.20.1080p.WEBRip.AAC2.0.HFR.H.265-SkY.mkv (0.00s)
    --- FAIL: TestParseReleaseDatedDailyShow/WWE.RAW.2026.11.16.1080p.WEBRip.x264-DEF.mkv (0.00s)
    --- FAIL: TestParseReleaseDatedDailyShow/WWE.RAW.2026-11-16.1080p.WEBRip.x264-DEF.mkv (0.00s)
    --- FAIL: TestParseReleaseDatedDailyShow/WWE.NXT.2026_11_18.1080p.WEB-DL.x264-GRP.mkv (0.00s)
=== RUN   TestParseReleaseDatedEpisodeKeepsMarkerBehaviour
--- PASS: TestParseReleaseDatedEpisodeKeepsMarkerBehaviour (0.00s)
FAIL
FAIL	github.com/leleasley/rex-alt/internal/tmdb	0.209s
FAIL
```

Exit code 1. The unrelated-hit guard already passed on the old code, which
confirms the new negative test does not depend on this fix. SmackDown also
already passed because its parsed title happened to equal TMDB's name exactly;
RAW and NXT failed as expected.

### Implementation

`internal/tmdb/parser.go`:

- Added `tvAirDateRE`, a date-triplet regex
  `\b(?:19|20)\d{2}[ .-](?:0[1-9]|1[0-2])[ .-](?:0[1-9]|[12]\d|3[01])\b`.
  Dots are already spaces, so `YYYY.MM.DD`, `YYYY-MM-DD` and spaced variants
  all match; bounded month/day ranges avoid treating arbitrary numbers as a
  date.
- In the TV branch, when no `SxxExx` marker exists (after any bare-season
  removal), the first air date ends the show name: everything from the date
  onward — including resolution/codec/upload-group tags — is truncated. This
  is the same rule already used for episode subtitles after `SxxExx`, and it
  removes the leftover `11 23`, `AAC2 0 HFR H 265-SkY`, `DEF`, etc.
- `year` extraction is skipped for such dated shows, so year stays 0 and the
  ±1 gate cannot reject the real 1993 series. Episode markers are unaffected.

`internal/tmdb/client.go`:

- Added the documented, seven-entry `tvSearchAliases` map plus
  `tvSearchTitle`, applied only in the `isShow` branch just before the TMDB
  query is built. Both the query and the scoring title use the alias, so an
  exact `Raw` hit scores 1.0 instead of the 0.4 that `WWE Monday Night RAW`
  would score against it.
- The cache key is computed before the rewrite and is unchanged, as are auth,
  the 429 backoff, image URLs, `models.Enriched`, `yearAllowed`,
  `bestHitIndex` and `minTitleScore = 0.6`.
- Both files were gofmt'd; this also normalised pre-existing alignment in the
  `parser.go` var block.

### GREEN output

Command:

```
cd /opt/rex-alt && docker run --rm -v /opt/rex-alt:/src -v rex-alt-gomod:/go/pkg/mod -w /src golang:1.27-alpine sh -c 'gofmt -w internal/tmdb/parser.go internal/tmdb/client_test.go && gofmt -l internal/tmdb && go test ./internal/tmdb -v'
```

`gofmt -l internal/tmdb` produced no output. Relevant test output:

```
=== RUN   TestEnrichResolvesDailyWrestlingShows
--- PASS: TestEnrichResolvesDailyWrestlingShows (0.12s)
    --- PASS: TestEnrichResolvesDailyWrestlingShows/monday_night_raw (0.04s)
    --- PASS: TestEnrichResolvesDailyWrestlingShows/friday_night_smackdown (0.04s)
    --- PASS: TestEnrichResolvesDailyWrestlingShows/nxt (0.04s)
=== RUN   TestEnrichRejectsUnrelatedTVHitWithNoYear
--- PASS: TestEnrichRejectsUnrelatedTVHitWithNoYear (0.04s)
=== RUN   TestParseReleaseDatedDailyShow
--- PASS: TestParseReleaseDatedDailyShow (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/WWE.Monday.Night.RAW.2026.11.23.1080p.WEBRip.h265-Star.mp4 (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/WWE.Friday.Night.SmackDown.2026.11.20.1080p.WEBRip.AAC2.0.HFR.H.265-SkY.mkv (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/WWE.RAW.2026.11.16.1080p.WEBRip.x264-DEF.mkv (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/WWE.RAW.2026-11-16.1080p.WEBRip.x264-DEF.mkv (0.00s)
    --- PASS: TestParseReleaseDatedDailyShow/WWE.NXT.2026_11_18.1080p.WEB-DL.x264-GRP.mkv (0.00s)
=== RUN   TestParseReleaseDatedEpisodeKeepsMarkerBehaviour
--- PASS: TestParseReleaseDatedEpisodeKeepsMarkerBehaviour (0.00s)
PASS
ok  	github.com/leleasley/rex-alt/internal/tmdb	0.840s
```

All pre-existing TMDB tests also passed in that run (auth, cache, 429, image
URLs, movie scoring, episode, parser controls).

### Full repository verification (prescribed command)

Command:

```
cd /opt/rex-alt && docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'go mod tidy && go test ./... && go vet ./...'
```

Output:

```
go: downloading modernc.org/sqlite v1.61.1
go: downloading modernc.org/libc v1.79.0
go: downloading golang.org/x/sys v0.49.0
go: downloading github.com/google/pprof v0.0.0-20261016150608-1facf8f91b50
go: downloading modernc.org/fileutil v1.4.0
go: downloading modernc.org/mathutil v1.7.1
go: downloading github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec
go: downloading github.com/dustin/go-humanize v1.1.0
go: downloading github.com/google/uuid v1.6.0
go: downloading github.com/mattn/go-isatty v0.0.24
go: downloading github.com/ncruces/go-strftime v1.1.0
go: downloading modernc.org/memory v1.12.1
go: downloading golang.org/x/tools v0.52.0
go: downloading modernc.org/cc/v4 v4.30.3
go: downloading modernc.org/ccgo/v4 v4.37.0
go: downloading modernc.org/goabi0 v0.2.0
go: downloading golang.org/x/mod v0.42.0
go: downloading golang.org/x/sync v0.24.0
go: downloading modernc.org/opt v0.2.0
go: downloading modernc.org/sortutil v1.2.1
go: downloading modernc.org/strutil v1.2.1
go: downloading modernc.org/token v1.1.0
go: downloading modernc.org/gc/v2 v2.6.5
go: downloading modernc.org/gc/v3 v3.2.0
go: downloading github.com/hashicorp/golang-lru/v2 v2.0.7
ok  	github.com/leleasley/rex-alt/cmd/rex-alt	0.010s
ok  	github.com/leleasley/rex-alt/internal/altmount	0.032s
ok  	github.com/leleasley/rex-alt/internal/api	0.173s
ok  	github.com/leleasley/rex-alt/internal/config	0.013s
ok  	github.com/leleasley/rex-alt/internal/library	0.005s
?   	github.com/leleasley/rex-alt/internal/models	[no test files]
ok  	github.com/leleasley/rex-alt/internal/probe	8.927s
ok  	github.com/leleasley/rex-alt/internal/rex	0.409s
ok  	github.com/leleasley/rex-alt/internal/store	0.670s
ok  	github.com/leleasley/rex-alt/internal/sync	1.530s
ok  	github.com/leleasley/rex-alt/internal/tmdb	1.356s
```

`go mod tidy` left `go.mod`/`go.sum` unchanged; `go vet ./...` produced no
output (clean). No push, tag, rebuild or publish was performed.

## Concerns / limitations

- The alias map is intentionally limited to the seven documented brand keys.
  Other daily shows still rely on the 0.6 Dice threshold and the year gate;
  if a brand's TMDB title changes, it needs an explicit new entry.
- Date truncation assumes the air date precedes the quality/codec tags, which
  is the standard release layout. A TV release with a `YYYY.MM.DD` triplet
  inside the actual show name (without `SxxExx`) would be shortened; no such
  case exists in the current fixtures.
- With year=0 the year gate is open by design, so a low-scoring shared-token
  hit such as `Raw Talk` could be accepted if the exact `Raw` were absent from
  the first five results. The fixtures deliberately rank the exact hit above
  the decoys; the 0.6 threshold and five-result cap remain unchanged.
- Aliases are applied before scoring but after the cache key is computed, so
  cached entries keep the original parsed title. An already-cached failed
  lookup is not retried with an alias, but the cache only stores successes.
- Verification used httptest fixtures only; no live TMDB request was made in
  this task.
