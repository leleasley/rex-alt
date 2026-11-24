package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
// (when a year was parsed) carries a year within one of it. An ambiguous or
// mismatched hit yields ok=false and no id, so an unrelated TMDB id can never
// be attached to a file. On the TV path, a small documented alias map rewrites
// well-known daily/wrestling brands before search; all other titles are
// untouched. Successful results are cached in SQLite under the normalised
// title|year|isShow|season|episode key.
func (c *Client) Enrich(ctx context.Context, title string, year, season, episode int, isShow bool) (models.Enriched, bool, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return models.Enriched{}, false, nil
	}
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
		if len(results.Results) == 0 {
			return models.Enriched{}, false, nil
		}
		idx := bestHitIndex(results.Results,
			func(r searchResult) string { return r.Name },
			func(r searchResult) string { return r.FirstAirDate },
			title, year)
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
		if len(results.Results) == 0 {
			return models.Enriched{}, false, nil
		}
		idx := bestHitIndex(results.Results,
			func(r searchResult) string { return r.Title },
			func(r searchResult) string { return r.ReleaseDate },
			title, year)
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
// is a hard eligibility gate: when a year was parsed, a hit with a missing or
// out-of-range year is skipped before scoring, so a wrong-year lookalike can
// never displace the right film or trigger a wrong-id attach. Higher scores
// win; ties keep the higher-ranked result, except that an exact normalised
// title match (score 1.0) wins a tie as required.
func bestHitIndex[T any](results []T, titleOf, dateOf func(T) string, wantTitle string, wantYear int) int {
	limit := len(results)
	if limit > maxSearchResults {
		limit = maxSearchResults
	}
	exact := normalizeTitle(wantTitle)
	best, bestScore, bestExact := -1, -1.0, false
	for i := 0; i < limit; i++ {
		hit := results[i]
		if !yearAllowed(dateOf(hit), wantYear) {
			continue
		}
		hitTitle := titleOf(hit)
		score := titleScore(wantTitle, hitTitle)
		isExact := normalizeTitle(hitTitle) == exact
		if score > bestScore || (score == bestScore && isExact && !bestExact) {
			best, bestScore, bestExact = i, score, isExact
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

// titleTokens is the scoring-only normalisation: apostrophes are deleted
// rather than split into word separators, so TMDB's "Patch's" matches the
// parsed release name's "Patchs". normalizeTitle (and therefore the cache
// key) is deliberately unchanged.
func titleTokens(s string) []string {
	return strings.Fields(normalizeTitle(scoringApostrophes.Replace(s)))
}

// yearAllowed applies the pinned ±1 year guard. A requested year with a
// missing hit year is ambiguous and rejected; with no requested year every
// hit remains eligible.
func yearAllowed(hitDate string, wantYear int) bool {
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
	return diff <= 1
}

func cacheKey(title string, year int, isShow bool, season, episode int) string {
	return fmt.Sprintf("%s|%d|%t|%d|%d", normalizeTitle(title), year, isShow, season, episode)
}

// normalizeTitle keeps letters and digits only, lower-cased and separated by
// single spaces. It is used for both the match check and the cache key.
func normalizeTitle(s string) string {
	var b strings.Builder
	pendingSpace := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if pendingSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			pendingSpace = false
			b.WriteRune(r)
		} else {
			pendingSpace = true
		}
	}
	return strings.TrimSpace(b.String())
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
