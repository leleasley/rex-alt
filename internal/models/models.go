// Package models holds the types shared by the Altmount enumerator, the TMDB
// enricher and the REX item mapper. Keeping them here avoids import cycles
// between those packages.
package models

// Entry is one media file found under an Altmount WebDAV category. Path is the
// raw WebDAV path; Category, Release and File are its three segments and drive
// the stable REX id. Season and Episode are zero for movies.
type Entry struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Category string `json:"category"`
	Release  string `json:"release"`
	File     string `json:"file"`
	Season   int    `json:"season,omitempty"`
	Episode  int    `json:"episode,omitempty"`
}

// Enriched is the optional TMDB metadata attached to an Entry. TMDBID is the
// movie or show id (never an episode id); EpisodeTMDBID is only used for
// episodes. All fields may be empty when enrichment is skipped or fails.
type Enriched struct {
	TMDBID        int    `json:"tmdb_id,omitempty"`
	IMDbID        string `json:"imdb_id,omitempty"`
	Title         string `json:"title,omitempty"`
	Summary       string `json:"summary,omitempty"`
	Date          string `json:"date,omitempty"`
	Poster        string `json:"poster,omitempty"`
	Backdrop      string `json:"backdrop,omitempty"`
	ShowTitle     string `json:"show_title,omitempty"`
	ShowPoster    string `json:"show_poster,omitempty"`
	ShowBackdrop  string `json:"show_backdrop,omitempty"`
	EpisodeTMDBID int    `json:"episode_tmdb_id,omitempty"`
}
