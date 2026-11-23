// Package sync drives one pass of the Altmount → TMDB → probe → REX pipeline.
// It owns no media bytes: REX fetches them directly from Altmount's WebDAV
// share. Only ids, paths and probe/TMDB metadata are persisted in SQLite.
package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	stdsync "sync"
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
	// sampleBytes bounds each ranged read used to reject zero-filled files.
	sampleBytes = 4096
	maxBatch    = 100
)

// ProbeFunc produces the full ffprobe document for one entry. Implementations
// must reject bad data before returning it: SyncOnce validates every result
// again before it is cached or pushed, so a zero-filled or truncated probe is
// held for the next sync instead of becoming an invisible REX row.
type ProbeFunc func(context.Context, models.Entry) (probe.Result, error)

// Stats is the outcome of one SyncOnce pass. Scanned counts the files listed
// before SYNC_MAX_* is applied; Added counts new ids from the catalogue diff;
// Probed counts fresh probe attempts (cache hits are not re-probed); Pushed
// counts items handed to REX; Held counts entries without a usable probe plus
// items REX held; Rejected counts REX rejections.
type Stats struct {
	Scanned  int
	Added    int
	Probed   int
	Pushed   int
	Held     int
	Rejected int
}

// Service is one registered REX source backed by Altmount, TMDB and SQLite.
type Service struct {
	cfg  config.Config
	db   *store.Store
	dav  *altmount.Client
	mdb  *tmdb.Client
	feed *rex.Client
	prb  ProbeFunc
	http *http.Client

	runMu      stdsync.Mutex // one sync pass at a time (API trigger + timer)
	regMu      stdsync.Mutex // registration happens exactly once, on first success
	registered bool
	features   rex.Features
}

// New assembles a sync service. A nil ProbeFunc is replaced by NewProbeFunc,
// which runs ffprobe over HTTP Range and validates the result with ranged
// sample reads. Tests inject a deterministic ProbeFunc instead.
func New(cfg config.Config, st *store.Store, am *altmount.Client, tm *tmdb.Client, rx *rex.Client, prb ProbeFunc) *Service {
	if prb == nil {
		prb = NewProbeFunc(cfg, nil)
	}
	return &Service{
		cfg:  cfg,
		db:   st,
		dav:  am,
		mdb:  tm,
		feed: rx,
		prb:  prb,
		http: &http.Client{Timeout: httpTimeout(cfg.HTTPTimeout)},
	}
}

// NewProbeFunc is the production ProbeFunc: it calls probe.Probe with a Range
// request and Basic auth, then probe.Validate with a ranged sample reader that
// reads 2+ distinct non-zero offsets from the same Altmount URL. An invalid
// result is returned as an error so the caller never caches or pushes it.
func NewProbeFunc(cfg config.Config, h *http.Client) ProbeFunc {
	if h == nil {
		h = &http.Client{Timeout: httpTimeout(cfg.HTTPTimeout)}
	}
	return func(ctx context.Context, e models.Entry) (probe.Result, error) {
		result, err := probe.Probe(ctx,
			webdavURL(cfg.AltmountWebDAVURL, e.Path),
			cfg.AltmountUser, cfg.AltmountPassword, "",
			cfg.ProbeTimeout, int(cfg.ProbeTimeout.Microseconds()))
		if err != nil {
			return probe.Result{}, err
		}
		if err := probe.Validate(result, rangedSample(h, ctx, cfg, e)); err != nil {
			return probe.Result{}, err
		}
		return result, nil
	}
}

