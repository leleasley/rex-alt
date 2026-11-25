package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/leleasley/rex-alt/internal/models"
	"github.com/leleasley/rex-alt/internal/store"
)

const (
	defaultBaseURL   = "https://api.themoviedb.org"
	defaultImageBase = "https://image.tmdb.org/t/p/"
	posterSize       = "w500"
	showPosterSize   = "w342" // poster_sizes in /3/configuration: w342, w500, w780, original
	backdropSize     = "w780"
	stillSize        = "w300" // still_sizes in /3/configuration: w92, w300, original
	maxRetries       = 3
	maxResponseBytes = 1 << 20
	defaultTimeout   = 15 * time.Second
	maxRetryWait     = 2 * time.Second
	// Only the first page's leading hits are considered: later results grow
	// rapidly less relevant, and scoring more results raises wrong-id risk.
	maxSearchResults = 5
	// Conservative Dice threshold. Residual release junk lowers a genuine
	// match to roughly 0.7; unrelated films score far below this.
	minTitleScore = 0.6
	// A title whose every token occurs in the parsed query is boosted by
	// base + span*(candidate tokens / query tokens), i.e. 0.6..0.95. Scaling
	// with coverage keeps a more complete candidate ahead of a shorter subset
	// while the cap stays below the 1.0 of an exact normalised match.
	containedTitleBase = 0.6
	containedTitleSpan = 0.35
	// A one-word title with a year in hand is accepted through containment at
	// this fixed score: below exact (1.0), above the Dice threshold. Without a
	// year the two-token containment rule still applies.
	singleTokenContained = 0.75
)

// scoringApostrophes is used only by the match scorer: TMDB writes "Patch's"
// while a parsed release name often reads "Patchs". The cache-key normaliser
// is deliberately left untouched.
var scoringApostrophes = strings.NewReplacer("'", "", "\u2019", "")

// tvSearchAliases is a deliberately small exception to the title score: daily
// wrestling brands are released with a "WWE" prefix and an episode date, but
// TMDB indexes some of the series under shorter or older names. Keys are
// normalised parsed show names; values are the exact TMDB search strings.
// Keep this list explicit — every title not listed here is unaffected and
// still judged by the 0.6 Dice threshold and the year gate.
var tvSearchAliases = map[string]string{
	"wwe monday night raw":       "Raw",
	"wwe raw":                    "Raw",
	"raw":                        "Raw",
	"wwe friday night smackdown": "WWE Friday Night SmackDown",
	"smackdown":                  "WWE Friday Night SmackDown",
	"wwe nxt":                    "NXT",
	"nxt":                        "NXT",
}

// movieSearchAliases rewrites a parsed movie release title to the exact TMDB
// search string when the release uses a regional title TMDB would not match.
// Keys are normalised parsed titles; values are the exact TMDB search strings.
// Document each entry with its TMDB id, verified live before encoding.
var movieSearchAliases = map[string]string{
	// "Kirpi Sonic 2" is the Turkish release title of the 2022 film
	// "Sonic the Hedgehog 2" (TMDB id 675353, Turkish title "Kirpi Sonic 2",
	// release 2022-03-30, verified live). The bare "kirpi sonic" key is
	// deliberately absent: it is the Turkish title of the 2020 film and must
	// never map to the 2022 sequel.
	"kirpi sonic 2": "Sonic the Hedgehog 2",
	// "Stake Land 2" is the release title of the 2016 film TMDB lists as
	// "The Stakelander" (TMDB id 410988, release 2016-10-14, verified live).
	// The alias rewrites the search so the canonical title scores 1.0.
	"stake land 2": "The Stakelander",
}

// movieSearchTitle applies the alias only on the movie search path. It runs
// after the cache key is computed, so cache entries keep the parsed title.
func movieSearchTitle(title string) string {
	if alias, ok := movieSearchAliases[normalizeTitle(title)]; ok {
		return alias
	}
	return title
}

// tvSearchTitle applies the alias only on the TV search path. It runs after
// the cache key is computed, so cache entries keep the parsed release title.
func tvSearchTitle(title string) string {
	if alias, ok := tvSearchAliases[normalizeTitle(title)]; ok {
		return alias
	}
	return title
}

// Client resolves release metadata against TMDB. The read token (v4 bearer) is
// preferred; a v3 API key is used only when no read token is configured. Both
// credentials are kept out of every error and never logged.
type Client struct {
	base      string
	readToken string
	apiKey    string
	cache     *store.Store
	http      *http.Client

	mu           sync.Mutex
	imageBaseURL string
}

