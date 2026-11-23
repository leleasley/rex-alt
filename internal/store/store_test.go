package store

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/leleasley/rex-alt/internal/models"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func seed(t *testing.T, s *Store, entries ...models.Entry) {
	t.Helper()
	if err := s.UpsertEntries(entries); err != nil {
		t.Fatalf("UpsertEntries: %v", err)
	}
}

func byID(entries []models.Entry) map[string]models.Entry {
	out := make(map[string]models.Entry, len(entries))
	for _, e := range entries {
		out[e.ID] = e
	}
	return out
}

func TestUpsertAndListEntries(t *testing.T) {
	s := testStore(t)
	want := []models.Entry{
		{ID: "movies/Rel/a.mkv", Path: "movies/Rel/a.mkv", Category: "movies", Release: "Rel", File: "a.mkv"},
		{ID: "tv/Show/S01E02.mkv", Path: "tv/Show/S01E02.mkv", Category: "tv", Release: "Show", File: "S01E02.mkv", Season: 1, Episode: 2},
	}
	seed(t, s, want...)

	got, err := s.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	if !reflect.DeepEqual(byID(got), byID(want)) {
		t.Errorf("entries = %+v, want %+v", byID(got), byID(want))
	}
}

func TestUpsertEntriesUpdatesExistingAndEmptyIsNoOp(t *testing.T) {
	s := testStore(t)
	id := "movies/Rel/a.mkv"
	seed(t, s, models.Entry{ID: id, Path: "old", Category: "movies", Release: "Rel", File: "a.mkv"})
	seed(t, s, models.Entry{ID: id, Path: "new", Category: "movies", Release: "Rel", File: "b.mkv", Season: 2})

	got, err := s.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	want := models.Entry{ID: id, Path: "new", Category: "movies", Release: "Rel", File: "b.mkv", Season: 2}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("entries = %+v, want [%+v]", got, want)
	}

	if err := s.UpsertEntries(nil); err != nil {
		t.Fatalf("UpsertEntries(nil): %v", err)
	}
	got, err = s.Entries()
	if err != nil || len(got) != 1 {
		t.Fatalf("after empty upsert: entries = %+v, err = %v", got, err)
	}
}

func TestOpenConfiguresWALAndBusyTimeout(t *testing.T) {
	s := testStore(t)
	var journal string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
	var timeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

func TestRemoveEntriesNotInRefusesEmptySet(t *testing.T) {
	s := testStore(t)
	seed(t, s,
		models.Entry{ID: "a", Path: "a"},
		models.Entry{ID: "b", Path: "b"},
		models.Entry{ID: "c", Path: "c"},
	)
	before, err := s.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}

	for name, ids := range map[string][]string{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			n, err := s.RemoveEntriesNotIn(ids)
			if err == nil {
				t.Fatalf("RemoveEntriesNotIn(%v) succeeded, want an error", ids)
			}
			if n != 0 {
				t.Errorf("removed %d rows, want 0", n)
			}
			after, err := s.Entries()
			if err != nil {
				t.Fatalf("Entries: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Errorf("empty set removed rows: before = %+v, after = %+v", before, after)
			}
		})
	}

	t.Run("truncated set", func(t *testing.T) {
		n, err := s.RemoveEntriesNotIn([]string{"a"})
		if err == nil {
			t.Fatal("RemoveEntriesNotIn keeping 1 of 3 succeeded, want an error")
		}
		if n != 0 {
			t.Errorf("removed %d rows, want 0", n)
		}
		after, err := s.Entries()
		if err != nil {
			t.Fatalf("Entries: %v", err)
		}
		if !reflect.DeepEqual(after, before) {
			t.Errorf("truncated set removed rows: before = %+v, after = %+v", before, after)
		}
	})
}

func TestRemoveEntriesNotInDeletesStale(t *testing.T) {
	s := testStore(t)
	seed(t, s,
		models.Entry{ID: "a", Path: "a"},
		models.Entry{ID: "b", Path: "b"},
		models.Entry{ID: "c", Path: "c"},
	)
	n, err := s.RemoveEntriesNotIn([]string{"a", "b", "unknown"})
	if err != nil {
		t.Fatalf("RemoveEntriesNotIn: %v", err)
	}
	if n != 1 {
		t.Errorf("removed %d rows, want 1", n)
	}
	got, err := s.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(got) != 2 || byID(got)["a"].ID != "a" || byID(got)["b"].ID != "b" {
		t.Errorf("entries = %+v, want a and b", got)
	}
}

func TestStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entry := models.Entry{ID: "movies/Rel/a.mkv", Path: "movies/Rel/a.mkv", Category: "movies", Release: "Rel", File: "a.mkv"}
	seed(t, s, entry)
	if err := s.SetProbe(entry.ID, `{"streams":[]}`, "digest"); err != nil {
		t.Fatalf("SetProbe: %v", err)
	}
	en := models.Enriched{TMDBID: 99, Title: "Title"}
	if err := s.SetTMDB("movie:99", en); err != nil {
		t.Fatalf("SetTMDB: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	entries, err := s.Entries()
	if err != nil || len(entries) != 1 || entries[0] != entry {
		t.Fatalf("entries after reopen = %+v, %v; want [%+v]", entries, err, entry)
	}
	gotJSON, digest, ok := s.Probe(entry.ID)
	if !ok || gotJSON != `{"streams":[]}` || digest != "digest" {
		t.Errorf("probe after reopen = %q, %q, %v", gotJSON, digest, ok)
	}
	gotEn, ok := s.TMDB("movie:99")
	if !ok || gotEn != en {
		t.Errorf("TMDB after reopen = %+v, %v; want %+v, true", gotEn, ok, en)
	}
}

func TestProbeCacheRoundTrip(t *testing.T) {
	s := testStore(t)
	if got, digest, ok := s.Probe("missing"); ok || got != "" || digest != "" {
		t.Errorf("miss = %q, %q, %v; want empty, empty, false", got, digest, ok)
	}

	const json1 = `{"streams":[{"codec_name":"h264"}],"format":{"duration":"10.0"}}`
	if err := s.SetProbe("id-1", json1, "digest-1"); err != nil {
		t.Fatalf("SetProbe: %v", err)
	}
	got, digest, ok := s.Probe("id-1")
	if !ok || got != json1 || digest != "digest-1" {
		t.Errorf("Probe = %q, %q, %v; want %q, %q, true", got, digest, ok, json1, "digest-1")
	}

	const json2 = `{"streams":[{"codec_name":"hevc"}]}`
	if err := s.SetProbe("id-1", json2, "digest-2"); err != nil {
		t.Fatalf("SetProbe overwrite: %v", err)
	}
	got, digest, ok = s.Probe("id-1")
	if !ok || got != json2 || digest != "digest-2" {
		t.Errorf("Probe after overwrite = %q, %q, %v; want %q, %q, true", got, digest, ok, json2, "digest-2")
	}
	if err := s.SetProbe("id-2", json1, "digest-3"); err != nil {
		t.Fatalf("SetProbe other id: %v", err)
	}
	got, digest, ok = s.Probe("id-2")
	if !ok || got != json1 || digest != "digest-3" {
		t.Errorf("Probe(id-2) = %q, %q, %v; want %q, %q, true", got, digest, ok, json1, "digest-3")
	}
}

func TestTMDBCacheRoundTrip(t *testing.T) {
	s := testStore(t)
	if got, ok := s.TMDB("missing"); ok || got != (models.Enriched{}) {
		t.Errorf("miss = %+v, %v; want zero, false", got, ok)
	}

	en := models.Enriched{
		TMDBID:        123,
		IMDbID:        "tt0000123",
		Title:         "A Movie",
		Summary:       "A summary",
		Date:          "2022-04-01",
		Poster:        "/poster.jpg",
		Backdrop:      "/backdrop.jpg",
		ShowTitle:     "A Show",
		ShowPoster:    "/show-poster.jpg",
		ShowBackdrop:  "/show-backdrop.jpg",
		EpisodeTMDBID: 456,
	}
	const key = "movie:123:2022"
	if err := s.SetTMDB(key, en); err != nil {
		t.Fatalf("SetTMDB: %v", err)
	}
	got, ok := s.TMDB(key)
	if !ok || got != en {
		t.Errorf("TMDB = %+v, %v; want %+v, true", got, ok, en)
	}

	updated := models.Enriched{TMDBID: 7, Title: "Another"}
	if err := s.SetTMDB(key, updated); err != nil {
		t.Fatalf("SetTMDB overwrite: %v", err)
	}
	got, ok = s.TMDB(key)
	if !ok || got != updated {
		t.Errorf("TMDB after overwrite = %+v, %v; want %+v, true", got, ok, updated)
	}
	if err := s.SetTMDB("other", en); err != nil {
		t.Fatalf("SetTMDB other key: %v", err)
	}
	got, ok = s.TMDB("other")
	if !ok || got != en {
		t.Errorf("TMDB(other) = %+v, %v; want %+v, true", got, ok, en)
	}
}
