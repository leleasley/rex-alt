package sync

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/leleasley/rex-alt/internal/altmount"
	"github.com/leleasley/rex-alt/internal/config"
	"github.com/leleasley/rex-alt/internal/models"
	"github.com/leleasley/rex-alt/internal/probe"
	"github.com/leleasley/rex-alt/internal/rex"
	"github.com/leleasley/rex-alt/internal/store"
	"github.com/leleasley/rex-alt/internal/tmdb"
)

const (
	davUser = "usenet"
	davPass = "dav-secret"
	rexUser = "rex-user"
	rexPass = "rex-secret"

	// validProbeJSON satisfies probe.Validate: a known codec, a positive
	// duration, and enough non-zero bytes at distinct offsets from the DAV
	// sample reader below.
	validProbeJSON = `{"format":{"duration":"120.000000","size":"4096"},"streams":[{"codec_type":"video","codec_name":"h264","duration":"120.000000"}]}`
	testDigest     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var (
	movieAlpha = "/webdav/movies/Alpha.Rel/Alpha.Movie.2022.1080p.BluRay.x264.mkv"
	movieBeta  = "/webdav/movies/Beta.Rel/Beta.Movie.2021.720p.WEB.x264.mkv"
	movieGamma = "/webdav/movies/Gamma.Rel/Gamma.Movie.2020.1080p.mkv"
	episodeOne = "/webdav/tv/Some.Rel/Some.Show.S01E02.720p.mkv"
)

// davState is the mutable state behind the Altmount test double. PROPFIND
// answers come from files; range GETs answer from sample.
type davState struct {
	mu       stdsync.Mutex
	files    map[string][]string
	sample   []byte
	getCount int
}

func newDAV(t *testing.T, state *davState) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != davUser || pass != davPass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			if r.Header.Get("Range") == "" {
				t.Errorf("sample GET without a Range header")
			}
			state.mu.Lock()
			state.getCount++
			sample := append([]byte(nil), state.sample...)
			state.mu.Unlock()
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(sample)
			return
		}
		state.mu.Lock()
		category := strings.Trim(strings.TrimPrefix(r.URL.Path, "/webdav/"), "/")
		files := append([]string(nil), state.files[category]...)
		state.mu.Unlock()
		var body strings.Builder
		body.WriteString(`<d:multistatus xmlns:d="DAV:">`)
		for _, href := range files {
			body.WriteString(`<d:response><d:href>` + href + `</d:href></d:response>`)
		}
		body.WriteString(`</d:multistatus>`)
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(body.String()))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (d *davState) setFiles(category string, files ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files[category] = append([]string(nil), files...)
}

func (d *davState) setSample(sample []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sample = append([]byte(nil), sample...)
}

func (d *davState) rangeGets() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.getCount
}

// rexState records every REX call so tests can assert registration,
// batching and the v1 no-delete rule without contacting a real server.
type rexState struct {
	mu            stdsync.Mutex
	server        *httptest.Server
	features      []string
	registrations int
	registration  struct {
		URL         string          `json:"url"`
		User        string          `json:"user"`
		Password    string          `json:"password"`
		Trash       *bool           `json:"trash"`
		Directories []rex.Directory `json:"directories"`
	}
	pushes  []pushedFeed
	deletes int
}

type pushedFeed struct {
	Items []rex.Item `json:"items"`
}

func newREX(t *testing.T, features []string) *rexState {
	t.Helper()
	state := &rexState{features: append([]string(nil), features...)}
	state.server = httptest.NewServer(http.HandlerFunc(state.serve))
	t.Cleanup(state.server.Close)
	return state
}