// NewClient returns a TMDB client. A nil HTTP client gets a bounded default.
// The base URL is the public TMDB API; tests in this package override c.base.
func NewClient(readToken, apiKey string, cache *store.Store, h *http.Client) *Client {
	if h == nil {
		h = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		base:      defaultBaseURL,
		readToken: readToken,
		apiKey:    apiKey,
		cache:     cache,
		http:      h,
	}
}

// Enrich returns the best metadata for a release and reports whether it was
// accepted. The first maxSearchResults hits are scored by normalised token
// overlap, and the best hit is accepted only when it clears minTitleScore and
// (when a year was parsed) carries an eligible year for it. An ambiguous or
// mismatched hit yields ok=false and no id, so an unrelated TMDB id can never
// be attached to a file. On the TV path, a small documented alias map rewrites
// well-known daily/wrestling brands before search; all other titles are
// untouched. Successful results are cached in SQLite under the normalised
// title|year|isShow|season|episode key. When the first search returns no
// eligible hit, up to four retries use progressively shorter cleaned queries
// built from the parsed title; their hits are still scored against that
// original title. When the full ladder with a year still yields nothing, the
// same ladder repeats without the year param (TMDB's own year filter drops
// off-by-one festival-vs-wide-release years); scoring still uses the original
// year so the year gate (exact ±2, non-exact ±1) is unchanged.
func (c *Client) Enrich(ctx context.Context, title string, year, season, episode int, isShow bool) (models.Enriched, bool, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return models.Enriched{}, false, nil
	}
	parsedTitle := title
	key := cacheKey(title, year, isShow, season, episode)
	if c.cache != nil {
		if cached, found := c.cache.TMDB(key); found && cached.TMDBID != 0 {
			return cached, true, nil
		}
	}

	var en models.Enriched
	var posterPath, backdropPath, showPosterPath, showBackdropPath string
	if isShow {
		// Daily/wrestling brands use a documented alias for both the TMDB
		// query and the title score; other shows keep the parsed title.
		title = tvSearchTitle(title)
		type searchResult struct {
			ID           int    `json:"id"`
			Name         string `json:"name"`
			FirstAirDate string `json:"first_air_date"`
			Overview     string `json:"overview"`
			PosterPath   string `json:"poster_path"`
			BackdropPath string `json:"backdrop_path"`
		}
		var results struct {
			Results []searchResult `json:"results"`
		}
		params := url.Values{"query": {title}}
		if year > 0 {
			params.Set("first_air_date_year", strconv.Itoa(year))
		}
		if err := c.getJSON(ctx, "/3/search/tv", params, &results); err != nil {
			return models.Enriched{}, false, err
		}
		idx := -1
		if len(results.Results) > 0 {
			idx = bestHitIndex(results.Results,
				func(r searchResult) string { return r.Name },
				func(r searchResult) string { return r.FirstAirDate },
				title, year)
		}
		if idx < 0 {
			for _, fallback := range fallbackSearchTitles(parsedTitle) {
				if fallback == title {
					continue
				}
				params.Set("query", fallback)
				var retry struct {
					Results []searchResult `json:"results"`
				}
				if err := c.getJSON(ctx, "/3/search/tv", params, &retry); err != nil {
					return models.Enriched{}, false, err
				}
				if len(retry.Results) == 0 {
					continue
				}
				if candidate := bestHitIndex(retry.Results,
					func(r searchResult) string { return r.Name },
					func(r searchResult) string { return r.FirstAirDate },
					parsedTitle, year); candidate >= 0 {
					idx, results = candidate, retry
					break
				}
			}
		}
		if idx < 0 && year > 0 {
			// TMDB's own first_air_date_year filter excludes shows whose
			// filename year is off by one. Repeat the same ladder without
			// the year param, still scoring against the original title and
			// year so the unchanged year gate applies.
			params.Del("first_air_date_year")
			params.Set("query", title)
			var retry struct {
				Results []searchResult `json:"results"`
			}
			if err := c.getJSON(ctx, "/3/search/tv", params, &retry); err != nil {
				return models.Enriched{}, false, err
			}
			if len(retry.Results) > 0 {
				if candidate := bestHitIndex(retry.Results,
					func(r searchResult) string { return r.Name },
					func(r searchResult) string { return r.FirstAirDate },
					parsedTitle, year); candidate >= 0 {
					idx, results = candidate, retry
				}
			}
			if idx < 0 {
				for _, fallback := range fallbackSearchTitles(parsedTitle) {
					if fallback == title {
						continue
					}
					params.Set("query", fallback)
					var retry struct {
						Results []searchResult `json:"results"`
					}
					if err := c.getJSON(ctx, "/3/search/tv", params, &retry); err != nil {
						return models.Enriched{}, false, err
					}
					if len(retry.Results) == 0 {
						continue
					}
					if candidate := bestHitIndex(retry.Results,
						func(r searchResult) string { return r.Name },
						func(r searchResult) string { return r.FirstAirDate },
						parsedTitle, year); candidate >= 0 {
						idx, results = candidate, retry
						break
					}
				}
			}
		}
		if idx < 0 {
			return models.Enriched{}, false, nil
		}
		top := results.Results[idx]
		en.TMDBID = top.ID
		en.ShowTitle = top.Name
		en.Title = top.Name
		en.Summary = top.Overview
		en.Date = top.FirstAirDate
		showPosterPath, showBackdropPath = top.PosterPath, top.BackdropPath

		if season > 0 && episode > 0 {
			var ep struct {
				ID        int    `json:"id"`
				Name      string `json:"name"`
				Overview  string `json:"overview"`
				AirDate   string `json:"air_date"`
				StillPath string `json:"still_path"`
			}
			path := fmt.Sprintf("/3/tv/%d/season/%d/episode/%d", top.ID, season, episode)
			if err := c.getJSON(ctx, path, nil, &ep); err != nil {
				return models.Enriched{}, false, err
			}
			en.EpisodeTMDBID = ep.ID
			en.Title = ep.Name
			en.Summary = ep.Overview
			en.Date = ep.AirDate
			posterPath = ep.StillPath
		}
	} else {
		// Foreign/regional release titles use a documented alias for both the
		// TMDB query and the title score; other films keep the parsed title.
		title = movieSearchTitle(title)
		type searchResult struct {
			ID           int    `json:"id"`
			Title        string `json:"title"`
			ReleaseDate  string `json:"release_date"`
			Overview     string `json:"overview"`
			PosterPath   string `json:"poster_path"`
			BackdropPath string `json:"backdrop_path"`
		}
		var results struct {
			Results []searchResult `json:"results"`
		}
		params := url.Values{"query": {title}}
		if year > 0 {
			params.Set("primary_release_year", strconv.Itoa(year))
		}
		if err := c.getJSON(ctx, "/3/search/movie", params, &results); err != nil {
			return models.Enriched{}, false, err
		}
		idx := -1
		if len(results.Results) > 0 {
			idx = bestHitIndex(results.Results,
				func(r searchResult) string { return r.Title },
				func(r searchResult) string { return r.ReleaseDate },
				title, year)
		}
		if idx < 0 {
			for _, fallback := range fallbackSearchTitles(parsedTitle) {
				if fallback == title {
					continue
				}
				params.Set("query", fallback)
				var retry struct {
					Results []searchResult `json:"results"`
				}
				if err := c.getJSON(ctx, "/3/search/movie", params, &retry); err != nil {
					return models.Enriched{}, false, err
				}
				if len(retry.Results) == 0 {
					continue
				}
				if candidate := bestHitIndex(retry.Results,
					func(r searchResult) string { return r.Title },
					func(r searchResult) string { return r.ReleaseDate },
					parsedTitle, year); candidate >= 0 {
					idx, results = candidate, retry
					break
				}
			}
		}
		if idx < 0 && year > 0 {
			// TMDB's own primary_release_year filter excludes films whose
			// filename year is off by one (festival vs wide release). Repeat
			// the same ladder without the year param, still scoring against
			// the original title and year so the unchanged year gate applies.
			params.Del("primary_release_year")
			params.Set("query", title)
			var retry struct {
				Results []searchResult `json:"results"`
			}
			if err := c.getJSON(ctx, "/3/search/movie", params, &retry); err != nil {
				return models.Enriched{}, false, err
			}
			if len(retry.Results) > 0 {
				if candidate := bestHitIndex(retry.Results,
					func(r searchResult) string { return r.Title },
					func(r searchResult) string { return r.ReleaseDate },
					parsedTitle, year); candidate >= 0 {
					idx, results = candidate, retry
				}
			}
			if idx < 0 {
				for _, fallback := range fallbackSearchTitles(parsedTitle) {
					if fallback == title {
						continue
					}
					params.Set("query", fallback)
					var retry struct {
						Results []searchResult `json:"results"`
					}
					if err := c.getJSON(ctx, "/3/search/movie", params, &retry); err != nil {
						return models.Enriched{}, false, err
					}
					if len(retry.Results) == 0 {
						continue
					}
					if candidate := bestHitIndex(retry.Results,
						func(r searchResult) string { return r.Title },
						func(r searchResult) string { return r.ReleaseDate },
						parsedTitle, year); candidate >= 0 {
						idx, results = candidate, retry
						break
					}
				}
			}
		}
		if idx < 0 {
			return models.Enriched{}, false, nil
		}
		top := results.Results[idx]
		en.TMDBID = top.ID
		en.Title = top.Title
		en.Summary = top.Overview
		en.Date = top.ReleaseDate
		posterPath, backdropPath = top.PosterPath, top.BackdropPath

		var details struct {
			IMDbID       string `json:"imdb_id"`
			Title        string `json:"title"`
			Overview     string `json:"overview"`
			ReleaseDate  string `json:"release_date"`
			PosterPath   string `json:"poster_path"`
			BackdropPath string `json:"backdrop_path"`
		}
		path := fmt.Sprintf("/3/movie/%d", top.ID)
		if err := c.getJSON(ctx, path, nil, &details); err != nil {
			return models.Enriched{}, false, err
		}
		en.IMDbID = details.IMDbID
		if details.Title != "" {
			en.Title = details.Title
		}
		if details.Overview != "" {
			en.Summary = details.Overview
		}
		if details.ReleaseDate != "" {
			en.Date = details.ReleaseDate
		}
		if details.PosterPath != "" {
			posterPath = details.PosterPath
		}
		if details.BackdropPath != "" {
			backdropPath = details.BackdropPath
		}
	}

	base := c.imageBase(ctx)
	if isShow {
		en.Poster = imageURL(base, posterPath, stillSize)
		en.ShowPoster = imageURL(base, showPosterPath, showPosterSize)
		en.ShowBackdrop = imageURL(base, showBackdropPath, backdropSize)
	} else {
		en.Poster = imageURL(base, posterPath, posterSize)
		en.Backdrop = imageURL(base, backdropPath, backdropSize)
	}

	// A cache failure must not turn a successful lookup into a failed one.
	if c.cache != nil {
		_ = c.cache.SetTMDB(key, en)
	}
	return en, true, nil
}

