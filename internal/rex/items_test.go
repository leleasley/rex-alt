package rex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/leleasley/rex-alt/internal/models"
)

const probeInfo = `{"format":{"size":1234},"streams":[{"codec_type":"video","codec_name":"h264"}]}`

func TestMovieItemNeedsProbe(t *testing.T) {
	e := models.Entry{Category: "movies", Release: "Rel", File: "file.mkv"}
	en := models.Enriched{Title: "Rel", TMDBID: 7}
	for name, info := range map[string]json.RawMessage{
		"nil":        nil,
		"empty":      json.RawMessage(""),
		"whitespace": json.RawMessage("  \n\t "),
	} {
		t.Run(name, func(t *testing.T) {
			item, ok := MovieItem(e, en, info, "digest", false)
			if ok {
				t.Fatalf("MovieItem with %q media_info returned ok=true", info)
			}
			if item.ID != "" || item.Path != "" {
				t.Errorf("rejected item leaked identity: %+v", item)
			}
		})
	}
}

func TestMovieItemTMDBFlag(t *testing.T) {
	e := models.Entry{Category: "movies", Release: "Rel", File: "file.mkv"}
	en := models.Enriched{
		TMDBID: 7, IMDbID: "tt0000007", Title: "A Title", Summary: "A summary",
		Date: "2024-05-06", Poster: "http://img/poster.jpg", Backdrop: "http://img/backdrop.jpg",
	}
	info := json.RawMessage(probeInfo)

	on, ok := MovieItem(e, en, info, "probe-digest", true)
	if !ok {
		t.Fatal("MovieItem(useTMDB=true) returned ok=false")
	}
	if on.IDs["tmdb"] != 7 {
		t.Errorf("ids.tmdb = %v, want 7", on.IDs["tmdb"])
	}
	if on.Kind != "movie" || on.Release != "Rel" || on.Probe != "probe-digest" || on.File != "file.mkv" {
		t.Errorf("identity fields = %+v", on)
	}
	if on.Size != 1234 {
		t.Errorf("size = %d, want 1234", on.Size)
	}
	if want := "movies/Rel/file.mkv"; on.ID != want {
		t.Errorf("id = %q, want %q", on.ID, want)
	}
	if on.Path != "movies/Rel/file.mkv" {
		t.Errorf("path = %q, want movies/Rel/file.mkv", on.Path)
	}
	if on.MediaInfo == nil || string(on.MediaInfo) != probeInfo {
		t.Errorf("media_info = %s, want %s", on.MediaInfo, probeInfo)
	}
	checkDisplay(t, on.Display, "A Title", "A summary", "2024-05-06", "http://img/poster.jpg", "http://img/backdrop.jpg")

	off, ok := MovieItem(e, en, info, "probe-digest", false)
	if !ok {
		t.Fatal("MovieItem(useTMDB=false) returned ok=false")
	}
	if len(off.IDs) != 0 {
		t.Errorf("ids = %v with useTMDB=false, want none", off.IDs)
	}
	if off.Display == nil || off.Display.Title != "A Title" {
		t.Errorf("display must be populated even when ids are off: %+v", off.Display)
	}
	raw, err := json.Marshal(off)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"ids"`) {
		t.Errorf("marshalled item contains ids with useTMDB=false: %s", raw)
	}
}

func TestEpisodeItemSetsSeasonEpisodeAndEpisodeTMDB(t *testing.T) {
	e := models.Entry{Category: "tv", Release: "Show", File: "Show.S02E04.mkv", Season: 2, Episode: 4}
	en := models.Enriched{
		TMDBID: 99, EpisodeTMDBID: 123, Title: "Episode Title", Summary: "Episode summary",
		Date: "2024-03-02", Poster: "http://img/still.jpg", Backdrop: "http://img/episode-art.jpg",
		ShowTitle: "Show Title", ShowPoster: "http://img/show.jpg", ShowBackdrop: "http://img/show-art.jpg",
	}
	info := json.RawMessage(probeInfo)

	on, ok := EpisodeItem(e, en, info, "digest", true)
	if !ok {
		t.Fatal("EpisodeItem(useTMDB=true) returned ok=false")
	}
	if on.Season == nil || *on.Season != 2 || on.Episode == nil || *on.Episode != 4 {
		t.Errorf("season/episode = %v/%v, want 2/4", on.Season, on.Episode)
	}
	if on.TMDBEpisode != 123 {
		t.Errorf("tmdb_episode = %d, want 123", on.TMDBEpisode)
	}
	if on.IDs["tmdb"] != 99 {
		t.Errorf("ids.tmdb = %v, want show id 99", on.IDs["tmdb"])
	}
	if _, ok := on.IDs["tmdb_episode"]; ok {
		t.Errorf("ids must not carry the episode id: %v", on.IDs)
	}
	if on.Kind != "episode" {
		t.Errorf("kind = %q, want episode", on.Kind)
	}
	if on.Display == nil || on.Display.Show == nil {
		t.Fatalf("episode display/show missing: %+v", on.Display)
	}
	checkDisplay(t, on.Display, "Episode Title", "Episode summary", "2024-03-02", "http://img/still.jpg", "http://img/episode-art.jpg")
	if on.Display.Show.Title != "Show Title" || on.Display.Show.Poster != "http://img/show.jpg" || on.Display.Show.Art != "http://img/show-art.jpg" {
		t.Errorf("show display = %+v", on.Display.Show)
	}

	off, ok := EpisodeItem(e, en, info, "digest", false)
	if !ok {
		t.Fatal("EpisodeItem(useTMDB=false) returned ok=false")
	}
	if len(off.IDs) != 0 || off.TMDBEpisode != 0 {
		t.Errorf("ids/tmdb_episode must be off when useTMDB=false: %v/%d", off.IDs, off.TMDBEpisode)
	}
}

func TestMovieItemDisplayDateAndSize(t *testing.T) {
	cases := []struct {
		name string
		date string
		info string
		want string
		size int64
	}{
		{"year", "2024", probeInfo, "2024", 1234},
		{"full date", "2024-05-06", probeInfo, "2024-05-06", 1234},
		{"padded date", " 2024-05-06 ", probeInfo, "2024-05-06", 1234},
		{"string size", "2024", `{"format":{"size":"2048"},"streams":[]}`, "2024", 2048},
		{"invalid date", "not a date", probeInfo, "", 1234},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item, ok := MovieItem(
				models.Entry{Category: "movies", Release: "Rel", File: "file.mkv"},
				models.Enriched{Title: "Title", Date: tc.date},
				json.RawMessage(tc.info), "digest", false,
			)
			if !ok {
				t.Fatal("MovieItem returned ok=false")
			}
			if item.Display == nil || item.Display.Date != tc.want {
				t.Errorf("display.date = %q, want %q", item.Display.Date, tc.want)
			}
			if item.Size != tc.size {
				t.Errorf("size = %d, want %d", item.Size, tc.size)
			}
		})
	}
}

func checkDisplay(t *testing.T, got *Display, title, summary, date, thumb, art string) {
	t.Helper()
	if got == nil {
		t.Fatal("display is nil")
	}
	if got.Title != title || got.Summary != summary || got.Date != date || got.Thumb != thumb || got.Art != art {
		t.Errorf("display = %+v, want title=%q summary=%q date=%q thumb=%q art=%q", got, title, summary, date, thumb, art)
	}
}
