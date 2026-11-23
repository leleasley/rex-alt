// Package store owns the add-on's SQLite state: the catalogue of media files
// and the probe/TMDB caches. Only metadata is persisted here — no media bytes
// and no provider stream URLs are ever written to the database.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/leleasley/rex-alt/internal/models"
	_ "modernc.org/sqlite" // pure-Go driver, no CGO needed in the runtime image
)

// maxRemovalDivisor bounds the share of the catalogue one sync may drop: at
// most len(current)/2 rows may go. A truncated PROPFIND response must not be
// able to empty the store.
const maxRemovalDivisor = 2

// Store is the single owner of the SQLite database. It is safe for concurrent
// use; SQLite serialises writers and busy_timeout waits for the lock.
type Store struct {
	db *sql.DB
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS entries (
		id       TEXT PRIMARY KEY,
		path     TEXT NOT NULL,
		category TEXT NOT NULL,
		release  TEXT NOT NULL,
		file     TEXT NOT NULL,
		season   INTEGER NOT NULL,
		episode  INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS probes (
		id     TEXT PRIMARY KEY,
		json   TEXT NOT NULL,
		digest TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS tmdb (
		key  TEXT PRIMARY KEY,
		json TEXT NOT NULL
	)`,
}

// Open creates (or opens) the database at path and applies the schema. The
// pragmas live in the DSN so every connection in the pool gets them, not just
// the one that happens to run the CREATE statements. WAL keeps reads from
// blocking the sync writer; a 5s busy timeout absorbs short lock contention.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: database path is required")
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: create schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// UpsertEntries inserts or refreshes the given entries by id, in one
// transaction. No probe JSON or TMDB data is touched: caches are keyed by id
// and survive a re-enumeration unchanged.
func (s *Store) UpsertEntries(entries []models.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin upsert: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO entries (id, path, category, release, file, season, episode)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			path = excluded.path,
			category = excluded.category,
			release = excluded.release,
			file = excluded.file,
			season = excluded.season,
			episode = excluded.episode`)
	if err != nil {
		return fmt.Errorf("store: prepare upsert: %w", err)
	}
	defer stmt.Close()

	for _, e := range entries {
		if _, err := stmt.Exec(e.ID, e.Path, e.Category, e.Release, e.File, e.Season, e.Episode); err != nil {
			return fmt.Errorf("store: upsert %q: %w", e.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit upsert: %w", err)
	}
	return nil
}

// Entries returns every known entry, ordered by id for stable diffs.
func (s *Store) Entries() ([]models.Entry, error) {
	rows, err := s.db.Query(`SELECT id, path, category, release, file, season, episode FROM entries ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list entries: %w", err)
	}
	defer rows.Close()

	out := make([]models.Entry, 0)
	for rows.Next() {
		var e models.Entry
		if err := rows.Scan(&e.ID, &e.Path, &e.Category, &e.Release, &e.File, &e.Season, &e.Episode); err != nil {
			return nil, fmt.Errorf("store: scan entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list entries: %w", err)
	}
	return out, nil
}

// RemoveEntriesNotIn deletes the entries whose ids are absent from ids and
// returns how many rows were deleted. It refuses to act on an empty set, or
// when more than half the catalogue would disappear: a failed or truncated
// WebDAV listing must never wipe the store.
func (s *Store) RemoveEntriesNotIn(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, fmt.Errorf("store: refusing to remove entries for an empty id set")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin removal: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id FROM entries`)
	if err != nil {
		return 0, fmt.Errorf("store: list ids: %w", err)
	}
	var current []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan id: %w", err)
		}
		current = append(current, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("store: list ids: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("store: close id list: %w", err)
	}

	keep := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		keep[id] = struct{}{}
	}
	var stale []string
	for _, id := range current {
		if _, found := keep[id]; !found {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("store: commit removal: %w", err)
		}
		return 0, nil
	}
	if len(stale) > len(current)/maxRemovalDivisor {
		return 0, fmt.Errorf("store: refusing to remove %d of %d entries; possible truncated catalogue", len(stale), len(current))
	}

	stmt, err := tx.Prepare(`DELETE FROM entries WHERE id = ?`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare removal: %w", err)
	}
	defer stmt.Close()
	removed := 0
	for _, id := range stale {
		res, err := stmt.Exec(id)
		if err != nil {
			return 0, fmt.Errorf("store: remove %q: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: count removal: %w", err)
		}
		removed += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit removal: %w", err)
	}
	return removed, nil
}

// SetProbe caches the full ffprobe JSON for id together with its digest.
func (s *Store) SetProbe(id, json, digest string) error {
	_, err := s.db.Exec(`INSERT INTO probes (id, json, digest) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET json = excluded.json, digest = excluded.digest`, id, json, digest)
	if err != nil {
		return fmt.Errorf("store: set probe %q: %w", id, err)
	}
	return nil
}

// Probe returns the cached probe JSON and digest for id. A missing row (or any
// read failure, since this accessor has no error result) is reported as a miss.
func (s *Store) Probe(id string) (json, digest string, ok bool) {
	err := s.db.QueryRow(`SELECT json, digest FROM probes WHERE id = ?`, id).Scan(&json, &digest)
	if err != nil {
		// The signature has no error result: a miss (or any read failure) is
		// reported as a miss so the caller re-probes instead of pushing stale
		// metadata.
		return "", "", false
	}
	return json, digest, true
}

// SetTMDB caches TMDB metadata under key (for example "movie:123:2022").
func (s *Store) SetTMDB(key string, en models.Enriched) error {
	body, err := json.Marshal(en)
	if err != nil {
		return fmt.Errorf("store: marshal TMDB %q: %w", key, err)
	}
	if _, err := s.db.Exec(`INSERT INTO tmdb (key, json) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET json = excluded.json`, key, string(body)); err != nil {
		return fmt.Errorf("store: set TMDB %q: %w", key, err)
	}
	return nil
}

// TMDB returns the cached metadata under key, or false on a miss / bad JSON.
func (s *Store) TMDB(key string) (models.Enriched, bool) {
	var body string
	if err := s.db.QueryRow(`SELECT json FROM tmdb WHERE key = ?`, key).Scan(&body); err != nil {
		return models.Enriched{}, false
	}
	var en models.Enriched
	if err := json.Unmarshal([]byte(body), &en); err != nil {
		return models.Enriched{}, false
	}
	return en, true
}

// Close releases the database handle. Callers should invoke it on shutdown so
// WAL frames are checkpointed before the container volume is unmounted.
func (s *Store) Close() error {
	return s.db.Close()
}