// SyncOnce enumerates both categories, applies the configured caps, diffs the
// catalogue, enriches and probes entries, maps them to REX items and pushes
// them in batches. Entries with no valid probe, and items REX holds or
// rejects, remain in the store for the next pass. Removals never call
// rex.Delete in v1: stale REX rows are left for manual cleanup.
func (s *Service) SyncOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	s.runMu.Lock()
	defer s.runMu.Unlock()

	if err := s.register(ctx); err != nil {
		return stats, err
	}

	movies, err := s.dav.List(ctx, "movies")
	if err != nil {
		return stats, fmt.Errorf("sync: list movies: %w", err)
	}
	shows, err := s.dav.List(ctx, "tv")
	if err != nil {
		return stats, fmt.Errorf("sync: list tv: %w", err)
	}
	stats.Scanned = len(movies) + len(shows)

	movies = limitEntries(movies, s.cfg.SyncMaxMovies)
	shows = limitEntries(shows, s.cfg.SyncMaxSeries)
	next := make([]models.Entry, 0, len(movies)+len(shows))
	next = append(next, movies...)
	next = append(next, shows...)
	// The enumerator leaves season/episode at zero; the release-name parser
	// fills them before the diff so a stable id is not seen as "changed" on
	// every later sync.
	for i := range next {
		_, _, season, episode := tmdb.ParseRelease(next[i].File, next[i].Category)
		next[i].Season, next[i].Episode = season, episode
	}

	old, err := s.db.Entries()
	if err != nil {
		return stats, fmt.Errorf("sync: read catalogue: %w", err)
	}
	added, _, removed, err := altmount.Diff(old, next)
	if err != nil {
		return stats, fmt.Errorf("sync: diff catalogue: %w", err)
	}
	stats.Added = len(added)

	// Persist the catalogue before probing: a held entry must still be known
	// on the next pass so it is retried rather than treated as a new file.
	if err := s.db.UpsertEntries(next); err != nil {
		return stats, fmt.Errorf("sync: upsert entries: %w", err)
	}
	if len(removed) > 0 {
		ids := make([]string, 0, len(next))
		for _, e := range next {
			ids = append(ids, e.ID)
		}
		if _, err := s.db.RemoveEntriesNotIn(ids); err != nil {
			return stats, fmt.Errorf("sync: prune stale entries: %w", err)
		}
	}

	// TMDB lookups are cached in SQLite by the client; a failed or ambiguous
	// match yields empty metadata but must not stop the push.
	enriched := make([]models.Enriched, len(next))
	for i, e := range next {
		enriched[i] = s.enrich(ctx, e)
	}

	results := make([]probe.Result, len(next))
	valid := make([]bool, len(next))
	fresh := make([]bool, len(next))
	misses := make([]int, 0, len(next))
	for i, e := range next {
		body, digest, ok := s.db.Probe(e.ID)
		if ok && len(body) > 0 {
			results[i] = probe.Result{JSON: json.RawMessage(body), Digest: digest}
			valid[i] = true
			continue
		}
		misses = append(misses, i)
	}
	s.probeMissing(ctx, next, misses, results, valid, fresh)
	stats.Probed = len(misses)
	// A probe is cached only after validation has accepted it.
	for _, i := range misses {
		if !valid[i] {
			continue
		}
		if err := s.db.SetProbe(next[i].ID, string(results[i].JSON), results[i].Digest); err != nil {
			return stats, fmt.Errorf("sync: cache probe: %w", err)
		}
	}

	s.regMu.Lock()
	probeDigest := s.features.ProbeDigest
	s.regMu.Unlock()

	items := make([]rex.Item, 0, len(next))
	for i, e := range next {
		if !valid[i] {
			stats.Held++
			continue
		}
		digest := results[i].Digest
		// Recurring pushes carry the digest only when REX advertises the
		// feature; a fresh item always sends it alongside full media_info.
		if !fresh[i] && !probeDigest {
			digest = ""
		}
		var item rex.Item
		var ok bool
		if e.Category == "tv" {
			item, ok = rex.EpisodeItem(e, enriched[i], results[i].JSON, digest, s.cfg.RexSendTMDBIDs)
		} else {
			item, ok = rex.MovieItem(e, enriched[i], results[i].JSON, digest, s.cfg.RexSendTMDBIDs)
		}
		if !ok {
			stats.Held++
			continue
		}
		items = append(items, item)
	}

	batch := s.cfg.PushBatch
	if batch < 1 || batch > maxBatch {
		batch = maxBatch
	}
	for start := 0; start < len(items); start += batch {
		end := min(start+batch, len(items))
		result, err := s.feed.Push(ctx, items[start:end])
		if err != nil {
			return stats, fmt.Errorf("sync: push items: %w", err)
		}
		stats.Pushed += end - start
		stats.Held += result.Held
		stats.Rejected += len(result.Rejected)
	}
	return stats, nil
}

