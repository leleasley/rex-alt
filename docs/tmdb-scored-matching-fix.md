# TMDB scored title matching fix

## Task and context

Task 7 owner, architectural fix for rex-alt TMDB enrichment in `/opt/rex-alt`.
The partner wants movie names corrected. Three earlier token-strip patches
(`d4ee69d`, `3a0814b`, plus the other parser iterations) tried to clean the
parsed release title further; they did not fix the general case. This change
fixes it at the matching layer instead of chasing individual junk tokens.

Approved baseline: `d1158ef` (`release: pin v0.1.5 (TV title parser fix)`),
clean working tree.

## Root cause

`Enrich` accepted a search hit only when the normalised parsed title equalled
the normalised TMDB title exactly (plus year ±1), and only looked at
`results[0]`. Real release names keep residual junk after parsing, so most
movies got no `ids.tmdb` and Plex displayed the raw filename.

Observed parser output vs TMDB title (from the task evidence):

| Parsed release title | TMDB title | Old result |
| --- | --- | --- |
| `101 Dalmatians 2 Patchs London Adventure AVC` | `101 Dalmatians II: Patch's London Adventure` | rejected |
| `A Christmas Story Christmas X 265-EVO` | `A Christmas Story Christmas` | rejected |
| `A Wounded Fawn AVC -UnKn0wn` | `A Wounded Fawn` | rejected |
| `Bone Tomahawk` | `Bone Tomahawk` | accepted (control) |

## Change (`internal/tmdb/client.go`)

- Added `maxSearchResults = 5` and conservative `minTitleScore = 0.6`.
- Replaced `hitMatches` (exact string equality) with:
  - `titleScore(want, got)`: Dice coefficient over word token **multisets**,
    `2 * shared / (len(a) + len(b))`, in `[0,1]`. Score 1.0 for an exact
    normalised match; near-zero for unrelated films. Multiset counts matter:
    for "A Christmas Story Christmas" a naive Jaccard on sets scores only
    0.5, while Dice scores 0.727.
  - `titleTokens`: scoring-only normalisation reusing `normalizeTitle`, with
    apostrophes deleted first so TMDB's `Patch's` and the filename's `Patchs`
    tokenise identically. Without this, the Dalmatians case scores
    `8/14 = 0.571` and would be rejected despite the 0.6 threshold; with it,
    the score is `10/13 = 0.769`.
  - `yearAllowed`: the pinned ±1 year guard. A requested year with a missing
    hit year is ambiguous and rejected; no requested year means no gate.
  - `bestHitIndex[T]`: examines only the first five results, skips
    year-ineligible hits **before** scoring, selects the highest score, and
    returns -1 unless the best score is `>= 0.6`. Ties keep the higher-ranked
    result, except that an exact normalised match wins a tie explicitly.
    Year-ineligible hits are filtered rather than merely penalised so an
    exact-title wrong-year decoy can never displace the correct hit.
- Both branches (`/3/search/movie` and `/3/search/tv`) now use `bestHitIndex`
  instead of hard-coding `results[0]`.
- Unchanged by design: v4 bearer/v3 key auth, endpoints and query params,
  `cacheKey`/`normalizeTitle`, 429 retry/backoff, image base URL and sizes,
  and the `models.Enriched` shape.

Scores of the three regression cases (all clear 0.6):

- Dalmatians: `10/13 = 0.7692` (rank 2)
- Christmas: `8/11 = 0.7273` (rank 3, outranks a matching-year "…Christmas 2" decoy at 0.667)
- Wounded Fawn: `6/8 = 0.75` (rank 3, outranks an exact-title wrong-year decoy)

## Test coverage

New fixtures in `internal/tmdb/testdata/`:

- `movie_search_dalmatians.json` — correct hit at rank 2, wrong-year
  lookalikes and an unrelated film.
- `movie_search_christmas.json` — correct hit at rank 3, rank-2 near decoy
  with a matching year, plus a wrong-year and an unrelated decoy.
- `movie_search_wounded_fawn.json` — exact-title 2019 decoy at rank 1,
  low-score 2022 decoy, correct 2022 hit at rank 3, unrelated control.
- `movie_search_unrelated_top.json` — unrelated top hit plus a below-threshold
  follower ("Swallowed By The Earth", score 0.4).

New tests in `internal/tmdb/client_test.go`:

- `TestEnrichResolvesNoisyMovieTitles` — the three real-world noisy titles
  resolve to ids 9102 / 9203 / 9303, with the **corrected TMDB title** and
  date in `Enriched`; asserts details are fetched only for the winning id
  (a decoy fetch would 404 and fail the test).
- `TestEnrichRejectsUnrelatedTopHit` — pins the wrong-id guard: ok=false,
  zero `models.Enriched`, no details request.
- `TestTitleScoreNoisyTitlesClearThreshold` — exact = 1.0, the three noisy
  pairs in their expected score bands, unrelated = 0.0, near miss ≈ 0.4.
- `TestBestHitIndexGuardsAndTieBreaks` — rank 5 examined, rank 6 ignored,
  exact match wins a score tie, wrong-year lookalike skipped, all-wrong-year
  rejected, and no-year request accepts a missing date.

Existing tests kept passing unchanged, including `TestEnrichRetries429`,
`TestEnrichStopsAfterThreeRetries`, `TestEnrichRejectsMismatchedYear` (both
subtests), `TestEnrichRejectsMismatchedTitle`, cache/auth/show-episode tests,
and all parser tests.

## TDD evidence

### RED — tests first, implementation unchanged

Command:

```
cd /opt/rex-alt && docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'go test ./internal/tmdb -run "TestEnrich(ResolvesNoisyMovieTitles|RejectsUnrelatedTopHit)" -v'
```

Output (abridged downloads):

```
=== RUN   TestEnrichResolvesNoisyMovieTitles
=== RUN   TestEnrichResolvesNoisyMovieTitles/roman_numeral_and_stray_AVC
    client_test.go:406: Enrich returned ok=false for "101 Dalmatians 2 Patchs London Adventure AVC"; want TMDB id 9102