// bestHitIndex picks the index of the best eligible hit among the first
// maxSearchResults results, or -1 when no result clears both guards. The year
// is a hard eligibility gate (exact ±2, non-exact ±1): when a year was parsed,
// a hit with a missing or out-of-range year is skipped before scoring, so a
// wrong-year lookalike can never displace the right film or trigger a
// wrong-id attach. Higher scores win; ties keep the higher-ranked result,
// except that an exact normalised-title match (score 1.0) wins a score tie,
// and among tied exact matches the nearer year wins. A candidate whose tokens are
// all contained in the parsed query gets a boost proportional to the share of
// query tokens it covers, and symmetrically a parsed query whose tokens are
// all contained in the candidate (a short name vs the full official title)
// gets the mirrored boost; both stay below 1.0 so an exact match still wins.
// These recover releases with trailing codec/group junk or abbreviated
// queries without letting a short subset outrank a more complete title.
func bestHitIndex[T any](results []T, titleOf, dateOf func(T) string, wantTitle string, wantYear int) int {
	limit := len(results)
	if limit > maxSearchResults {
		limit = maxSearchResults
	}
	exact := normalizeTitle(wantTitle)
	best, bestScore, bestExact := -1, -1.0, false
	bestDiff := 0
	for i := 0; i < limit; i++ {
		hit := results[i]
		if !yearAllowed(dateOf(hit), wantYear, wantTitle, titleOf(hit)) {
			continue
		}
		hitTitle := titleOf(hit)
		score := titleScore(wantTitle, hitTitle)
		if contained := containmentScore(wantTitle, hitTitle); contained > score {
			score = contained
		}
		if reverse := reverseContainmentScore(wantTitle, hitTitle); reverse > score {
			score = reverse
		}
		if single := singleTokenContainedScore(wantTitle, hitTitle, wantYear); single > score {
			score = single
		}
		isExact := normalizeTitle(hitTitle) == exact
		// An exact normalised-title match (score 1.0) wins a score tie. When
		// two exact matches tie — e.g. same-title films a few years apart
		// under the exact ±2 tolerance — the nearer year wins; otherwise the
		// higher-ranked result is kept.
		if score > bestScore || (score == bestScore && isExact && (!bestExact || nearerYear(dateOf(hit), wantYear, bestDiff))) {
			best, bestScore, bestExact = i, score, isExact
			bestDiff = absYearDiff(dateOf(hit), wantYear)
		}
	}
	if bestScore < minTitleScore {
		return -1
	}
	return best
}

