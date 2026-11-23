package rex

import "encoding/json"

// Item is one file a source pushes to REX's feed. The JSON tags are the feed
// API's wire format; fields with no value are omitted so REX keeps whatever it
// already knows for them. MediaInfo is deliberately not omitted: an item with
// no probe is held by REX rather than shown unplayable.
//
// This file holds only the wire type. The constructors that build Items from
// library entries live in items.go (Task 4).
type Item struct {
	ID          string          `json:"id"`
	Path        string          `json:"path"`
	Kind        string          `json:"kind,omitempty"`
	Name        string          `json:"name,omitempty"`
	Season      *int            `json:"season,omitempty"`
	Episode     *int            `json:"episode,omitempty"`
	EpisodeEnd  *int            `json:"episode_end,omitempty"`
	TMDBEpisode int             `json:"tmdb_episode,omitempty"`
	IDs         map[string]any  `json:"ids,omitempty"`
	MediaInfo   json.RawMessage `json:"media_info"`
	Display     *Display        `json:"display,omitempty"`
	Release     string          `json:"release,omitempty"`
	Probe       string          `json:"probe,omitempty"`
	Size        int64           `json:"size,omitempty"`
	File        string          `json:"file,omitempty"`
	Collections []string        `json:"collections,omitempty"`
}

// Display supplies the title and artwork REX shows while no metadata agent
// has identified the row.
type Display struct {
	Title   string       `json:"title,omitempty"`
	Summary string       `json:"summary,omitempty"`
	Date    string       `json:"date,omitempty"`
	Thumb   string       `json:"thumb,omitempty"`
	Art     string       `json:"art,omitempty"`
	Show    *DisplayShow `json:"show,omitempty"`
}

// DisplayShow is the show-level part of an episode's Display.
type DisplayShow struct {
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	Poster  string `json:"poster,omitempty"`
	Art     string `json:"art,omitempty"`
}