=== RUN   TestEnrichResolvesNoisyMovieTitles/codec_tail_after_repeated_title_word
    client_test.go:406: Enrich returned ok=false for "A Christmas Story Christmas X 265-EVO"; want TMDB id 9203
=== RUN   TestEnrichResolvesNoisyMovieTitles/upload_group_suffix
    client_test.go:406: Enrich returned ok=false for "A Wounded Fawn AVC -UnKn0wn"; want TMDB id 9303
--- FAIL: TestEnrichResolvesNoisyMovieTitles (0.04s)
--- PASS: TestEnrichRejectsUnrelatedTopHit (0.01s)
FAIL
FAIL	github.com/leleasley/rex-alt/internal/tmdb	0.061s
```

The guard test already passed on the old code, confirming no wrong-id
regression; the three real-world resolutions failed as expected.

### GREEN — after implementing scored matching

Command:

```
cd /opt/rex-alt && docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'gofmt -l internal/tmdb && go test ./internal/tmdb -v'
```

Output (relevant lines):

```
internal/tmdb/parser.go        # pre-existing gofmt finding; file untouched by this task
=== RUN   TestEnrichResolvesNoisyMovieTitles
--- PASS: TestEnrichResolvesNoisyMovieTitles (0.07s)
    --- PASS: .../roman_numeral_and_stray_AVC (0.03s)
    --- PASS: .../codec_tail_after_repeated_title_word (0.02s)
    --- PASS: .../upload_group_suffix (0.01s)
=== RUN   TestEnrichRejectsUnrelatedTopHit
--- PASS: TestEnrichRejectsUnrelatedTopHit (0.01s)
=== RUN   TestTitleScoreNoisyTitlesClearThreshold
--- PASS: TestTitleScoreNoisyTitlesClearThreshold (0.00s)
=== RUN   TestBestHitIndexGuardsAndTieBreaks
--- PASS: TestBestHitIndexGuardsAndTieBreaks (0.00s)
--- PASS: TestEnrichRetries429 (0.01s)
--- PASS: TestEnrichStopsAfterThreeRetries (0.02s)
--- PASS: TestEnrichRejectsMismatchedYear (0.03s)
--- PASS: TestEnrichRejectsMismatchedTitle (0.02s)
--- PASS: TestEnrichShowEpisode (0.02s)
PASS
ok  	github.com/leleasley/rex-alt/internal/tmdb	0.280s
```

### Full repository verification (prescribed command)

Command:

```
cd /opt/rex-alt && docker run --rm -v /opt/rex-alt:/src -w /src golang:1.27-alpine sh -c 'go mod tidy && go test ./... && go vet ./...'
```

Output:

```
ok  	github.com/leleasley/rex-alt/cmd/rex-alt	0.018s
ok  	github.com/leleasley/rex-alt/internal/altmount	0.032s
ok  	github.com/leleasley/rex-alt/internal/api	0.064s
ok  	github.com/leleasley/rex-alt/internal/config	0.025s
ok  	github.com/leleasley/rex-alt/internal/library	0.011s
?   	github.com/leleasley/rex-alt/internal/models	[no test files]
ok  	github.com/leleasley/rex-alt/internal/probe	8.951s
ok  	github.com/leleasley/rex-alt/internal/rex	0.393s
ok  	github.com/leleasley/rex-alt/internal/store	0.202s
ok  	github.com/leleasley/rex-alt/internal/sync	0.597s
ok  	github.com/leleasley/rex-alt/internal/tmdb	0.442s
```

`go mod tidy` left `go.mod`/`go.sum` unchanged; `go vet ./...` produced no
output (clean). No push, tag, rebuild or publish was performed.

## Concerns / limitations

- The 0.6 threshold is heuristic, tuned so genuine noisy titles score ~0.7
  while unrelated films score near zero. A title reduced to very few tokens
  (e.g. a one-word film with heavy junk) could still fall below it; that
  fails safe (filename shown) rather than attaching a wrong id.
- Dice on token multisets is order-insensitive: a deliberate reordering of
  the same words would score 1.0. An exact normalised match still wins ties
  explicitly, and the year gate plus threshold keep the wrong-id risk low.
- Only the first five results are examined by design; a correct hit beyond
  rank 5 is ignored (pinned by a unit test).
- Verification used httptest fixtures only; no live TMDB request was made in
  this task.