func (r *rexState) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case req.Method == http.MethodPut && req.URL.Path == "/rex/sources/rex-alt":
		r.registrations++
		if err := json.NewDecoder(req.Body).Decode(&r.registration); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		features, _ := json.Marshal(r.features)
		_, _ = fmt.Fprintf(w, `{"name":"rex-alt","features":%s}`, features)
	case req.Method == http.MethodPost && req.URL.Path == "/rex/feed/rex-alt/items":
		var feed pushedFeed
		if err := json.NewDecoder(req.Body).Decode(&feed); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.pushes = append(r.pushes, feed)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"created":%d}`, len(feed.Items))
	case req.Method == http.MethodDelete && req.URL.Path == "/rex/feed/rex-alt/items":
		r.deletes++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deleted":0}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (r *rexState) snapshot() (int, struct {
	URL         string          `json:"url"`
	User        string          `json:"user"`
	Password    string          `json:"password"`
	Trash       *bool           `json:"trash"`
	Directories []rex.Directory `json:"directories"`
}, []pushedFeed, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pushes := make([]pushedFeed, len(r.pushes))
	copy(pushes, r.pushes)
	return r.registrations, r.registration, pushes, r.deletes
}

func (r *rexState) clearPushes() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pushes = nil
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func testConfig(davURL string) config.Config {
	return config.Config{
		RexSourceName:     "rex-alt",
		RexSourceUser:     rexUser,
		RexSourcePassword: rexPass,
		PublicURL:         davURL + "/webdav/",
		AltmountWebDAVURL: davURL + "/webdav/",
		AltmountUser:      davUser,
		AltmountPassword:  davPass,
		TMDBReadToken:     "test-token",
		ProbeWorkers:      2,
		ProbeTimeout:      45 * time.Second,
		HTTPTimeout:       5 * time.Second,
		SyncMaxMovies:     1,
		PushBatch:         100,
	}
}

func newService(t *testing.T, cfg config.Config, st *store.Store, davURL string, rx *rexState, prb ProbeFunc) *Service {
	t.Helper()
	am := altmount.NewClient(davURL+"/webdav/", davUser, davPass, nil)
	tm := tmdb.NewClient(cfg.TMDBReadToken, "", st, nil)
	client := rex.NewClient(rx.server.URL, cfg.RexSourceName, cfg.RexSourceUser, cfg.RexSourcePassword, cfg.PushBatch)
	return New(cfg, st, am, tm, client, prb)
}

// primeTMDB writes the exact cache key tmdb.Enrich looks up, so these tests
// exercise the cache path and never reach the real TMDB API.
func primeTMDB(t *testing.T, st *store.Store, key string, id int, title string) {
	t.Helper()
	if err := st.SetTMDB(key, models.Enriched{TMDBID: id, Title: title, Summary: "A summary", Date: "2022-03-11"}); err != nil {
		t.Fatalf("prime TMDB cache: %v", err)
	}
}

func primeAll(t *testing.T, st *store.Store) {
	t.Helper()
	primeTMDB(t, st, "alpha movie|2022|false|0|0", 101, "Alpha Movie")
	primeTMDB(t, st, "beta movie|2021|false|0|0", 102, "Beta Movie")
	primeTMDB(t, st, "gamma movie|2020|false|0|0", 103, "Gamma Movie")
	primeTMDB(t, st, "some show|0|true|1|2", 201, "Some Show S01E02")
}

func goodProbe(context.Context, models.Entry) (probe.Result, error) {
	return probe.Result{JSON: json.RawMessage(validProbeJSON), Digest: testDigest}, nil
}

func TestSyncOncePushesProbedItems(t *testing.T) {
	dav := &davState{files: map[string][]string{}, sample: []byte{1, 2, 3, 4}}
	dav.setFiles("movies", movieAlpha)
	dav.setFiles("tv")
	server := newDAV(t, dav)
	st := newStore(t)
	primeAll(t, st)
	rx := newREX(t, []string{"probe_digest"})
	cfg := testConfig(server.URL)
	svc := newService(t, cfg, st, server.URL, rx, goodProbe)

	stats, err := svc.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if stats.Scanned != 1 || stats.Added != 1 || stats.Probed != 1 || stats.Pushed != 1 || stats.Held != 0 || stats.Rejected != 0 {
		t.Errorf("stats = %+v, want Scanned=1 Added=1 Probed=1 Pushed=1 Held=0 Rejected=0", stats)
	}

	registrations, registration, pushes, deletes := rx.snapshot()
	if registrations != 1 {
		t.Errorf("got %d registrations, want 1", registrations)
	}
	wantDirs := []rex.Directory{
		{Name: "movies", Movies: "Altmount Movies"},
		{Name: "tv", Shows: "Altmount TV"},
	}
	if fmt.Sprint(registration.Directories) != fmt.Sprint(wantDirs) {
		t.Errorf("directories = %+v, want %+v (name must match the first path segment)", registration.Directories, wantDirs)
	}
	if registration.Trash == nil || *registration.Trash {
		t.Errorf("trash = %v, want false", registration.Trash)
	}
	if registration.User != rexUser || registration.Password != rexPass {
		t.Error("registration credentials are wrong")
	}
	if len(pushes) != 1 || len(pushes[0].Items) != 1 {
		t.Fatalf("got %d pushes with %v items, want 1 push of 1 item", len(pushes), pushSizes(pushes))
	}
	item := pushes[0].Items[0]
	if len(item.MediaInfo) == 0 || !strings.Contains(string(item.MediaInfo), "codec_name") {
		t.Errorf("item media_info = %q, want full probe JSON", item.MediaInfo)
	}
	if item.Kind != "movie" || !strings.HasPrefix(item.Path, "movies/") {
		t.Errorf("item kind/path = %q/%q, want movie under movies/", item.Kind, item.Path)
	}
	if item.Display == nil || item.Display.Title != "Alpha Movie" {
		t.Errorf("item display = %+v, want enriched title", item.Display)
	}
	if deletes != 0 {
		t.Errorf("got %d REX deletes, want 0 in v1", deletes)
	}

	entries, err := st.Entries()
	if err != nil || len(entries) != 1 {
		t.Errorf("store entries = %d (%v), want 1", len(entries), err)
	}
	if _, _, ok := st.Probe(entries[0].ID); !ok {
		t.Error("successful probe was not cached")
	}
}

func pushSizes(pushes []pushedFeed) []int {
	out := make([]int, len(pushes))
	for i, feed := range pushes {
		out[i] = len(feed.Items)
	}
	return out
}

func TestSyncOnceRespectsLimitsAndHoldsUnprobed(t *testing.T) {
	dav := &davState{files: map[string][]string{}, sample: []byte{1, 2, 3, 4}}
	dav.setFiles("movies", movieAlpha, movieBeta)
	dav.setFiles("tv")
	server := newDAV(t, dav)
	st := newStore(t)
	primeAll(t, st)
	rx := newREX(t, nil)
	cfg := testConfig(server.URL) // SyncMaxMovies=1: only Alpha is processed.

	var probeErr error
	calls := 0
	prb := func(ctx context.Context, e models.Entry) (probe.Result, error) {
		calls++
		if probeErr != nil {
			return probe.Result{}, probeErr
		}
		return goodProbe(ctx, e)
	}
	svc := newService(t, cfg, st, server.URL, rx, prb)

	probeErr = fmt.Errorf("cold file failed")
	stats, err := svc.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce with failed probe: %v", err)
	}
	if stats.Scanned != 2 || stats.Probed != 1 || stats.Pushed != 0 || stats.Held != 1 {
		t.Errorf("stats = %+v, want Scanned=2 Probed=1 Pushed=0 Held=1", stats)
	}
	if calls != 1 {
		t.Errorf("got %d probe calls, want 1 (SYNC_MAX_MOVIES=1)", calls)
	}
	if _, _, _, deletes := rx.snapshot(); deletes != 0 {
		t.Error("v1 must never call REX delete")
	}
	if _, _, pushes, _ := rx.snapshot(); len(pushes) != 0 {
		t.Errorf("got %d pushes for an unprobed entry, want 0", len(pushes))
	}
	entries, err := st.Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("held entry was not kept for next sync: entries=%d (%v)", len(entries), err)
	}
	if _, _, ok := st.Probe(entries[0].ID); ok {
		t.Error("failed probe must not be cached")
	}

	// The held entry is retried on the next sync and now succeeds.
	probeErr = nil
	stats, err = svc.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("second SyncOnce: %v", err)
	}
	if stats.Added != 0 || stats.Pushed != 1 || stats.Held != 0 {
		t.Errorf("second stats = %+v, want Added=0 Pushed=1 Held=0", stats)
	}
	if _, _, pushes, _ := rx.snapshot(); len(pushes) != 1 || len(pushes[0].Items) != 1 {
		t.Errorf("second pushes = %v, want one item", pushSizes(pushes))
	}
}

func TestSyncOnceUsesProbeDigestOnSecondRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []string
		want     string
	}{
		{"supported", []string{"probe_digest"}, testDigest},
		{"unsupported", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dav := &davState{files: map[string][]string{}, sample: []byte{1, 2, 3, 4}}
			dav.setFiles("movies", movieAlpha)
			dav.setFiles("tv")
			server := newDAV(t, dav)
			st := newStore(t)
			primeAll(t, st)
			rx := newREX(t, tc.features)
			svc := newService(t, testConfig(server.URL), st, server.URL, rx, goodProbe)

			if _, err := svc.SyncOnce(context.Background()); err != nil {
				t.Fatalf("first SyncOnce: %v", err)
			}
			_, _, firstPushes, _ := rx.snapshot()
			if len(firstPushes) != 1 || len(firstPushes[0].Items) != 1 || firstPushes[0].Items[0].Probe != testDigest {
				t.Errorf("first push = %v, want one item carrying the digest alongside media_info", pushSizes(firstPushes))
			}
			rx.clearPushes()
			stats, err := svc.SyncOnce(context.Background())
			if err != nil {
				t.Fatalf("second SyncOnce: %v", err)
			}
			if stats.Probed != 0 {
				t.Errorf("second run probed %d items, want 0 (cache hit)", stats.Probed)
			}
			_, _, pushes, _ := rx.snapshot()
			if len(pushes) != 1 || len(pushes[0].Items) != 1 {
				t.Fatalf("second pushes = %v, want one item", pushSizes(pushes))
			}
			if got := pushes[0].Items[0].Probe; got != tc.want {
				t.Errorf("recurring item probe = %q, want %q", got, tc.want)
			}
			if len(pushes[0].Items[0].MediaInfo) == 0 {
				t.Error("recurring push lost its media_info")
			}
		})
	}
}

func TestSyncOnceHoldsZeroFilledProbe(t *testing.T) {
	dav := &davState{
		files:  map[string][]string{},
		sample: []byte{0, 0, 0, 0}, // Altmount failure masking: zero-filled bytes.
	}
	dav.setFiles("movies", movieAlpha)
	dav.setFiles("tv")
	server := newDAV(t, dav)
	st := newStore(t)
	primeAll(t, st)
	rx := newREX(t, nil)
	svc := newService(t, testConfig(server.URL), st, server.URL, rx, goodProbe)

	stats, err := svc.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if stats.Held != 1 || stats.Pushed != 0 {
		t.Errorf("stats = %+v, want Held=1 Pushed=0 for an invalid probe", stats)
	}
	if dav.rangeGets() < 2 {
		t.Errorf("got %d range GETs, want at least 2 distinct samples", dav.rangeGets())
	}
	entries, err := st.Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entry should remain held for next sync: %d (%v)", len(entries), err)
	}
	if _, _, ok := st.Probe(entries[0].ID); ok {
		t.Error("zero-filled probe must not be cached")
	}
	if _, _, pushes, _ := rx.snapshot(); len(pushes) != 0 {
		t.Errorf("zero-filled probe was pushed: %v", pushSizes(pushes))
	}

	// Once the bytes are real, the same held entry is probed, cached and pushed.
	dav.setSample([]byte{9, 8, 7, 6})
	stats, err = svc.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("second SyncOnce: %v", err)
	}
	if stats.Held != 0 || stats.Pushed != 1 {
		t.Errorf("second stats = %+v, want Held=0 Pushed=1", stats)
	}
	if _, _, ok := st.Probe(entries[0].ID); !ok {
		t.Error("validated probe was not cached on retry")
	}
}

func TestSyncOnceLeavesRemovalsForManualCleanup(t *testing.T) {
	dav := &davState{files: map[string][]string{}, sample: []byte{1, 2, 3, 4}}
	dav.setFiles("movies", movieAlpha, movieBeta)
	dav.setFiles("tv")
	server := newDAV(t, dav)
	st := newStore(t)
	primeAll(t, st)
	rx := newREX(t, nil)
	cfg := testConfig(server.URL)
	cfg.SyncMaxMovies = 0 // unlimited
	svc := newService(t, cfg, st, server.URL, rx, goodProbe)

	if stats, err := svc.SyncOnce(context.Background()); err != nil || stats.Pushed != 2 {
		t.Fatalf("first SyncOnce = %+v, %v; want 2 pushed", stats, err)
	}
	dav.setFiles("movies", movieAlpha)
	if stats, err := svc.SyncOnce(context.Background()); err != nil || stats.Pushed != 1 {
		t.Fatalf("second SyncOnce = %+v, %v; want 1 pushed", stats, err)
	}
	_, _, pushes, deletes := rx.snapshot()
	if deletes != 0 {
		t.Errorf("got %d REX deletes; v1 leaves stale rows for manual cleanup", deletes)
	}
	if got := pushSizes(pushes); fmt.Sprint(got) != "[2 1]" {
		t.Errorf("push sizes = %v, want [2 1]", got)
	}
	entries, err := st.Entries()
	if err != nil || len(entries) != 1 {
		t.Errorf("stale store row was not pruned: entries=%d (%v)", len(entries), err)
	}
}

func TestSyncOnceBatchesPushes(t *testing.T) {
	dav := &davState{files: map[string][]string{}, sample: []byte{1, 2, 3, 4}}
	dav.setFiles("movies", movieAlpha, movieBeta, movieGamma)
	dav.setFiles("tv", episodeOne)
	server := newDAV(t, dav)
	st := newStore(t)
	primeAll(t, st)
	rx := newREX(t, nil)
	cfg := testConfig(server.URL)
	cfg.SyncMaxMovies = 0
	cfg.PushBatch = 2
	svc := newService(t, cfg, st, server.URL, rx, goodProbe)

	stats, err := svc.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if stats.Scanned != 4 || stats.Pushed != 4 {
		t.Errorf("stats = %+v, want Scanned=4 Pushed=4", stats)
	}
	_, _, pushes, _ := rx.snapshot()
	if got := pushSizes(pushes); fmt.Sprint(got) != "[2 2]" {
		t.Errorf("push sizes = %v, want [2 2]", got)
	}
	// The episode must be mapped as an episode, with season and episode set.
	for _, feed := range pushes {
		for _, item := range feed.Items {
			if item.Path == strings.TrimPrefix(episodeOne, "/webdav/") {
				if item.Kind != "episode" || item.Season == nil || *item.Season != 1 || item.Episode == nil || *item.Episode != 2 {
					t.Errorf("episode item = %+v, want kind=episode S01E02", item)
				}
				return
			}
		}
	}
	t.Error("episode item was not pushed")
}

func TestSyncOnceRefusesTruncatedCatalogue(t *testing.T) {
	dav := &davState{files: map[string][]string{}, sample: []byte{1, 2, 3, 4}}
	dav.setFiles("movies")
	dav.setFiles("tv")
	server := newDAV(t, dav)
	st := newStore(t)
	primeAll(t, st)
	rx := newREX(t, nil)
	svc := newService(t, testConfig(server.URL), st, server.URL, rx, goodProbe)

	if _, err := svc.SyncOnce(context.Background()); err == nil {
		t.Fatal("SyncOnce with an empty catalogue returned no error; Diff guard must be respected")
	}
	if _, _, pushes, deletes := rx.snapshot(); len(pushes) != 0 || deletes != 0 {
		t.Errorf("empty catalogue produced %d pushes and %d deletes", len(pushes), deletes)
	}
	entries, err := st.Entries()
	if err != nil || len(entries) != 0 {
		t.Errorf("store changed after refused sync: %d entries (%v)", len(entries), err)
	}
}

func TestSyncOnceAggregatesREXResult(t *testing.T) {
	dav := &davState{files: map[string][]string{}, sample: []byte{1, 2, 3, 4}}
	dav.setFiles("movies", movieAlpha)
	dav.setFiles("tv")
	server := newDAV(t, dav)
	st := newStore(t)
	primeAll(t, st)
	rx := newREX(t, nil)
	// A REX that holds one item and rejects another must be reflected in Stats.
	rx.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"features":[]}`))
		case r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"created":1,"held":1,"rejected":[{"id":"abc","reason":"bad"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	svc := newService(t, testConfig(server.URL), st, server.URL, rx, goodProbe)

	stats, err := svc.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if stats.Held != 1 || stats.Rejected != 1 {
		t.Errorf("stats = %+v, want Held=1 Rejected=1 from the REX result", stats)
	}
	if _, _, _, deletes := rx.snapshot(); deletes != 0 {
		t.Error("v1 must never call REX delete")
	}
	// Hold/reject never removes the entry: it is retried next sync.
	entries, err := st.Entries()
	if err != nil || len(entries) != 1 {
		t.Errorf("held entry was removed: %d (%v)", len(entries), err)
	}
}

func TestItemIDIsStableAcrossSyncs(t *testing.T) {
	dav := &davState{files: map[string][]string{}, sample: []byte{1, 2, 3, 4}}
	dav.setFiles("movies", movieAlpha)
	dav.setFiles("tv")
	server := newDAV(t, dav)
	st := newStore(t)
	primeAll(t, st)
	rx := newREX(t, nil)
	svc := newService(t, testConfig(server.URL), st, server.URL, rx, goodProbe)

	for run := 0; run < 2; run++ {
		if _, err := svc.SyncOnce(context.Background()); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	_, _, pushes, _ := rx.snapshot()
	if len(pushes) != 2 {
		t.Fatalf("got %d pushes, want 2", len(pushes))
	}
	first := pushes[0].Items[0]
	second := pushes[1].Items[0]
	if first.ID != second.ID || first.ID == "" {
		t.Errorf("item ids %q and %q are not stable", first.ID, second.ID)
	}
	if len(first.ID) != 24 {
		t.Errorf("item id %q has length %d, want 24", first.ID, len(first.ID))
	}
	if _, err := hex.DecodeString(first.ID); err != nil {
		t.Errorf("item id %q is not hex: %v", first.ID, err)
	}
}
