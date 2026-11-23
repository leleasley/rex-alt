package rex

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/leleasley/rex-alt/internal/library"
	"github.com/leleasley/rex-alt/internal/models"
)

// datePattern accepts a year or a full calendar date (and the leading part of
// an ISO timestamp, which TMDB occasionally returns).
var datePattern = regexp.MustCompile(`^(\d{4})(?:-(\d{2})-(\d{2}))?`)

// MovieItem maps an enumerated movie to a REX feed item. The item is skipped
// unless media_info is present: REX holds unprobed items invisibly forever.
// TMDB ids are opt-in; display metadata is always sent.
func MovieItem(e models.Entry, en models.Enriched, info json.RawMessage, digest string, useTMDB bool) (Item, bool) {
	if !hasProbe(info) {
		return Item{}, false
	}
	item := baseItem(e, info, digest, "movie")
	item.Display = &Display{
		Title:   en.Title,
		Summary: en.Summary,
		Date:    displayDate(en.Date),
		Thumb:   en.Poster,
		Art:     en.Backdrop,
	}
	if useTMDB {
		item.IDs = tmdbIDs(en)
	}
	return item, true
}

// EpisodeItem maps an enumerated episode to a REX feed item. ids.tmdb carries
// the *show* id, while the episode's own TMDB id goes in tmdb_episode; both are
// gated by useTMDB.
func EpisodeItem(e models.Entry, en models.Enriched, info json.RawMessage, digest string, useTMDB bool) (Item, bool) {
	if !hasProbe(info) {
		return Item{}, false
	}
	item := baseItem(e, info, digest, "episode")
	season, episode := e.Season, e.Episode
	item.Season = &season
	item.Episode = &episode
	item.Name = firstNonEmpty(en.ShowTitle, "Series")
	item.Display = &Display{
		Title:   firstNonEmpty(en.Title, en.ShowTitle),
		Summary: en.Summary,
		Date:    displayDate(en.Date),
		Thumb:   firstNonEmpty(en.Poster, en.ShowPoster),
		Art:     en.Backdrop,
		Show: &DisplayShow{
			Title:  en.ShowTitle,
			Poster: en.ShowPoster,
			Art:    en.ShowBackdrop,
		},
	}
	if useTMDB {
		item.IDs = tmdbIDs(en)
		item.TMDBEpisode = en.EpisodeTMDBID
	}
	return item, true
}

func baseItem(e models.Entry, info json.RawMessage, digest, kind string) Item {
	return Item{
		ID:        library.ItemID(e),
		Path:      library.ItemPath(e),
		Kind:      kind,
		MediaInfo: info,
		Release:   e.Release,
		Probe:     digest,
		Size:      probeSize(info),
		File:      e.File,
	}
}

func hasProbe(info json.RawMessage) bool {
	return len(bytes.TrimSpace(info)) > 0
}

// tmdbIDs identifies the movie or show only; an episode id must never appear
// in ids. IMDb is a fallback when TMDB did not yield an id.
func tmdbIDs(en models.Enriched) map[string]any {
	ids := map[string]any{}
	if en.TMDBID > 0 {
		ids["tmdb"] = en.TMDBID
	} else if strings.TrimSpace(en.IMDbID) != "" {
		ids["imdb"] = strings.TrimSpace(en.IMDbID)
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

func probeSize(info json.RawMessage) int64 {
	var document struct {
		Format struct {
			Size any `json:"size"`
		} `json:"format"`
	}
	decoder := json.NewDecoder(bytes.NewReader(info))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return 0
	}
	switch size := document.Format.Size.(type) {
	case json.Number:
		return parseSize(string(size))
	case string:
		return parseSize(strings.TrimSpace(size))
	case float64:
		return int64(size)
	default:
		return 0
	}
}

func parseSize(value string) int64 {
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return int64(f)
	}
	return 0
}

func displayDate(raw string) string {
	value := strings.TrimSpace(raw)
	match := datePattern.FindStringSubmatch(value)
	if match == nil {
		return ""
	}
	if match[2] == "" {
		return match[1]
	}
	month, _ := strconv.Atoi(match[2])
	day, _ := strconv.Atoi(match[3])
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return match[1]
	}
	return match[1] + "-" + match[2] + "-" + match[3]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