// titleScore returns the Dice coefficient of two titles' word tokens (0..1):
// twice the shared token count divided by the total token count. Comparing
// token multisets instead of whole strings lets residual release junk ("AVC",
// "X 265-EVO", "-UnKn0wn") coexist with the real title, while unrelated films
// still score near zero and can never be accepted. An exact normalised match
// scores 1.0.
func titleScore(want, got string) float64 {
	a, b := titleTokens(want), titleTokens(got)
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	counts := make(map[string]int, len(a))
	for _, token := range a {
		counts[token]++
	}
	shared := 0
	for _, token := range b {
		if counts[token] > 0 {
			counts[token]--
			shared++
		}
	}
	return float64(2*shared) / float64(len(a)+len(b))
}

// containmentScore scores a TMDB title highly when every one of its tokens is
// also a token of the parsed query and it has at least two tokens. Parsed
// release names often end in codec/group junk ("Train Dreams H 265-XEBEC"),
// which dilutes the Dice overlap with the real title below minTitleScore;
// containment recognises the real title as a subset without a junk-token list.
// A candidate with any extra token is never contained, so extended or
// unrelated titles cannot be mistaken for the parsed one. The boost grows with
// the share of query tokens covered, so a complete title outranks a shorter
// subset on the same tokens; it stays below 1.0 so an exact match still wins.
func containmentScore(want, got string) float64 {
	wantTokens, gotTokens := titleTokens(want), titleTokens(got)
	if len(wantTokens) == 0 || len(gotTokens) < 2 {
		return 0
	}
	counts := make(map[string]int, len(wantTokens))
	for _, token := range wantTokens {
		counts[token]++
	}
	for _, token := range gotTokens {
		if counts[token] == 0 {
			return 0
		}
		counts[token]--
	}
	return containedTitleBase + containedTitleSpan*float64(len(gotTokens))/float64(len(wantTokens))
}

