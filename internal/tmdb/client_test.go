package tmdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/leleasley/rex-alt/internal/models"
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

// The parser leaves residual release junk in these real-world titles
// ("AVC", "X 265-EVO", "-UnKn0wn", and "2" where TMDB uses "II"). The old
// exact-normalised-title guard rejected all of them, so Plex showed the raw
// filename. Each fake response puts the correct hit below rank 1 and includes
// decoys, including near matches that must not outscore it.
func TestEnrichResolvesNoisyMovieTitles(t *testing.T) {
	cases := []struct {
		name      string
		search    string
		title     string
		year      int
		wantID    int
		wantTitle string
		wantDate  string
	}{
		{
			name:      "roman numeral and stray AVC",
			search:    "movie_search_dalmatians.json",
			title:     "101 Dalmatians 2 Patchs London Adventure AVC",
			year:      2003,
			wantID:    9102,
			wantTitle: "101 Dalmatians II: Patch's London Adventure",
			wantDate:  "2003-01-21",
		},
		{
			name:      "codec tail after repeated title word",
			search:    "movie_search_christmas.json",
			title:     "A Christmas Story Christmas X 265-EVO",
			year:      2022,
			wantID:    9203,
			wantTitle: "A Christmas Story Christmas",
			wantDate:  "2022-11-17",
		},
		{
			name:      "upload group suffix",
			search:    "movie_search_wounded_fawn.json",
			title:     "A Wounded Fawn AVC -UnKn0wn",
			year:      2022,
			wantID:    9303,
			wantTitle: "A Wounded Fawn",
			wantDate:  "2022-09-16",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configBody := fixture(t, "configuration.json")
			searchBody := fixture(t, tc.search)
			wantDetails := fmt.Sprintf("/3/movie/%d", tc.wantID)
			detailsBody := fmt.Sprintf(
				`{"id":%d,"imdb_id":"tt%07d","title":%q,"release_date":%q,`+
					`"overview":"Scored match overview.",`+
					`"poster_path":"/scored-poster.jpg","backdrop_path":"/scored-backdrop.jpg"}`,
				tc.wantID, tc.wantID, tc.wantTitle, tc.wantDate)

			var mu sync.Mutex
			var detailsPaths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/3/configuration":
					writeJSON(w, configBody)
				case "/3/search/movie":
					writeJSON(w, searchBody)
				case wantDetails:
					mu.Lock()
					detailsPaths = append(detailsPaths, r.URL.Path)
					mu.Unlock()
					writeJSON(w, detailsBody)
				default:
					// Any details fetch for a decoy id is a wrong-id attach.
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
			en, ok, err := c.Enrich(context.Background(), tc.title, tc.year, 0, 0, false)
			if err != nil {
				t.Fatalf("Enrich: %v", err)
			}
			if !ok {
				t.Fatalf("Enrich returned ok=false for %q; want TMDB id %d", tc.title, tc.wantID)
			}
			if en.TMDBID != tc.wantID {
				t.Errorf("TMDBID = %d, want %d", en.TMDBID, tc.wantID)
			}
			if en.Title != tc.wantTitle {
				t.Errorf("Title = %q, want the corrected TMDB title %q", en.Title, tc.wantTitle)
			}
			if en.Date != tc.wantDate {
				t.Errorf("Date = %q, want %q", en.Date, tc.wantDate)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(detailsPaths) != 1 || detailsPaths[0] != wantDetails {
				t.Errorf("details paths = %v, want only %s", detailsPaths, wantDetails)
			}
		})
	}
}

// Pin the wrong-id guard: an unrelated top hit and a low-scoring follower must
// never produce an id or a details request, even when years line up.
func TestEnrichRejectsUnrelatedTopHit(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := fixture(t, "movie_search_unrelated_top.json")
	var mu sync.Mutex
	fetched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			writeJSON(w, searchBody)
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
	if ok || en != (models.Enriched{}) {
		t.Errorf("Enrich = %+v, %v; want ok=false and zero Enriched", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if fetched {
		t.Error("client fetched details for an unrelated hit")
	}
}

func TestTitleScoreNoisyTitlesClearThreshold(t *testing.T) {
	cases := []struct {
		name, want, got string
		lo, hi          float64
	}{
		{"exact normalized match", "A Wounded Fawn", "A Wounded Fawn", 1, 1},
		{"roman numeral and possessive", "101 Dalmatians 2 Patchs London Adventure AVC", "101 Dalmatians II: Patch's London Adventure", 0.75, 0.80},
		{"codec tail", "A Christmas Story Christmas X 265-EVO", "A Christmas Story Christmas", 0.70, 0.75},
		{"upload group suffix", "A Wounded Fawn AVC -UnKn0wn", "A Wounded Fawn", 0.74, 0.76},
		{"unrelated", "Swallowed", "A Completely Different Film", 0, 0},
		{"near miss below threshold", "Swallowed", "Swallowed By The Earth", 0.39, 0.41},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := titleScore(tc.want, tc.got)
			if got < tc.lo || got > tc.hi {
				t.Errorf("titleScore(%q, %q) = %.4f, want between %.4f and %.4f",
					tc.want, tc.got, got, tc.lo, tc.hi)
			}
			if got >= 1 && normalizeTitle(tc.want) != normalizeTitle(tc.got) {
				t.Errorf("non-exact pair scored 1.0: %q / %q", tc.want, tc.got)
			}
		})
	}
}

