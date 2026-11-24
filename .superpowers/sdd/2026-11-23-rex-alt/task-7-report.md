# Task 7 — Release-name parser fix report

Date: 2026-11-24
Area: `internal/tmdb` (release-name → TMDB metadata)
Status: fixed, verified with TDD (RED → GREEN), committed locally (no push/tag/build/publish).

## Root cause

`ParseRelease` treated an `SxxExx` marker as an ordinary token to be replaced
with a space, so for TV releases it kept everything after the marker (the
episode-specific subtitle) inside the show title. It also kept a bare `Sxx`
season-only token. TMDB's strict guard `hitMatches` (internal/tmdb/client.go)
requires `normalizeTitle(hitTitle) == normalizeTitle(wantTitle)`, so the
polluted title rejected the correct show hit: no TMDB ids/artwork and Plex
displayed only "Episode 1".

Live evidence before the fix:

- `ParseRelease("12.12.12.S01E01.Im.Robbing.the.House.on.Christmas.Eve.While.the.Familys.at.Mass.1080p.ATVP.WEB-DL.DDP5.1.Atmos.H.264-RAWR.mkv", "tv")`
  → title `"12 12 12 Im Robbing the House on Christmas Eve While the Familys at Mass H 264-RAWR"`, year 0, S01E01. Correct title: `"12 12 12"`.
- `ParseRelease("The.Simpsons.S01.1080p.Web-DL.x264-OFT.mkv", "tv")`
  → `"The Simpsons S01"`; correct title: `"The Simpsons"`.

## Fix (at the source, in `internal/tmdb/parser.go`)

1. `seasonEpisodeRE` now accepts a 1–3 digit season: `(?i)\bs(\d{1,3})e(\d{1,4})\b`.
2. For TV categories, the title is **truncated at the first `SxxExx` match**
   (`base = base[:loc[0]]`) and season/episode are extracted from that match.
   Everything after it (episode subtitle, quality, codec, group) is discarded
   before year extraction/codec stripping, instead of leaking into the title.
3. For TV categories with no `SxxExx` match, a bare `Sxx` token is removed via
   the new `seasonOnlyRE = (?i)\bs\d{1,3}\b` (season-only release). Word
   boundaries prevent partial matches inside longer tokens; `S01E02` cannot
   match the bare pattern because `E` is a word character.
4. Movie categories are unchanged: an `SxxExx` marker is still removed (not
   reported) so it cannot leak into a movie title; bare `Sxx` is not stripped
   for movies. Audio-codec stripping (`stripAudioCodecs`), junk-token filtering
   and the "last four-digit year wins" rule are untouched.
5. The pinned guard is untouched: `hitMatches` still rejects a hit whose
   normalised title differs or whose year is off by more than one; a wrong TMDB
   id is never attached. The fix only makes the requested title correct.

## TDD evidence

### RED — regression tests added first, parser unchanged

Command:

```
docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'go test ./internal/tmdb/ -run "TestParseRelease" -v'
```

Relevant output (excerpt):

```
=== RUN   TestParseReleaseTVEpisodeTitleStopsAtMarker
--- FAIL: TestParseReleaseTVEpisodeTitleStopsAtMarker (0.00s)
    --- FAIL: .../12.12.12.S01E01.Im.Robbing.the.House.on.Christmas.Eve.While.the.Familys.at.Mass.1080p.ATVP.WEB-DL.DDP5.1.Atmos.H.264-RAWR.mkv
        parser_test.go:163: title = "12 12 12 Im Robbing the House on Christmas Eve While the Familys at Mass H 264-RAWR", want "12 12 12"
    --- FAIL: .../Something.Very.Bad.is.Going.to.Happen.S123E04.2160p.WEB.h265-ETHEL
        parser_test.go:163: title = "Something Very Bad is Going to Happen S123E04", want "Something Very Bad is Going to Happen"
        parser_test.go:169: season/episode = 0/0, want 123/4
=== RUN   TestParseReleaseTVSeasonOnlyDropsBareSeason
--- FAIL: TestParseReleaseTVSeasonOnlyDropsBareSeason (0.00s)
    parser_test.go:180: title = "The Simpsons S01", want "The Simpsons"
=== RUN   TestParseReleaseMovieControlUnaffected
--- PASS: TestParseReleaseMovieControlUnaffected (0.00s)
FAIL    github.com/leleasley/rex-alt/internal/tmdb  0.007s
FAIL
```