// reverseContainmentScore scores a TMDB title highly when every token of the
// parsed query is also a token of the candidate and the query has at least
// two tokens. A short parsed release name can be a strict subset of the
// official title ("Borat Subsequent Moviefilm" vs the full official title),
// which dilutes the Dice overlap below minTitleScore while forward
// containment is 0 because the candidate carries extra tokens. The boost
// mirrors containmentScore with the ratio inverted: base plus span times the
// share of candidate tokens the query covers, staying below the 1.0 of an
// exact normalised match so exact still wins. The two-token minimum keeps the
// no-year guard; single-token queries with a year are handled by
// singleTokenContainedScore instead.
func reverseContainmentScore(want, got string) float64 {
	wantTokens, gotTokens := titleTokens(want), titleTokens(got)
	if len(wantTokens) < 2 || len(gotTokens) == 0 {
		return 0
	}
	counts := make(map[string]int, len(gotTokens))
	for _, token := range gotTokens {
		counts[token]++
	}
	for _, token := range wantTokens {
		if counts[token] == 0 {
			return 0
		}
		counts[token]--
	}
	return containedTitleBase + containedTitleSpan*float64(len(wantTokens))/float64(len(gotTokens))
}

// singleTokenContainedScore admits a one-word TMDB title only when a year was
// parsed and that word occurs in the parsed query. A parsed release such as
// "20251028 160401 Relay" otherwise scores 0.5 by Dice against "Relay" and is
// rejected, although the year gate already proves the hit is the right film.
// With wantYear == 0 this returns 0, keeping the two-token containment rule
// that protects decoys for no-year daily shows.
func singleTokenContainedScore(want, got string, wantYear int) float64 {
	if wantYear <= 0 {
		return 0
	}
	gotTokens := titleTokens(got)
	if len(gotTokens) != 1 {
		return 0
	}
	for _, token := range titleTokens(want) {
		if token == gotTokens[0] {
			return singleTokenContained
		}
	}
	return 0
}

// titleTokens is the scoring-only normalisation: apostrophes are deleted
// rather than split into word separators, so TMDB's "Patch's" matches the
// parsed release name's "Patchs". normalizeTitle (and therefore the cache
// key) is deliberately unchanged.
func titleTokens(s string) []string {
	return strings.Fields(normalizeTitle(scoringApostrophes.Replace(s)))
}