func TestBestHitIndexGuardsAndTieBreaks(t *testing.T) {
	type hit struct{ title, date string }
	titleOf := func(h hit) string { return h.title }
	dateOf := func(h hit) string { return h.date }
	unrelated := hit{title: "Completely Different Film", date: "2022-01-01"}

	cases := []struct {
		name    string
		results []hit
		want    string
		year    int
		index   int
	}{
		{
			name:    "rank five is examined",
			results: []hit{unrelated, unrelated, unrelated, unrelated, {title: "A Wounded Fawn", date: "2022-09-16"}},
			want:    "A Wounded Fawn", year: 2022, index: 4,
		},
		{
			name:    "rank six is out of scope",
			results: []hit{unrelated, unrelated, unrelated, unrelated, unrelated, {title: "A Wounded Fawn", date: "2022-09-16"}},
			want:    "A Wounded Fawn", year: 2022, index: -1,
		},
		{
			name:    "exact normalized match wins a score tie",
			results: []hit{{title: "Fawn A Wounded", date: "2022-01-01"}, {title: "A Wounded Fawn", date: "2022-09-16"}},
			want:    "A Wounded Fawn", year: 2022, index: 1,
		},
		{
			name:    "wrong-year lookalike is skipped",
			results: []hit{{title: "A Wounded Fawn", date: "2019-01-01"}, {title: "A Wounded Fawn", date: "2022-09-16"}},
			want:    "A Wounded Fawn", year: 2022, index: 1,
		},
		{
			name:    "all hits wrong year",
			results: []hit{{title: "A Wounded Fawn", date: "2019-01-01"}},
			want:    "A Wounded Fawn", year: 2022, index: -1,
		},
		{
			name:    "no year requested accepts a missing date",
			results: []hit{{title: "A Wounded Fawn", date: ""}},
			want:    "A Wounded Fawn", year: 0, index: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bestHitIndex(tc.results, titleOf, dateOf, tc.want, tc.year); got != tc.index {
				t.Errorf("bestHitIndex = %d, want %d", got, tc.index)
			}
		})
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

// Daily wrestling brands are released with a WWE prefix and an episode date,
// but TMDB indexes the series under a shorter name. These fake results put the
// "Classics"/"Talk"/"UK" decoys ahead of the real show: the documented alias
// must rewrite the query and let the exact hit win with no year gate (the
// parsed 2026 date is an air date, so year is 0).
func TestEnrichResolvesDailyWrestlingShows(t *testing.T) {
	cases := []struct {
		name, title, search, wantQuery, wantTitle, wantDate string
		wantID                                              int
	}{
		{
			name:      "monday night raw",
			title:     "WWE Monday Night RAW",
			search:    "tv_search_raw.json",
			wantQuery: "Raw",
			wantID:    8104,
			wantTitle: "Raw",
			wantDate:  "1993-01-11",
		},
		{
			name:      "friday night smackdown",
			title:     "WWE Friday Night SmackDown",
			search:    "tv_search_smackdown.json",
			wantQuery: "WWE Friday Night SmackDown",
			wantID:    8203,
			wantTitle: "WWE Friday Night SmackDown",
			wantDate:  "1999-08-26",
		},
		{
			name:      "nxt",
			title:     "WWE NXT",
			search:    "tv_search_nxt.json",
			wantQuery: "NXT",
			wantID:    8302,
			wantTitle: "NXT",
			wantDate:  "2010-06-23",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configBody := fixture(t, "configuration.json")
			searchBody := fixture(t, tc.search)

			var mu sync.Mutex
			var paths []string
			var search seenRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				switch r.URL.Path {
				case "/3/configuration":
					writeJSON(w, configBody)
				case "/3/search/tv":
					snap := snapshot(r)
					mu.Lock()
					search = snap
					mu.Unlock()
					writeJSON(w, searchBody)
				default:
					// Fetching a decoy's endpoint would be a wrong-id attach.
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
			en, ok, err := c.Enrich(context.Background(), tc.title, 0, 0, 0, true)
			if err != nil {
				t.Fatalf("Enrich: %v", err)
			}
			if !ok {
				t.Fatalf("Enrich returned ok=false for %q; want TMDB id %d", tc.title, tc.wantID)
			}
			if en.TMDBID != tc.wantID || en.ShowTitle != tc.wantTitle || en.Date != tc.wantDate {
				t.Errorf("Enriched = %+v, want id=%d title=%q date=%q", en, tc.wantID, tc.wantTitle, tc.wantDate)
			}

			mu.Lock()
			defer mu.Unlock()
			if search.path == "" {
				t.Fatal("no /3/search/tv request was made")
			}
			if got := search.query.Get("query"); got != tc.wantQuery {
				t.Errorf("query = %q, want alias %q", got, tc.wantQuery)
			}
			if got := search.query.Get("first_air_date_year"); got != "" {
				t.Errorf("first_air_date_year = %q, want absent when year is 0", got)
			}
			for _, p := range paths {
				if strings.HasPrefix(p, "/3/tv/") {
					t.Errorf("fetched a show endpoint %q; no season/episode was requested", p)
				}
			}
		})
	}
}

// With year=0 the year gate is open, so rejection must come from the pinned
// 0.6 Dice threshold. A low-scoring lookalike and unrelated shows must never
// yield an id or a details fetch, even when an alias was applied.
func TestEnrichRejectsUnrelatedTVHitWithNoYear(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := fixture(t, "tv_search_unrelated.json")
	var mu sync.Mutex
	fetched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/tv":
			writeJSON(w, searchBody)
		default:
			mu.Lock()
			fetched = true
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "WWE Monday Night RAW", 0, 0, 0, true)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if ok || en != (models.Enriched{}) {
		t.Errorf("Enrich = %+v, %v; want ok=false and zero Enriched", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if fetched {
		t.Error("client fetched details for an unrelated TV hit")
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