// register sends the source definition once. The directory names match the
// first segment of library.ItemPath ("movies"/"tv"): a different name would
// make REX place the items in a library it cannot reach. The library titles
// are the user-facing "Altmount Movies" and "Altmount TV".
func (s *Service) register(ctx context.Context) error {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	if s.registered {
		return nil
	}
	publicURL := s.cfg.PublicURL
	if publicURL == "" {
		publicURL = s.cfg.AltmountWebDAVURL
	}
	features, err := s.feed.Register(ctx, publicURL, []rex.Directory{
		{Name: "movies", Movies: "Altmount Movies"},
		{Name: "tv", Shows: "Altmount TV"},
	})
	if err != nil {
		return fmt.Errorf("sync: register source: %w", err)
	}
	s.features = features
	s.registered = true
	return nil
}

func (s *Service) enrich(ctx context.Context, e models.Entry) models.Enriched {
	if (s.cfg.TMDBReadToken == "" && s.cfg.TMDBAPIKey == "") || s.mdb == nil {
		return models.Enriched{}
	}
	title, year, season, episode := tmdb.ParseRelease(e.File, e.Category)
	en, ok, err := s.mdb.Enrich(ctx, title, year, season, episode, e.Category == "tv")
	if err != nil || !ok {
		return models.Enriched{}
	}
	return en
}

// probeMissing validates and caches fresh probes with at most ProbeWorkers
// concurrent ffprobe runs. Results land in their original entry indices so
// pushes stay in catalogue order and held entries are easy to count.
func (s *Service) probeMissing(ctx context.Context, entries []models.Entry, misses []int, results []probe.Result, valid, fresh []bool) {
	if len(misses) == 0 {
		return
	}
	workers := s.cfg.ProbeWorkers
	if workers < 1 {
		workers = 1
	}
	if workers > len(misses) {
		workers = len(misses)
	}
	jobs := make(chan int)
	var wg stdsync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				e := entries[idx]
				result, err := s.prb(ctx, e)
				if err != nil {
					continue
				}
				// probe.Probe does not validate: an ffprobe document can describe
				// a zero-filled file. Reject it here, before cache and push.
				if err := probe.Validate(result, s.rangedSample(ctx, e)); err != nil {
					continue
				}
				results[idx] = result
				valid[idx] = true
				fresh[idx] = true
			}
		}()
	}
	for _, idx := range misses {
		jobs <- idx
	}
	close(jobs)
	wg.Wait()
}

// rangedSample performs one small Basic-auth HTTP Range GET per requested
// offset. probe.Validate calls it at 2+ distinct offsets; a failure at any
// single offset is tolerated as long as two others yield non-zero bytes.
func (s *Service) rangedSample(ctx context.Context, e models.Entry) func(int64) ([]byte, error) {
	return rangedSample(s.http, ctx, s.cfg, e)
}

func rangedSample(h *http.Client, ctx context.Context, cfg config.Config, e models.Entry) func(int64) ([]byte, error) {
	target := webdavURL(cfg.AltmountWebDAVURL, e.Path)
	return func(offset int64) ([]byte, error) {
		if offset < 0 {
			return nil, fmt.Errorf("sync: negative sample offset %d", offset)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+sampleBytes-1))
		req.SetBasicAuth(cfg.AltmountUser, cfg.AltmountPassword)
		resp, err := h.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("sync: sample GET returned HTTP %d", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, sampleBytes))
	}
}

// webdavURL joins the WebDAV base with an entry's relative path, escaping each
// segment so spaces and unicode in release names become valid request paths.
func webdavURL(base, itemPath string) string {
	base = strings.TrimRight(base, "/")
	parts := strings.Split(itemPath, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return base + "/" + strings.Join(parts, "/")
}

func limitEntries(entries []models.Entry, max int) []models.Entry {
	if max > 0 && len(entries) > max {
		return entries[:max]
	}
	return entries
}

func httpTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return 15 * time.Second
	}
	return d
}