// yearAllowed applies the year guard. A requested year with a missing hit
// year is ambiguous and rejected; with no requested year every hit remains
// eligible. Non-exact title matches keep the pinned ±1 gate, while an exact
// normalised-title match tolerates |diff| ≤ 2 for festival-vs-wide-release
// drift of identical titles (remakes with the same title are decades apart).
func yearAllowed(hitDate string, wantYear int, wantTitle, hitTitle string) bool {
	if wantYear <= 0 {
		return true
	}
	gotYear := yearFromDate(hitDate)
	if gotYear <= 0 {
		return false
	}
	diff := gotYear - wantYear
	if diff < 0 {
		diff = -diff
	}
	if diff <= 1 {
		return true
	}
	return diff <= 2 && normalizeTitle(wantTitle) == normalizeTitle(hitTitle)
}

// nearerYear reports whether the hit's year is strictly closer to the wanted
// year than the current best diff. Without a wanted year there is nothing to
// compare, so rank order is kept.
func nearerYear(hitDate string, wantYear, bestDiff int) bool {
	if wantYear <= 0 {
		return false
	}
	return absYearDiff(hitDate, wantYear) < bestDiff
}

func absYearDiff(hitDate string, wantYear int) int {
	diff := yearFromDate(hitDate) - wantYear
	if diff < 0 {
		diff = -diff
	}
	return diff
}

func cacheKey(title string, year int, isShow bool, season, episode int) string {
	return fmt.Sprintf("%s|%d|%t|%d|%d", normalizeTitle(title), year, isShow, season, episode)
}

// fallbackJunkRE strips codec, source, HDR and 3D tags from a retry query. It
// is never applied to the first search, which stays byte-for-byte identical.
var fallbackJunkRE = regexp.MustCompile(`(?i)\b(?:hdr10plus|hdr10|hdr|dv|remux|mpeg[- ]?2|mpeg|avc|x264|x265|h264|h265|hevc|web[- ]?dl|web|dl|webrip|bluray|blueray|blu|ray|3-?d|amzn|nf|dsnp|hmax|atvp)\b`)

// fallbackCleanTokens removes upload timestamps, release-group tails and known
// codec/source tags from a parsed title. Short leading numbers are part of
// titles ("12 Hour Shift", "101 Dalmatians 2") and are preserved.
func fallbackCleanTokens(title string) []string {
	cleaned := fallbackJunkRE.ReplaceAllString(
		strings.NewReplacer(".", " ", "_", " ").Replace(title), " ")
	out := make([]string, 0, 8)
	leading := true
	for _, field := range strings.Fields(cleaned) {
		// Upload-group suffixes are glued to a hyphen ("-BLURANiUM").
		if strings.HasPrefix(field, "-") {
			continue
		}
		for _, token := range titleTokens(field) {
			if leading && isUploadNumber(token) {
				continue
			}
			leading = false
			out = append(out, token)
		}
	}
	return out
}

// fallbackQueryOfSize joins the first n cleaned tokens, tolerating n above the
// token count.
func fallbackQueryOfSize(tokens []string, n int) string {
	if n > len(tokens) {
		n = len(tokens)
	}
	if n <= 0 {
		return ""
	}
	return strings.Join(tokens[:n], " ")
}

// fallbackSearchTitle is the four-token rung of the fallback ladder.
func fallbackSearchTitle(title string) string {
	return fallbackQueryOfSize(fallbackCleanTokens(title), 4)
}

// fallbackSearchTitles is the ordered set of retry queries: all cleaned
// tokens (capped at six), then progressively shorter prefixes. Duplicates and
// empty queries are skipped so a short parsed title is not searched twice.
func fallbackSearchTitles(title string) []string {
	tokens := fallbackCleanTokens(title)
	queries := make([]string, 0, 4)
	add := func(q string) {
		if q == "" {
			return
		}
		for _, previous := range queries {
			if previous == q {
				return
			}
		}
		queries = append(queries, q)
	}
	add(fallbackQueryOfSize(tokens, 6))
	add(fallbackSearchTitle(title))
	add(fallbackQueryOfSize(tokens, 3))
	add(fallbackQueryOfSize(tokens, 2))
	return queries
}

