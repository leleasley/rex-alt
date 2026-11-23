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
	showPosterSize   = "w300"
	backdropSize     = "w780"
	stillSize        = "w500"
	maxRetries       = 3
	maxResponseBytes = 1 << 20
	defaultTimeout   = 15 * time.Second
	maxRetryWait     = 2 * time.Second
)

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
// accepted. A top search hit is accepted only when its normalised title
// matches and its year is within one of the requested year (when one was
// supplied); an ambiguous or mismatched hit yields ok=false and no id, so an
// unrelated TMDB id can never be attached to a file. Successful results are
// cached in SQLite under the normalised title|year|isShow|season|episode key.
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
		top := results.Results[0]
		if !hitMatches(top.Name, top.FirstAirDate, title, year) {
			return models.Enriched{}, false, nil
		}
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
		top := results.Results[0]
		if !hitMatches(top.Title, top.ReleaseDate, title, year) {
			return models.Enriched{}, false, nil
		}
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

// hitMatches is the guard against attaching a wrong TMDB id: the normalised
// titles must be equal, and when a year was requested the hit must carry a
// year within one of it. A requested year with a missing hit year is treated
// as ambiguous and rejected.
func hitMatches(hitTitle, hitDate, wantTitle string, wantYear int) bool {
	if normalizeTitle(hitTitle) != normalizeTitle(wantTitle) {
		return false
	}
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