All pre-existing tests (including `TestParseReleaseStripsAudioCodecs`,
`TestParseReleaseEpisode`, `TestParseReleaseDoesNotReadSeasonFromMovieName`)
passed in RED; only the new root-cause assertions failed.

### GREEN — after the parser change

Command (full verification, as specified):

```
docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'go mod tidy && go test ./... && go vet ./...'
```

Output:

```
ok  	github.com/leleasley/rex-alt/cmd/rex-alt	0.011s
ok  	github.com/leleasley/rex-alt/internal/altmount	0.022s
ok  	github.com/leleasley/rex-alt/internal/api	0.169s
ok  	github.com/leleasley/rex-alt/internal/config	0.018s
ok  	github.com/leleasley/rex-alt/internal/library	0.010s
?   	github.com/leleasley/rex-alt/internal/models	[no test files]
ok  	github.com/leleasley/rex-alt/internal/probe	8.756s
ok  	github.com/leleasley/rex-alt/internal/rex	0.276s
ok  	github.com/leleasley/rex-alt/internal/store	0.645s
ok  	github.com/leleasley/rex-alt/internal/sync	1.171s
ok  	github.com/leleasley/rex-alt/internal/tmdb	0.730s
```

`go mod tidy` produced no dependency changes (go.mod/go.sum untouched);
`go vet ./...` produced no output and exited 0. `git diff --check` clean.

## Covering tests (in `internal/tmdb/parser_test.go`)

- `TestParseReleaseTVEpisodeTitleStopsAtMarker`
  - the live-discovered `12.12.12` S01E01 release → title `"12 12 12"`, year 0, S01E01.
  - `S123E04` case → proves the 1–3 digit season regex and truncation.
- `TestParseReleaseTVSeasonOnlyDropsBareSeason`
  - `The.Simpsons.S01.1080p.Web-DL.x264-OFT.mkv` (tv) → `"The Simpsons"`.
- `TestParseReleaseMovieControlUnaffected`
  - `10.Cloverfield.Lane.2016.720p.BluRay.DDP5.1.x264-HiDt` (movies) → `10 Cloverfield Lane`, 2016 (leading digit + audio codec fix preserved).
  - `12.12.12.2022.720p.WEB-DL.x264-GROUP.mkv` (movies) → `12 12 12`, 2022 (leading digits preserved).
- Existing coverage retained: `TestParseReleaseMovie`, `TestParseReleaseStripsAudioCodecs`, `TestParseReleaseEpisode`, `TestParseReleaseDoesNotReadSeasonFromMovieName`, plus client-guard tests `TestHitMatches*` / mismatched-title tests in `internal/tmdb/client_test.go`.

## Notes / concerns

- A bare `Sxx` season-only token is stripped from the title but does not set a
  season value (no episode number is present, so no episode lookup is made);
  this matches the requested scope.
- During RED, a first-draft movie control used `...DDP5.1.Atmos.H.264-GROUP.mkv`
  and exposed a separate, pre-existing gap: the `H 264-GROUP` fragment survives
  the codec filter when no hyphenated codec head is present. Out of scope for
  this fix; the control was changed to the known-good hyphenated `x264-HiDt`
  form and is documented here for follow-up.
- No push, tag, rebuild, publish, or container image changes were performed.

## Files changed

- `internal/tmdb/parser.go` — source fix.
- `internal/tmdb/parser_test.go` — regression tests.
- `.superpowers/sdd/2026-11-23-rex-alt/task-7-report.md` — this report (recreated
  directory because the SDD workspace was deleted after final review).