func isUploadNumber(token string) bool {
	if len(token) < 4 {
		return false
	}
	for _, r := range token {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// romanNumerals maps standalone multi-character Roman numerals to arabic so a
// release name and its TMDB listing compare equal ("Stake Land 2" vs
// "Stake Land II"). Only unambiguous multi-character numerals are mapped:
// i/v/x (real words and titles such as "V for Vendetta", "X" and "I") and
// longer forms like xx/c/d/m/l stay untouched.
var romanNumerals = map[string]string{
	"ii": "2", "iii": "3", "iv": "4", "vi": "6", "vii": "7",
	"viii": "8", "ix": "9", "xi": "11", "xii": "12",
}

// diacriticReplacer folds precomposed Latin letters to their ASCII base so a
// release name and its TMDB listing compare equal ("Aladdín"→"aladdin").
// Modifier letters that TMDB drops ("ʻOhana"→"Ohana") are removed, and letters
// with no ASCII base map to their conventional expansion (ß→"ss", æ→"ae",
// œ→"oe", þ→"th"). It runs after lower-casing, so only lowercase pairs are
// needed; combining marks from decomposed input are stripped separately by
// foldDiacritics via unicode.Mn.
var diacriticReplacer = strings.NewReplacer(
	"à", "a", "á", "a", "â", "a", "ã", "a", "ä", "a", "å", "a",
	"ā", "a", "ă", "a", "ą", "a", "ǎ", "a", "ǻ", "a", "ȁ", "a", "ȃ", "a",
	"æ", "ae",
	"ç", "c", "ć", "c", "ĉ", "c", "ċ", "c", "č", "c",
	"ď", "d", "đ", "d", "ð", "d",
	"è", "e", "é", "e", "ê", "e", "ë", "e",
	"ē", "e", "ĕ", "e", "ė", "e", "ę", "e", "ě", "e", "ȅ", "e", "ȇ", "e",
	"ĝ", "g", "ğ", "g", "ġ", "g", "ģ", "g",
	"ĥ", "h", "ħ", "h",
	"ì", "i", "í", "i", "î", "i", "ï", "i",
	"ĩ", "i", "ī", "i", "ĭ", "i", "į", "i", "ı", "i", "ǐ", "i", "ȉ", "i", "ȋ", "i",
	"ĳ", "ij",
	"ĵ", "j",
	"ķ", "k", "ĸ", "k",
	"ĺ", "l", "ļ", "l", "ľ", "l", "ŀ", "l", "ł", "l",
	"ñ", "n", "ń", "n", "ņ", "n", "ň", "n", "ŉ", "n", "ŋ", "n",
	"ò", "o", "ó", "o", "ô", "o", "õ", "o", "ö", "o", "ø", "o",
	"ō", "o", "ŏ", "o", "ő", "o", "ơ", "o", "ǿ", "o", "ȍ", "o", "ȏ", "o",
	"œ", "oe",
	"ŕ", "r", "ŗ", "r", "ř", "r",
	"ś", "s", "ŝ", "s", "ş", "s", "š", "s", "ș", "s", "ß", "ss",
	"ţ", "t", "ť", "t", "ŧ", "t", "ț", "t",
	"ù", "u", "ú", "u", "û", "u", "ü", "u",
	"ũ", "u", "ū", "u", "ŭ", "u", "ů", "u", "ű", "u", "ų", "u", "ư", "u",
	"ǔ", "u", "ȕ", "u", "ȗ", "u",
	"ŵ", "w",
	"ý", "y", "ÿ", "y", "ŷ", "y",
	"ź", "z", "ż", "z", "ž", "z",
	"þ", "th",
	"ʻ", "", "ʼ", "",
)

// foldDiacritics strips combining marks left over from decomposed input after
// diacriticReplacer has folded the precomposed forms.
func foldDiacritics(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, diacriticReplacer.Replace(s))
}

// normalizeTitle keeps letters and digits only, lower-cased and separated by
// single spaces. Diacritics are folded, hyphens joining two letters are
// deleted ("Z-O-M-B-I-E-S"→"zombies", "X-Men"→"xmen") and standalone
// multi-character Roman numerals map to arabic first, so both sides of every
// comparison — exact match, Dice, containment and the cache key — agree. A
// hyphen touching a digit still separates ("3754-Family", "265-EVO") so
// numeric-prefix and codec-token handling is unchanged. It is used for both
// the match check and the cache key.
func normalizeTitle(s string) string {
	words := strings.FieldsFunc(deleteLetterHyphens(foldDiacritics(strings.ToLower(s))), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for i, word := range words {
		if arabic, ok := romanNumerals[word]; ok {
			words[i] = arabic
		}
	}
	return strings.Join(words, " ")
}

// deleteLetterHyphens removes an ASCII hyphen only when it joins two letters,
// so stylised TMDB titles ("Z-O-M-B-I-E-S", "X-Men", "Spider-Man") normalise
// to the single token a parsed release name yields. Any other hyphen becomes
// a space, preserving the old split for numeric prefixes ("3754-Family") and
// codec tails ("265-EVO").
func deleteLetterHyphens(s string) string {
	if !strings.Contains(s, "-") {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range runes {
		if r != '-' {
			b.WriteRune(r)
			continue
		}
		var prev, next rune
		if i > 0 {
			prev = runes[i-1]
		}
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		if unicode.IsLetter(prev) && unicode.IsLetter(next) {
			continue
		}
		b.WriteRune(' ')
	}
	return b.String()
}

func yearFromDate(date string) int {
	if len(date) < 4 {
		return 0
	}
	year, err := strconv.Atoi(date[:4])
	if err != nil || year < 1900 || year > 2099 {
		return 0
	}
	return year
}

// imageBase returns the secure artwork base URL from /3/configuration. It is
// fetched once per client and reused; an unreachable configuration endpoint
// degrades to the documented public base URL.
func (c *Client) imageBase(ctx context.Context) string {
	c.mu.Lock()
	base := c.imageBaseURL
	c.mu.Unlock()
	if base != "" {
		return base
	}

	var cfg struct {
		Images struct {
			BaseURL       string `json:"base_url"`
			SecureBaseURL string `json:"secure_base_url"`
		} `json:"images"`
	}
	if err := c.getJSON(ctx, "/3/configuration", nil, &cfg); err != nil {
		return defaultImageBase
	}
	base = cfg.Images.SecureBaseURL
	if base == "" {
		base = cfg.Images.BaseURL
	}
	if !strings.HasPrefix(base, "https://") {
		base = defaultImageBase
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	c.mu.Lock()
	c.imageBaseURL = base
	c.mu.Unlock()
	return base
}

func imageURL(base, path, size string) string {
	if path == "" {
		return ""
	}
	return base + size + "/" + strings.TrimPrefix(path, "/")
}

// getJSON issues a GET and decodes a JSON reply. A 429 is retried at most
// maxRetries times, honouring Retry-After when it is a whole number of
// seconds; the wait is bounded and cancellable through ctx.
func (c *Client) getJSON(ctx context.Context, path string, params url.Values, dst any) error {
	root, err := url.Parse(c.base)
	if err != nil {
		return fmt.Errorf("tmdb: invalid base: %w", err)
	}
	query := url.Values{}
	for key, values := range params {
		for _, value := range values {
			query.Add(key, value)
		}
	}
	if c.readToken == "" && c.apiKey != "" {
		query.Set("api_key", c.apiKey)
	}
	root.Path = strings.TrimRight(root.Path, "/") + path
	root.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root.String(), nil)
	if err != nil {
		return c.safeErr(err)
	}
	req.Header.Set("Accept", "application/json")
	if c.readToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.readToken)
	}

	for attempt := 0; ; attempt++ {
		resp, err := c.http.Do(req)
		if err != nil {
			return c.safeErr(err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return c.safeErr(readErr)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			if attempt >= maxRetries {
				return fmt.Errorf("tmdb: %s: rate limited after %d retries", path, maxRetries)
			}
			wait := retryWait(resp.Header.Get("Retry-After"), attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("tmdb: %s: unexpected status %d", path, resp.StatusCode)
		}
		if len(body) > maxResponseBytes {
			return fmt.Errorf("tmdb: %s: response exceeds %d bytes", path, maxResponseBytes)
		}
		if err := json.Unmarshal(body, dst); err != nil {
			return fmt.Errorf("tmdb: %s: decode response: %w", path, err)
		}
		return nil
	}
}

func retryWait(retryAfter string, attempt int) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 {
		wait := time.Duration(seconds) * time.Second
		if wait > maxRetryWait {
			wait = maxRetryWait
		}
		return wait
	}
	return time.Duration(25<<attempt) * time.Millisecond
}

// safeErr strips either credential from an error string: a url.Error from the
// transport includes the full query, which would otherwise expose a v3 key.
func (c *Client) safeErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if c.apiKey != "" {
		msg = strings.ReplaceAll(msg, c.apiKey, "***")
	}
	if c.readToken != "" {
		msg = strings.ReplaceAll(msg, c.readToken, "***")
	}
	return errors.New(msg)
}
