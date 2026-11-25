package tmdb

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

// A parsed release can end in codec/group tokens ("H.265-XEBEC"): the real
// TMDB title is then a strict subset of the query tokens and Dice alone falls
// below 0.6. Containment must recover the exact film even with decoys ahead of
// it, without fetching details for any decoy.
func TestEnrichResolvesTitleWithTrailingCodecTokens(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := fixture(t, "movie_search_train_dreams.json")
	const wantID = 1241983
	const wantDetails = "/3/movie/1241983"
	detailsBody := `{"id":1241983,"imdb_id":"tt1241983","title":"Train Dreams",` +
		`"release_date":"2025-11-21","overview":"A logger's life in the American West.",` +
		`"poster_path":"/train-dreams-poster.jpg","backdrop_path":"/train-dreams-backdrop.jpg"}`

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
	en, ok, err := c.Enrich(context.Background(), "Train Dreams H 265-XEBEC", 2025, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if !ok {
		t.Fatal("Enrich returned ok=false for a title with trailing codec tokens")
	}
	if en.TMDBID != wantID {
		t.Errorf("TMDBID = %d, want %d", en.TMDBID, wantID)
	}
	if en.Title != "Train Dreams" || en.Date != "2025-11-21" {
		t.Errorf("metadata = %+v", en)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(detailsPaths) != 1 || detailsPaths[0] != wantDetails {
		t.Errorf("details paths = %v, want only %s", detailsPaths, wantDetails)
	}
}

// With no year parsed the year gate is open, so every hit is eligible and the
// score alone must pick the film. A constant containment boost flattened the
// shorter subset and the full sequel to the same score, letting the
// higher-ranked shorter film win; a proportional boost must prefer the more
// complete candidate.
func TestEnrichNoYearPrefersMoreCompleteContainedTitle(t *testing.T) {
	cases := []struct {
		name, search, title, wantTitle string
		wantID                         int
		wantDate                       string
	}{
		{
			name:      "shorter subset does not displace the full title",
			search:    "movie_search_christmas.json",
			title:     "A Christmas Story Christmas X 265-EVO",
			wantID:    9203,
			wantTitle: "A Christmas Story Christmas",
			wantDate:  "2022-11-17",
		},
		{
			name:      "subset does not displace a roman-numeral sequel",
			search:    "movie_search_dalmatians.json",
			title:     "101 Dalmatians 2 Patchs London Adventure AVC",
			wantID:    9102,
			wantTitle: "101 Dalmatians II: Patch's London Adventure",
			wantDate:  "2003-01-21",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configBody := fixture(t, "configuration.json")
			searchBody := fixture(t, tc.search)
			wantDetails := fmt.Sprintf("/3/movie/%d", tc.wantID)
			detailsBody := fmt.Sprintf(
				`{"id":%d,"imdb_id":"tt%07d","title":%q,"release_date":%q,`+
					`"overview":"Proportional containment overview.",`+
					`"poster_path":"/p.jpg","backdrop_path":"/b.jpg"}`,
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
					// Fetching a decoy's details would be a wrong-id attach.
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
			en, ok, err := c.Enrich(context.Background(), tc.title, 0, 0, 0, false)
			if err != nil {
				t.Fatalf("Enrich: %v", err)
			}
			if !ok {
				t.Fatalf("Enrich returned ok=false for %q", tc.title)
			}
			if en.TMDBID != tc.wantID {
				t.Errorf("TMDBID = %d, want %d (no year: prefer the more complete title)", en.TMDBID, tc.wantID)
			}
			if en.Title != tc.wantTitle || en.Date != tc.wantDate {
				t.Errorf("metadata = %+v", en)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(detailsPaths) != 1 || detailsPaths[0] != wantDetails {
				t.Errorf("details paths = %v, want only %s", detailsPaths, wantDetails)
			}
		})
	}
}

// Control for the open year gate: with no year parsed, rejection must still
// come from the score, and no details request may be made for a decoy.
func TestEnrichRejectsUnrelatedCandidateWithNoYear(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := fixture(t, "movie_search_train_dreams_extended.json")
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
	en, ok, err := c.Enrich(context.Background(), "Train Dreams H 265-XEBEC", 0, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if ok || en != (models.Enriched{}) {
		t.Errorf("Enrich = %+v, %v; want ok=false and zero Enriched", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if fetched {
		t.Error("client fetched details for an unrelated no-year candidate")
	}
}

// Containment must not widen the wrong-id guard: a candidate with extra tokens
// that are absent from the parsed query is still rejected, even when its other
// tokens and year line up.
func TestEnrichRejectsExtendedTitleCandidate(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := fixture(t, "movie_search_train_dreams_extended.json")
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
	en, ok, err := c.Enrich(context.Background(), "Train Dreams H 265-XEBEC", 2025, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if ok || en != (models.Enriched{}) {
		t.Errorf("Enrich = %+v, %v; want ok=false and zero Enriched for an extended title", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if fetched {
		t.Error("client fetched details for a candidate with extra tokens")
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
		// Numeral-mapped: "2" and "II" are the same token after
		// normalizeTitle, so 12 of 13 tokens are shared.
		{"roman numeral and possessive", "101 Dalmatians 2 Patchs London Adventure AVC", "101 Dalmatians II: Patch's London Adventure", 0.92, 0.93},
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

// Containment must be proportional to how much of the parsed query the
// candidate covers: a more complete title scores higher, and the boost always
// stays strictly below the 1.0 of an exact normalised match.
func TestContainmentScoreIsProportional(t *testing.T) {
	cases := []struct {
		name, want, got string
		score           float64
	}{
		{"two of five query tokens", "Train Dreams H 265-XEBEC", "Train Dreams", 0.74},
		{"three of seven query tokens", "A Christmas Story Christmas X 265-EVO", "A Christmas Story", 0.75},
		{"four of seven query tokens", "A Christmas Story Christmas X 265-EVO", "A Christmas Story Christmas", 0.80},
		{"full query stays below exact", "Train Dreams", "Train Dreams", 0.95},
		{"candidate with extra token is not contained", "Train Dreams H 265-XEBEC", "Train Dreams Revisited", 0},
		{"single-token candidate is not contained", "Swallowed H 265-XEBEC", "Swallowed", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := containmentScore(tc.want, tc.got)
			if math.Abs(got-tc.score) > 1e-9 {
				t.Errorf("containmentScore(%q, %q) = %.4f, want %.4f", tc.want, tc.got, got, tc.score)
			}
			if got >= 1 {
				t.Errorf("containment score %v must stay below exact-match 1.0", got)
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
		{
			name:    "contained title outranks an extended decoy",
			results: []hit{{title: "Train Dreams Revisited", date: "2025-06-01"}, {title: "Train Dreams", date: "2025-11-21"}},
			want:    "Train Dreams H 265-XEBEC", year: 2025, index: 1,
		},
		{
			name:    "candidate with extra tokens is not contained",
			results: []hit{{title: "Train Dreams Revisited", date: "2025-11-21"}},
			want:    "Train Dreams H 265-XEBEC", year: 2025, index: -1,
		},
		{
			name:    "no year: shorter contained title does not win",
			results: []hit{{title: "A Christmas Story", date: "1983-11-18"}, {title: "A Christmas Story Christmas", date: "2022-11-17"}},
			want:    "A Christmas Story Christmas X 265-EVO", year: 0, index: 1,
		},
		{
			name:    "no year: contained subset does not beat a roman-numeral sequel",
			results: []hit{{title: "101 Dalmatians", date: "1996-11-27"}, {title: "101 Dalmatians II: Patch's London Adventure", date: "2003-01-21"}},
			want:    "101 Dalmatians 2 Patchs London Adventure AVC", year: 0, index: 1,
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

// A parsed title can be a strict subset of the official TMDB title
// ("Borat Subsequent Moviefilm" vs the full official title): Dice is diluted
// below 0.6 and forward containment is 0, so reverse containment must recover
// the full title while the year gate still applies.
func TestReverseContainmentScoreMirrorsContainment(t *testing.T) {
	cases := []struct {
		name, want, got string
		score           float64
	}{
		{"three of eighteen candidate tokens", "Borat Subsequent Moviefilm", "Borat Subsequent Moviefilm Delivery of Prodigious Bribe to American Regime for Make Benefit Once Glorious Nation of Kazakhstan", 0.6 + 0.35*3.0/18.0},
		{"full candidate stays below exact", "Train Dreams", "Train Dreams", 0.95},
		{"query with extra token is not reverse contained", "Train Dreams Revisited", "Train Dreams", 0},
		{"single-token query is not reverse contained", "Relay", "Relay Station", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reverseContainmentScore(tc.want, tc.got)
			if math.Abs(got-tc.score) > 1e-9 {
				t.Errorf("reverseContainmentScore(%q, %q) = %.4f, want %.4f", tc.want, tc.got, got, tc.score)
			}
			if got >= 1 {
				t.Errorf("reverse containment score %v must stay below exact-match 1.0", got)
			}
		})
	}
}

func TestEnrichResolvesSupersetCandidateTitle(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	const fullTitle = "Borat Subsequent Moviefilm: Delivery of Prodigious Bribe to American Regime for Make Benefit Once Glorious Nation of Kazakhstan"
	searchBody := `{"results":[{"id":659986,"title":` + strconv.Quote(fullTitle) + `,"release_date":"2020-10-23"}]}`
	detailsBody := `{"id":659986,"imdb_id":"tt13143964","title":` + strconv.Quote(fullTitle) + `,"release_date":"2020-10-23","overview":"Borat overview."}`

	var mu sync.Mutex
	var detailsPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			writeJSON(w, searchBody)
		case "/3/movie/659986":
			mu.Lock()
			detailsPaths = append(detailsPaths, r.URL.Path)
			mu.Unlock()
			writeJSON(w, detailsBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "Borat Subsequent Moviefilm", 2020, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if !ok {
		t.Fatal("Enrich returned ok=false for a superset official title")
	}
	if en.TMDBID != 659986 {
		t.Errorf("TMDBID = %d, want 659986", en.TMDBID)
	}
	if en.Title != fullTitle || en.Date != "2020-10-23" {
		t.Errorf("metadata = %+v", en)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(detailsPaths) != 1 || detailsPaths[0] != "/3/movie/659986" {
		t.Errorf("details paths = %v, want only /3/movie/659986", detailsPaths)
	}
}

// TMDB's own primary_release_year filter excludes films whose filename year is
// off by one (festival vs wide release). The client must repeat the ladder
// without the year param and let the unchanged ±1 year gate pick the hit.
func TestEnrichFallsBackToSearchWithoutYear(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	withoutYear := `{"results":[
		{"id":111,"title":"Almost Friends","release_date":"2014-01-01"},
		{"id":428496,"title":"Almost Friends","release_date":"2017-11-17"}
	]}`
	detailsBody := `{"id":428496,"imdb_id":"tt428496","title":"Almost Friends","release_date":"2017-11-17","overview":"Almost Friends overview."}`

	var mu sync.Mutex
	var searches []seenRequest
	var detailsPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			mu.Lock()
			searches = append(searches, snapshot(r))
			mu.Unlock()
			if r.URL.Query().Get("primary_release_year") != "" {
				// Year-filtered TMDB search returns nothing for the off-by-one file year.
				writeJSON(w, `{"results":[]}`)
				return
			}
			writeJSON(w, withoutYear)
		case "/3/movie/428496":
			mu.Lock()
			detailsPaths = append(detailsPaths, r.URL.Path)
			mu.Unlock()
			writeJSON(w, detailsBody)
		default:
			// Fetching the 2014 decoy's details would be a wrong-id attach.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "Almost Friends", 2016, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if !ok {
		t.Fatal("Enrich returned ok=false after the yearless retry")
	}
	if en.TMDBID != 428496 || en.Date != "2017-11-17" {
		t.Errorf("Enrich = %+v, want id 428496 date 2017-11-17 (2014 decoy rejected by year gate)", en)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(detailsPaths) != 1 || detailsPaths[0] != "/3/movie/428496" {
		t.Errorf("details paths = %v, want only /3/movie/428496", detailsPaths)
	}
	seenYearless := false
	for _, s := range searches {
		if s.query.Get("primary_release_year") == "" {
			seenYearless = true
		}
	}
	if !seenYearless {
		t.Error("no yearless retry search was made")
	}
	if len(searches) > 2*(1+len(fallbackSearchTitles("Almost Friends"))) {
		t.Errorf("searches = %d, want at most twice the ladder length", len(searches))
	}
}

// Reverse containment must not widen the wrong-id guard: a superset candidate
// whose year is outside the ±1 gate is still rejected.
func TestEnrichRejectsSupersetCandidateWithWrongYear(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	const fullTitle = "Borat Subsequent Moviefilm: Delivery of Prodigious Bribe to American Regime for Make Benefit Once Glorious Nation of Kazakhstan"
	searchBody := `{"results":[{"id":659986,"title":` + strconv.Quote(fullTitle) + `,"release_date":"2020-10-23"}]}`
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
	en, ok, err := c.Enrich(context.Background(), "Borat Subsequent Moviefilm", 2018, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if ok || en != (models.Enriched{}) {
		t.Errorf("Enrich = %+v, %v; want ok=false and zero Enriched for a superset decoy two years off", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if fetched {
		t.Error("client fetched details for a wrong-year superset candidate")
	}
}

// Reverse containment must not accept an unrelated title for a short query.
func TestEnrichRejectsUnrelatedSupersetQuery(t *testing.T) {
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
	en, ok, err := c.Enrich(context.Background(), "Borat Subsequent Moviefilm", 2020, 0, 0, false)
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

func TestFallbackSearchTitle(t *testing.T) {
	cases := []struct{ parsed, want string }{
		{"12 Hour Shift MPEG-2 -BLURANiUM", "12 hour shift"},
		{"28 Years Later HDR10Plus", "28 years later"},
		{"20251008 105743 London Calling", "london calling"},
		{"3754-Family", "family"},
		// Small numbers belong to the title; only long leading numeric prefixes
		// (timestamps and upload ids) are discarded.
		{"101 Dalmatians 2 AVC", "101 dalmatians 2"},
		{"A Christmas Story Christmas X 265-EVO", "a christmas story christmas"},
	}
	for _, tc := range cases {
		t.Run(tc.parsed, func(t *testing.T) {
			if got := fallbackSearchTitle(tc.parsed); got != tc.want {
				t.Errorf("fallbackSearchTitle(%q) = %q, want %q", tc.parsed, got, tc.want)
			}
		})
	}
}

// A noisy parsed title makes TMDB return zero results, so scoring cannot help.
// The retry query must be cleaned, but the hit is still scored against the
// original noisy title and the cache key stays the original one.
func TestEnrichFallsBackToCleanedSearchQuery(t *testing.T) {
	cases := []struct {
		name, noisyTitle, wantQuery, wantTitle, wantDate string
		year, wantID                                     int
	}{
		{"mpeg-2 and group suffix", "12 Hour Shift MPEG-2 -BLURANiUM", "12 hour shift", "12 Hour Shift", "2020-10-09", 2020, 771002},
		{"hdr10plus suffix", "28 Years Later HDR10Plus", "28 years later", "28 Years Later", "2025-06-20", 2025, 853604},
		{"leading upload timestamp", "20251008 105743 London Calling", "london calling", "London Calling", "2025-02-14", 2025, 774410},
		{"numeric hyphen prefix", "3754-Family", "family", "Family", "2018-05-18", 2018, 500211},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configBody := fixture(t, "configuration.json")
			hit := fmt.Sprintf(
				`{"results":[{"id":%d,"title":%q,"release_date":%q,"overview":"Fallback overview.",`+
					`"poster_path":"/fallback-poster.jpg","backdrop_path":"/fallback-backdrop.jpg"}]}`,
				tc.wantID, tc.wantTitle, tc.wantDate)
			details := fmt.Sprintf(
				`{"id":%d,"imdb_id":"tt%07d","title":%q,"release_date":%q,"overview":"Fallback overview."}`,
				tc.wantID, tc.wantID, tc.wantTitle, tc.wantDate)
			wantDetails := fmt.Sprintf("/3/movie/%d", tc.wantID)

			var mu sync.Mutex
			var queries, detailsPaths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/3/configuration":
					writeJSON(w, configBody)
				case "/3/search/movie":
					q := r.URL.Query().Get("query")
					mu.Lock()
					queries = append(queries, q)
					mu.Unlock()
					switch q {
					case tc.noisyTitle:
						// The first, unchanged query finds nothing.
						writeJSON(w, `{"results":[]}`)
					case tc.wantQuery:
						writeJSON(w, hit)
					default:
						t.Errorf("unexpected fallback query %q", q)
						writeJSON(w, `{"results":[]}`)
					}
				default:
					mu.Lock()
					detailsPaths = append(detailsPaths, r.URL.Path)
					mu.Unlock()
					if r.URL.Path == wantDetails {
						writeJSON(w, details)
						return
					}
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
			en, ok, err := c.Enrich(context.Background(), tc.noisyTitle, tc.year, 0, 0, false)
			if err != nil {
				t.Fatalf("Enrich: %v", err)
			}
			if !ok {
				t.Fatalf("Enrich returned ok=false for %q", tc.noisyTitle)
			}
			if en.TMDBID != tc.wantID || en.Title != tc.wantTitle || en.Date != tc.wantDate {
				t.Errorf("Enrich = %+v, want id=%d title=%q date=%q", en, tc.wantID, tc.wantTitle, tc.wantDate)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(queries) != 2 || queries[0] != tc.noisyTitle || queries[1] != tc.wantQuery {
				t.Errorf("search queries = %v, want [%q %q]", queries, tc.noisyTitle, tc.wantQuery)
			}
			if len(detailsPaths) != 1 || detailsPaths[0] != wantDetails {
				t.Errorf("details paths = %v, want only %s", detailsPaths, wantDetails)
			}
		})
	}
}

// The fallback must not weaken the wrong-id guard: an unrelated hit returned
// for the cleaned query is still rejected and never fetched in detail.
func TestEnrichRejectsUnrelatedFallbackHit(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	var mu sync.Mutex
	detailsFetched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			if r.URL.Query().Get("query") == "12 hour shift" {
				writeJSON(w, `{"results":[{"id":999,"title":"A Completely Different Film","release_date":"2020-01-01"}]}`)
				return
			}
			writeJSON(w, `{"results":[]}`)
		default:
			mu.Lock()
			detailsFetched = true
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "12 Hour Shift MPEG-2 -BLURANiUM", 2020, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if ok || en.TMDBID != 0 {
		t.Errorf("Enrich = %+v, %v; want ok=false and no id", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if detailsFetched {
		t.Error("client fetched details for an unrelated fallback hit")
	}
}

// A one-word TMDB title cannot clear the Dice threshold against a parsed name
// carrying an upload timestamp. With a year in hand, containment may accept it
// once the word is present in the query; without a year the old two-token rule
// must still reject it (the WWE/Raw decoys depend on that).
func TestSingleTokenContainedScore(t *testing.T) {
	cases := []struct {
		want, got string
		year      int
		score     float64
	}{
		{"20251028 160401 Relay", "Relay", 2025, 0.75},
		{"20251028 160401 Relay", "Relay", 0, 0},
		{"20251028 160401 Relay", "Unrelated", 2025, 0},
		{"20251028 160401 Relay", "Relay Station", 2025, 0},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%s/%d", tc.want, tc.got, tc.year), func(t *testing.T) {
			if got := singleTokenContainedScore(tc.want, tc.got, tc.year); got != tc.score {
				t.Errorf("singleTokenContainedScore = %.2f, want %.2f", got, tc.score)
			}
		})
	}
}

func TestEnrichAcceptsSingleTokenHitWithYear(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := `{"results":[
		{"id":7001,"title":"Relay Station","release_date":"2025-04-01"},
		{"id":7002,"title":"Relayed","release_date":"2025-05-01"},
		{"id":1088166,"title":"Relay","release_date":"2025-08-22"}
	]}`
	details := `{"id":1088166,"imdb_id":"tt1088166","title":"Relay","release_date":"2025-08-22","overview":"A relay film."}`

	var mu sync.Mutex
	var detailsPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			writeJSON(w, searchBody)
		case "/3/movie/1088166":
			mu.Lock()
			detailsPaths = append(detailsPaths, r.URL.Path)
			mu.Unlock()
			writeJSON(w, details)
		default:
			mu.Lock()
			detailsPaths = append(detailsPaths, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "20251028 160401 Relay", 2025, 0, 0, false)
	if err != nil || !ok {
		t.Fatalf("Enrich = %+v, %v, %v; want the Relay id", en, ok, err)
	}
	if en.TMDBID != 1088166 || en.Title != "Relay" {
		t.Errorf("Enrich = %+v, want id 1088166 title Relay", en)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(detailsPaths) != 1 || detailsPaths[0] != "/3/movie/1088166" {
		t.Errorf("details paths = %v, want only /3/movie/1088166", detailsPaths)
	}
}

func TestEnrichRejectsSingleTokenHitWithMismatchedYear(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := `{"results":[{"id":1088166,"title":"Relay","release_date":"2015-01-01"}]}`
	var mu sync.Mutex
	detailsFetched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			writeJSON(w, searchBody)
		default:
			mu.Lock()
			detailsFetched = true
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "20251028 160401 Relay", 2025, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if ok || en.TMDBID != 0 {
		t.Errorf("Enrich = %+v, %v; want ok=false for the wrong year", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if detailsFetched {
		t.Error("client fetched details for a wrong-year single-token hit")
	}
}

func TestEnrichRejectsSingleTokenHitWithoutYear(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := `{"results":[{"id":1088166,"title":"Relay","release_date":"2025-08-22"}]}`
	var mu sync.Mutex
	detailsFetched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			writeJSON(w, searchBody)
		default:
			mu.Lock()
			detailsFetched = true
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "20251028 160401 Relay", 0, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if ok || en.TMDBID != 0 {
		t.Errorf("Enrich = %+v, %v; want ok=false with no parsed year", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if detailsFetched {
		t.Error("client fetched details for a single-token hit without a year")
	}
}

func TestFallbackSearchTitlesSequence(t *testing.T) {
	cases := []struct {
		title string
		want  []string
	}{
		{
			"6 Underground AAC5 1 LAMA",
			[]string{"6 underground aac5 1 lama", "6 underground aac5 1", "6 underground aac5", "6 underground"},
		},
		{
			"A Hollywood Christmas DDP 5 1 DV H 265 PiRaTeS",
			[]string{"a hollywood christmas ddp 5 1", "a hollywood christmas ddp", "a hollywood christmas", "a hollywood"},
		},
		{"Relay", []string{"relay"}},
	}
	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			got := fallbackSearchTitles(tc.title)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("queries = %q, want %q", got, tc.want)
			}
		})
	}
}

// When the cleaned query still carries codec junk, TMDB returns nothing. The
// client must walk shorter prefixes until one scores against the original
// parsed title, without changing the first query or the cache key.
func TestEnrichFallbackTriesQueryPrefixes(t *testing.T) {
	cases := []struct {
		name, title, hitQuery, wantTitle, wantDate string
		year, wantID                               int
		wantQueries                                []string
	}{
		{
			name:        "six underground via two-token prefix",
			title:       "6 Underground AAC5 1 LAMA",
			year:        2019,
			hitQuery:    "6 underground",
			wantID:      532152,
			wantTitle:   "6 Underground",
			wantDate:    "2019-12-13",
			wantQueries: []string{"6 underground aac5 1 lama", "6 underground aac5 1", "6 underground aac5", "6 underground"},
		},
		{
			name:        "hollywood christmas via three-token prefix",
			title:       "A Hollywood Christmas DDP 5 1 DV H 265 PiRaTeS",
			year:        2022,
			hitQuery:    "a hollywood christmas",
			wantID:      1011985,
			wantTitle:   "A Hollywood Christmas",
			wantDate:    "2022-12-01",
			wantQueries: []string{"a hollywood christmas ddp 5 1", "a hollywood christmas ddp", "a hollywood christmas"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configBody := fixture(t, "configuration.json")
			hit := fmt.Sprintf(
				`{"results":[{"id":%d,"title":%q,"release_date":%q,"overview":"Prefix overview.",`+
					`"poster_path":"/prefix-poster.jpg","backdrop_path":"/prefix-backdrop.jpg"}]}`,
				tc.wantID, tc.wantTitle, tc.wantDate)
			details := fmt.Sprintf(
				`{"id":%d,"imdb_id":"tt%07d","title":%q,"release_date":%q,"overview":"Prefix overview."}`,
				tc.wantID, tc.wantID, tc.wantTitle, tc.wantDate)
			wantDetails := fmt.Sprintf("/3/movie/%d", tc.wantID)

			var mu sync.Mutex
			var queries, detailsPaths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/3/configuration":
					writeJSON(w, configBody)
				case "/3/search/movie":
					q := r.URL.Query().Get("query")
					mu.Lock()
					queries = append(queries, q)
					mu.Unlock()
					if q == tc.hitQuery {
						writeJSON(w, hit)
						return
					}
					writeJSON(w, `{"results":[]}`)
				default:
					mu.Lock()
					detailsPaths = append(detailsPaths, r.URL.Path)
					mu.Unlock()
					if r.URL.Path == wantDetails {
						writeJSON(w, details)
						return
					}
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
			en, ok, err := c.Enrich(context.Background(), tc.title, tc.year, 0, 0, false)
			if err != nil || !ok {
				t.Fatalf("Enrich = %+v, %v, %v; want id %d", en, ok, err, tc.wantID)
			}
			if en.TMDBID != tc.wantID || en.Title != tc.wantTitle || en.Date != tc.wantDate {
				t.Errorf("Enrich = %+v, want id=%d title=%q date=%q", en, tc.wantID, tc.wantTitle, tc.wantDate)
			}

			mu.Lock()
			defer mu.Unlock()
			wantQueries := append([]string{tc.title}, tc.wantQueries...)
			if !reflect.DeepEqual(queries, wantQueries) {
				t.Errorf("search queries = %q, want %q", queries, wantQueries)
			}
			if len(detailsPaths) != 1 || detailsPaths[0] != wantDetails {
				t.Errorf("details paths = %v, want only %s", detailsPaths, wantDetails)
			}
		})
	}
}

// Prefix shortening widens the search, not the acceptance rule: an unrelated
// hit returned for every attempt must still be rejected without a details fetch.
// With a year parsed the ladder runs twice (with year, then without), so the
// cap is twice the ladder length.
func TestEnrichRejectsUnrelatedHitAcrossAllPrefixes(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	var mu sync.Mutex
	searches, detailsFetched := 0, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			mu.Lock()
			searches++
			mu.Unlock()
			writeJSON(w, `{"results":[{"id":999,"title":"A Completely Different Film","release_date":"2022-01-01"}]}`)
		default:
			mu.Lock()
			detailsFetched = true
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "A Hollywood Christmas DDP 5 1 DV H 265 PiRaTeS", 2022, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if ok || en.TMDBID != 0 {
		t.Errorf("Enrich = %+v, %v; want ok=false and no id", en, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if searches != 10 {
		t.Errorf("searches = %d, want 10 (ladder of 5 with year, then without)", searches)
	}
	if detailsFetched {
		t.Error("client fetched details for an unrelated prefix hit")
	}
}

// Diacritic folding must make a release name and its TMDB listing compare
// equal: "Aladdín" parses with an accent while TMDB lists "Aladdin", and the
// file parses as "Finding Ohana" while TMDB lists "Finding ʻOhana".
func TestNormalizeTitleFoldsDiacriticsAndNumerals(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Aladdín", "aladdin"},
		{"Finding ʻOhana", "finding ohana"},
		{"Amélie", "amelie"},
		{"Léon", "leon"},
		{"Stake Land II", "stake land 2"},
		{"Rocky IV", "rocky 4"},
		{"101 Dalmatians 2", "101 dalmatians 2"},
		// Controls: single-character numerals are real words/titles and the
		// WWE alias keys are ASCII, so none of these may change.
		{"V for Vendetta", "v for vendetta"},
		{"X", "x"},
		{"I", "i"},
		{"WWE Monday Night RAW", "wwe monday night raw"},
		{"WWE Friday Night SmackDown", "wwe friday night smackdown"},
		{"WWE NXT", "wwe nxt"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeTitle(tc.in); got != tc.want {
				t.Errorf("normalizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A numeral-mapped title must be an exact normalised match for its TMDB
// listing (score 1.0), not a near miss.
func TestTitleScoreRomanNumeralIsExact(t *testing.T) {
	if got := titleScore("Stake Land 2", "Stake Land II"); got != 1.0 {
		t.Errorf("titleScore = %.4f, want 1.0 for a numeral-mapped exact match", got)
	}
}

// Control for the 3D junk token: if TMDB genuinely carries "3D" in the title
// while the parser stripped it, the Dice overlap still clears the threshold.
func TestTitleScoreGenuine3DTitleStillMatches(t *testing.T) {
	got := titleScore("Onward", "Onward 3D")
	if got < minTitleScore {
		t.Errorf("titleScore = %.4f, want >= %.2f so a genuine 3D title still matches", got, minTitleScore)
	}
	if want := 2.0 / 3.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("titleScore = %.4f, want %.4f", got, want)
	}
}

// Festival-vs-wide drift: when normalised titles match exactly, |year diff|
// of 2 is accepted (The Upside filed 2017, TMDB 2019-01-10; Wake Wood filed
// 2009, TMDB 2011-03-25). Anything else keeps the ±1 gate: exact+diff-5 and
// non-exact+diff-2 are still rejected.
func TestBestHitIndexExactMatchYearToleranceTwo(t *testing.T) {
	type hit struct{ title, date string }
	titleOf := func(h hit) string { return h.title }
	dateOf := func(h hit) string { return h.date }

	cases := []struct {
		name    string
		want    string
		year    int
		results []hit
		index   int
	}{
		{
			name:    "exact plus two accepted",
			want:    "The Upside",
			year:    2017,
			results: []hit{{title: "The Upside", date: "2019-01-10"}},
			index:   0,
		},
		{
			name:    "exact minus two accepted",
			want:    "The Upside",
			year:    2021,
			results: []hit{{title: "The Upside", date: "2019-01-10"}},
			index:   0,
		},
		{
			name:    "exact plus five rejected",
			want:    "The Upside",
			year:    2014,
			results: []hit{{title: "The Upside", date: "2019-01-10"}},
			index:   -1,
		},
		{
			name:    "tied exact matches prefer the nearer year",
			want:    "Almost Friends",
			year:    2016,
			results: []hit{{title: "Almost Friends", date: "2014-01-01"}, {title: "Almost Friends", date: "2017-11-17"}},
			index:   1,
		},
		{
			name:    "non-exact plus two rejected",
			want:    "Train Dreams H 265-XEBEC",
			year:    2023,
			results: []hit{{title: "Train Dreams", date: "2025-11-21"}},
			index:   -1,
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

// End-to-end diacritic cases over HTTP fixtures: the parsed release name must
// resolve to the TMDB listing despite the accent/okina difference.
func TestEnrichResolvesDiacriticTitles(t *testing.T) {
	cases := []struct {
		name      string
		title     string
		year      int
		hitID     int
		hitTitle  string
		hitDate   string
		wantQuery string
	}{
		{
			name:      "accented release against plain TMDB title",
			title:     "Aladdín",
			year:      2019,
			hitID:     420817,
			hitTitle:  "Aladdin",
			hitDate:   "2019-05-22",
			wantQuery: "Aladdín",
		},
		{
			name:      "plain release against okina TMDB title",
			title:     "Finding Ohana",
			year:      2021,
			hitID:     642378,
			hitTitle:  "Finding ʻOhana",
			hitDate:   "2021-01-29",
			wantQuery: "Finding Ohana",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configBody := fixture(t, "configuration.json")
			searchBody := fmt.Sprintf(
				`{"results":[{"id":%d,"title":%q,"release_date":%q,"overview":"Diacritic overview.",`+
					`"poster_path":"/p.jpg","backdrop_path":"/b.jpg"}]}`,
				tc.hitID, tc.hitTitle, tc.hitDate)
			detailsBody := fmt.Sprintf(
				`{"id":%d,"imdb_id":"tt%07d","title":%q,"release_date":%q,"overview":"Diacritic overview."}`,
				tc.hitID, tc.hitID, tc.hitTitle, tc.hitDate)
			wantDetails := fmt.Sprintf("/3/movie/%d", tc.hitID)

			var mu sync.Mutex
			var queries, detailsPaths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/3/configuration":
					writeJSON(w, configBody)
				case "/3/search/movie":
					mu.Lock()
					queries = append(queries, r.URL.Query().Get("query"))
					mu.Unlock()
					writeJSON(w, searchBody)
				default:
					mu.Lock()
					detailsPaths = append(detailsPaths, r.URL.Path)
					mu.Unlock()
					if r.URL.Path == wantDetails {
						writeJSON(w, detailsBody)
						return
					}
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
				t.Fatalf("Enrich returned ok=false for %q; want TMDB id %d", tc.title, tc.hitID)
			}
			if en.TMDBID != tc.hitID || en.Title != tc.hitTitle || en.Date != tc.hitDate {
				t.Errorf("Enrich = %+v, want id=%d title=%q date=%q", en, tc.hitID, tc.hitTitle, tc.hitDate)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(queries) < 1 || queries[0] != tc.wantQuery {
				t.Errorf("first search query = %q, want %q (the parsed title, unchanged)", queries, tc.wantQuery)
			}
			if len(detailsPaths) != 1 || detailsPaths[0] != wantDetails {
				t.Errorf("details paths = %v, want only %s", detailsPaths, wantDetails)
			}
		})
	}
}

// The numeral map makes "Stake Land 2" an exact normalised match for a
// II-titled entry, outranking the older "Stake Land" decoy (which the year
// gate rejects). Fixture note: live TMDB lists the 2016 sequel under the
// canonical title "The Stakelander" (id 410988, matched via its "Stake Land
// II" alternative title); the mapping covers II-titled entries exactly.
func TestEnrichResolvesRomanNumeralSequel(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	searchBody := `{"results":[
		{"id":52015,"title":"Stake Land","release_date":"2010-09-30"},
		{"id":410988,"title":"Stake Land II","release_date":"2016-10-14"}
	]}`
	detailsBody := `{"id":410988,"imdb_id":"tt0410988","title":"Stake Land II","release_date":"2016-10-14","overview":"Sequel overview."}`

	var mu sync.Mutex
	var detailsPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			writeJSON(w, searchBody)
		case "/3/movie/410988":
			mu.Lock()
			detailsPaths = append(detailsPaths, r.URL.Path)
			mu.Unlock()
			writeJSON(w, detailsBody)
		default:
			// Fetching the 2010 decoy's details would be a wrong-id attach.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "Stake Land 2", 2016, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if !ok {
		t.Fatal("Enrich returned ok=false for Stake Land 2; want TMDB id 410988")
	}
	if en.TMDBID != 410988 {
		t.Errorf("TMDBID = %d, want 410988 (the 2010 decoy must not win)", en.TMDBID)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(detailsPaths) != 1 || detailsPaths[0] != "/3/movie/410988" {
		t.Errorf("details paths = %v, want only /3/movie/410988", detailsPaths)
	}
}

// "Kirpi Sonic 2" is the Turkish release title of the 2022 film
// "Sonic the Hedgehog 2" (TMDB id 675353, Turkish title "Kirpi Sonic 2",
// verified live). The alias rewrites the search so the exact title scores 1.0.
func TestEnrichResolvesMovieAliasKirpiSonic2(t *testing.T) {
	configBody := fixture(t, "configuration.json")
	hit := `{"results":[{"id":675353,"title":"Sonic the Hedgehog 2","release_date":"2022-03-30",` +
		`"overview":"Sonic overview.","poster_path":"/p.jpg","backdrop_path":"/b.jpg"}]}`
	details := `{"id":675353,"imdb_id":"tt12412888","title":"Sonic the Hedgehog 2",` +
		`"release_date":"2022-03-30","overview":"Sonic overview."}`

	var mu sync.Mutex
	var queries, detailsPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/configuration":
			writeJSON(w, configBody)
		case "/3/search/movie":
			q := r.URL.Query().Get("query")
			mu.Lock()
			queries = append(queries, q)
			mu.Unlock()
			if q == "Sonic the Hedgehog 2" {
				writeJSON(w, hit)
				return
			}
			writeJSON(w, `{"results":[]}`)
		case "/3/movie/675353":
			mu.Lock()
			detailsPaths = append(detailsPaths, r.URL.Path)
			mu.Unlock()
			writeJSON(w, details)
		default:
			mu.Lock()
			detailsPaths = append(detailsPaths, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, testToken, "", newTestStore(t))
	en, ok, err := c.Enrich(context.Background(), "Kirpi Sonic 2", 2022, 0, 0, false)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if !ok {
		t.Fatal("Enrich returned ok=false for Kirpi Sonic 2; want TMDB id 675353")
	}
	if en.TMDBID != 675353 || en.Title != "Sonic the Hedgehog 2" || en.Date != "2022-03-30" {
		t.Errorf("Enrich = %+v, want id=675353 title Sonic the Hedgehog 2 date 2022-03-30", en)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(queries) < 1 || queries[0] != "Sonic the Hedgehog 2" {
		t.Errorf("first search query = %q, want the alias target", queries)
	}
	if len(detailsPaths) != 1 || detailsPaths[0] != "/3/movie/675353" {
		t.Errorf("details paths = %v, want only /3/movie/675353", detailsPaths)
	}
}
