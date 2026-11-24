package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/leleasley/rex-alt/internal/store"
)

const (
	testToken = "v4-secret-token"
	testKey   = "v3-secret-key"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(body)
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "tmdb.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestClient(t *testing.T, base, token, apiKey string, cache *store.Store) *Client {
	t.Helper()
	c := NewClient(token, apiKey, cache, nil)
	c.base = base
	return c
}

type seenRequest struct {
	path  string
	query url.Values
	auth  string
}

func snapshot(r *http.Request) seenRequest {
	return seenRequest{path: r.URL.Path, query: r.URL.Query(), auth: r.Header.Get("Authorization")}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestEnrichSendsBearerAndUsesCache(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := fixture(t, "movie_search.json")
	detailsBody := fixture(t, "movie_details.json")

	var mu sync.Mutex
	var seen []seenRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, snapshot(r))
		mu.Unlock()
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			writeJSON(w, searchBody)
		case "/3/movie/12345":
			writeJSON(w, detailsBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	ctx := context.Background()

	first, ok, err := c.Enrich(ctx, "Swallowed", 2022, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if !ok {
		t.Fatal("Enrich returned ok=false for an exact title/year match")
	}
	if first.TMDBID != 12345 || first.IMDbID != "tt1234567" {
		t.Errorf("ids = %d / %q, want 12345 / tt1234567", first.TMDBID, first.IMDbID)
	}
	if first.Title != "Swallowed" || first.Summary != "A swallowed movie summary." || first.Date != "2022-03-11" {
		t.Errorf("metadata = %+v", first)
	}
	if want := "https://image.tmdb.org/t/p/w500/poster.jpg"; first.Poster != want {
		t.Errorf("poster = %q, want %q", first.Poster, want)
	}
	if want := "https://image.tmdb.org/t/p/w780/backdrop.jpg"; first.Backdrop != want {
		t.Errorf("backdrop = %q, want %q", first.Backdrop, want)
	}

	mu.Lock()
	before := len(seen)
	requests := append([]seenRequest(nil), seen...)
	mu.Unlock()
	if before != 3 {
		t.Fatalf("got %d requests, want 3 (configuration, search, details)", before)
	}
	for _, req := range requests {
		if req.auth != "Bearer "+testToken {
			t.Errorf("%s Authorization = %q, want bearer token", req.path, req.auth)
		}
		if req.query.Get("api_key") != "" {
			t.Errorf("%s carried an api_key query param", req.path)
		}
	}
	var search seenRequest
	for _, req := range requests {
		if req.path == "/3/search/movie" {
			search = req
		}
	}
	if search.path == "" || search.query.Get("query") != "Swallowed" ||
		search.query.Get("primary_release_year") != "2022" {
		t.Errorf("search request = %s %v", search.path, search.query)
	}

	second, ok, err := c.Enrich(ctx, "Swallowed", 2022, 0, 0, false)
	if err != nil || !ok {
		t.Fatalf("second Enrich = %+v, %v, %v", second, ok, err)
	}
	if second != first {
		t.Errorf("cached Enriched = %+v, want %+v", second, first)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != before {
		t.Errorf("second Enrich made %d extra HTTP requests", len(seen)-before)
	}
}

func TestEnrichRetries429(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := fixture(t, "movie_search.json")
	detailsBody := fixture(t, "movie_details.json")

	var mu sync.Mutex
	searchAttempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			mu.Lock()
			searchAttempts++
			n := searchAttempts
			mu.Unlock()
			if n <= 2 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			writeJSON(w, searchBody)
		case "/3/movie/12345":
			writeJSON(w, detailsBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "Swallowed", 2022, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if !ok || en.TMDBID != 12345 {
		t.Fatalf("Enrich = %+v, %v; want TMDBID 12345", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if searchAttempts != 3 {
		t.Errorf("search attempts = %d, want 3 (two 429s then success)", searchAttempts)
	}
}

func TestEnrichStopsAfterThreeRetries(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/3/search/movie" {
			mu.Lock()
			attempts++
			mu.Unlock()
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "Swallowed", 2022, 0, 0, false)
	if err == nil {
		t.Fatal("Enrich returned nil error after four consecutive 429s")
	}
	if ok || en.TMDBID != 0 {
		t.Errorf("Enrich = %+v, %v; want empty result", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 4 {
		t.Errorf("attempts = %d, want 4 (initial + 3 retries)", attempts)
	}
}

func TestEnrichRejectsMismatchedYear(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	detailsBody := fixture(t, "movie_details.json")

	t.Run("too far", func(t *testing.T) {
		badYear := fixture(t, "movie_search_bad_year.json")
		var mu sync.Mutex
		var paths []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			switch r.URL.Path {
			case "/3/configuration":
				writeJSON(w, configBody)
			case "/3/search/movie":
				writeJSON(w, badYear)
			case "/3/movie/12345":
				writeJSON(w, detailsBody)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
		en, ok, err := c.Enrich(context.Background(), "Swallowed", 2022, 0, 0, false)
		if err != nil {
			t.Fatalf("Enrich: %v", err)
		}
		if ok || en.TMDBID != 0 {
			t.Errorf("Enrich = %+v, %v; want ok=false and no id for a 1999 hit asked for 2022", en, ok)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, p := range paths {
			if p == "/3/movie/666" || p == "/3/configuration" {
				t.Errorf("mismatched hit was fetched from %s", p)
			}
		}
	})

	t.Run("within one year is accepted", func(t *testing.T) {
		searchBody := fixture(t, "movie_search.json")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/3/configuration":
				writeJSON(w, configBody)
			case "/3/search/movie":
				writeJSON(w, searchBody)
			case "/3/movie/12345":
				writeJSON(w, detailsBody)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
		en, ok, err := c.Enrich(context.Background(), "Swallowed", 2021, 0, 0, false)
		if err != nil || !ok || en.TMDBID != 12345 {
			t.Fatalf("Enrich = %+v, %v, %v; want TMDBID 12345", en, ok, err)
		}
	})
}

func TestEnrichRejectsMismatchedTitle(t *testing.T) {
	badTitle := fixture(t, "movie_search_bad_title.json")
	var mu sync.Mutex
	fetched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/search/movie":
			writeJSON(w, badTitle)
		default:
			mu.Lock()
			fetched = true
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "Swallowed", 2022, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if ok || en.TMDBID != 0 {
		t.Errorf("Enrich = %+v, %v; want ok=false for a different title", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if fetched {
		t.Error("client fetched details for a mismatched title")
	}
}

func TestEnrichUsesV3APIKeyWhenNoBearer(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := fixture(t, "movie_search.json")
	detailsBody := fixture(t, "movie_details.json")

	var mu sync.Mutex
	var seen []seenRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, snapshot(r))
		mu.Unlock()
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			writeJSON(w, searchBody)
		case "/3/movie/12345":
			writeJSON(w, detailsBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, "", testKey, newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "Swallowed", 2022, 0, 0, false)
	if err != nil || !ok || en.TMDBID != 12345 {
		t.Fatalf("Enrich = %+v, %v, %v", en, ok, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("got %d requests, want 3", len(seen))
	}
	for _, req := range seen {
		if req.query.Get("api_key") != testKey {
			t.Errorf("%s api_key = %q, want %q", req.path, req.query.Get("api_key"), testKey)
		}
		if req.auth != "" {
			t.Errorf("%s carried Authorization %q with only a v3 key", req.path, req.auth)
		}
	}
}

func TestEnrichShowEpisode(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := fixture(t, "tv_search.json")
	episodeBody := fixture(t, "tv_episode.json")

	var mu sync.Mutex
	var seen []seenRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, snapshot(r))
		mu.Unlock()
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/tv":
			writeJSON(w, searchBody)
		case "/3/tv/555/season/1/episode/3":
			writeJSON(w, episodeBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "Something Very Bad Is Going to Happen", 0, 1, 3, true)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if !ok {
		t.Fatal("Enrich returned ok=false for a matching show")
	}
	if en.TMDBID != 555 || en.EpisodeTMDBID != 8899 {
		t.Errorf("ids = %d / %d, want 555 / 8899", en.TMDBID, en.EpisodeTMDBID)
	}
	if en.ShowTitle != "Something Very Bad Is Going to Happen" || en.Title != "Episode Three" ||
		en.Summary != "An episode summary." || en.Date != "2025-02-10" {
		t.Errorf("show/episode metadata = %+v", en)
	}
	if want := "https://image.tmdb.org/t/p/w342/showposter.jpg"; en.ShowPoster != want {
		t.Errorf("show poster = %q, want %q", en.ShowPoster, want)
	}
	if want := "https://image.tmdb.org/t/p/w780/showbackdrop.jpg"; en.ShowBackdrop != want {
		t.Errorf("show backdrop = %q, want %q", en.ShowBackdrop, want)
	}
	if want := "https://image.tmdb.org/t/p/w300/still.jpg"; en.Poster != want {
		t.Errorf("episode still = %q, want %q", en.Poster, want)
	}
	if en.IMDbID != "" {
		t.Errorf("IMDbID = %q, want empty for a show", en.IMDbID)
	}

	mu.Lock()
	defer mu.Unlock()
	var search seenRequest
	for _, req := range seen {
		if req.path == "/3/search/tv" {
			search = req
		}
	}
	if search.path == "" {
		t.Fatal("no /3/search/tv request was made")
	}
	if search.query.Get("query") != "Something Very Bad Is Going to Happen" {
		t.Errorf("query = %q", search.query.Get("query"))
	}
	if search.query.Get("first_air_date_year") != "" {
		t.Errorf("first_air_date_year = %q, want absent when year is 0", search.query.Get("first_air_date_year"))
	}
}

func TestEnrichCachesConfigurationAndKeysByEpisode(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := fixture(t, "tv_search.json")
	episodeBody := fixture(t, "tv_episode.json")

	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/tv":
			writeJSON(w, searchBody)
		default:
			if strings.HasPrefix(r.URL.Path, "/3/tv/555/season/1/episode/") {
				writeJSON(w, episodeBody)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	ctx := context.Background()
	for _, ep := range []int{3, 4} {
		if _, ok, err := c.Enrich(ctx, "Something Very Bad Is Going to Happen", 0, 1, ep, true); err != nil || !ok {
			t.Fatalf("Enrich episode %d = %v, %v", ep, ok, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	configs, searches, episodes := 0, 0, 0
	for _, p := range paths {
		switch {
		case p == "/3/configuration":
			configs++
		case p == "/3/search/tv":
			searches++
		case strings.HasPrefix(p, "/3/tv/555/season/1/episode/"):
			episodes++
		}
	}
	if configs != 1 {
		t.Errorf("configuration fetched %d times, want 1", configs)
	}
	if searches != 2 {
		t.Errorf("show searched %d times, want 2 (cache key includes episode)", searches)
	}
	if episodes != 2 {
		t.Errorf("episode endpoint called %d times, want 2", episodes)
	}
}

func TestErrorsNeverEchoCredentials(t *testing.T) {
	t.Run("v3 key", func(t *testing.T) {
		c := newTestClient(t, "http://127.0.0.1:1", "", testKey, newTestStore(t))
		_, ok, err := c.Enrich(context.Background(), "Swallowed", 2022, 0, 0, false)
		if err == nil {
			t.Fatal("expected a transport error")
		}
		if ok {
			t.Error("ok=true on a transport error")
		}
		if strings.Contains(err.Error(), testKey) {
			t.Errorf("error echoed the API key: %q", err)
		}
		if !strings.Contains(err.Error(), "***") {
			t.Errorf("error did not redact the API key: %q", err)
		}
	})

	t.Run("v4 token", func(t *testing.T) {
		c := newTestClient(t, "http://127.0.0.1:1", testToken, "", newTestStore(t))
		_, _, err := c.Enrich(context.Background(), "Swallowed", 2022, 0, 0, false)
		if err == nil {
			t.Fatal("expected a transport error")
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("error echoed the bearer token: %q", err)
		}
	})
}
