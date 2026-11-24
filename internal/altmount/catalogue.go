package altmount

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/leleasley/rex-alt/internal/library"
	"github.com/leleasley/rex-alt/internal/models"
)

// videoExtensions mirrors REX's "which files are videos" list: the extensions
// a real PMS makes a playable Part of. Playlists, ISOs, DVD sidecars, subtitle
// files and NFOs are deliberately absent. .m3u8 is absent too: REX accepts it
// only when its probe says HLS, which enumeration alone cannot establish.
var videoExtensions = map[string]bool{
	"3g2": true, "3gp": true, "asf": true, "avc": true, "avi": true,
	"avs": true, "bivx": true, "divx": true, "dv": true, "dvr-ms": true,
	"evo": true, "fli": true, "flv": true, "m2t": true, "m2ts": true,
	"m2v": true, "m4v": true, "mk3d": true, "mkv": true, "mov": true,
	"mp4": true, "mpeg": true, "mpg": true, "mts": true, "nsv": true,
	"nuv": true, "ogm": true, "ogv": true, "pva": true, "qt": true,
	"rm": true, "rmvb": true, "svq3": true, "tp": true, "ts": true,
	"ty": true, "vdr": true, "vob": true, "vp3": true, "webm": true,
	"wmv": true, "wtv": true, "xvid": true,
}

// samplePattern matches "sample" as a whole token in a filename, matching
// REX's rejection of samples without rejecting titles such as "sampled.avi".
var samplePattern = regexp.MustCompile(`(?i)(^|[^a-z])sample([^a-z]|$)`)

func isVideoFile(name string) bool {
	ext := path.Ext(name)
	if !videoExtensions[strings.TrimPrefix(strings.ToLower(ext), ".")] {
		return false
	}
	return !samplePattern.MatchString(strings.TrimSuffix(name, ext))
}

// List enumerates one WebDAV category with a Depth: infinity PROPFIND and returns one
// Entry per video file at <category>/<release>/<file>. Season and Episode stay
// zero here; the release-name parser fills them in a later stage.
func (c *Client) List(ctx context.Context, category string) ([]models.Entry, error) {
	target, err := c.propfindURL(category)
	if err != nil {
		return nil, err
	}
	hrefs, err := c.propfind(ctx, target)
	if err != nil {
		return nil, err
	}

	prefix := c.basePath + category + "/"
	entries := make([]models.Entry, 0, len(hrefs))
	seen := make(map[string]struct{}, len(hrefs))
	for _, href := range hrefs {
		href, err := hrefPath(href)
		if err != nil || !strings.HasPrefix(href, prefix) {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(href, prefix), "/"), "/")
		if len(parts) != 2 || !validSegment(parts[0]) || !validSegment(parts[1]) {
			continue
		}
		release, file := parts[0], parts[1]
		if !isVideoFile(file) {
			continue
		}

		e := models.Entry{Category: category, Release: release, File: file}
		e.Path = library.ItemPath(e)
		e.ID = library.ItemID(e)
		if _, duplicate := seen[e.ID]; duplicate {
			continue
		}
		seen[e.ID] = struct{}{}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "\\\x00")
}

// Diff compares the stored catalogue with a freshly enumerated one. It is pure
// and keys entries by ID. An empty next catalogue, or one that would drop more
// than half of the old entries, is refused: a truncated or failed PROPFIND must
// never wipe the catalogue.
func Diff(old, next []models.Entry) (added, changed, removed []models.Entry, err error) {
	if len(next) == 0 {
		return nil, nil, nil, fmt.Errorf("altmount: refusing empty catalogue; possible truncated PROPFIND response")
	}

	oldByID := make(map[string]models.Entry, len(old))
	for _, e := range old {
		if _, exists := oldByID[e.ID]; !exists {
			oldByID[e.ID] = e
		}
	}
	nextByID := make(map[string]models.Entry, len(next))
	for _, e := range next {
		nextByID[e.ID] = e
	}

	for _, e := range next {
		previous, known := oldByID[e.ID]
		switch {
		case !known:
			added = append(added, e)
		case !sameEntry(previous, e):
			changed = append(changed, e)
		}
	}
	for _, e := range old {
		if _, found := nextByID[e.ID]; !found {
			removed = append(removed, e)
		}
	}
	if len(removed) > len(old)/2 {
		return nil, nil, nil, fmt.Errorf("altmount: refusing to drop %d of %d entries; possible truncated catalogue", len(removed), len(old))
	}

	sortByID(added)
	sortByID(changed)
	sortByID(removed)
	return added, changed, removed, nil
}

// sameEntry treats any metadata difference under a stable id as a change.
// models.Entry has no size field, so a replaced file is visible here through
// its file/path (and, once Task 7 runs, its season/episode).
func sameEntry(a, b models.Entry) bool {
	return a.ID == b.ID && a.Path == b.Path && a.Category == b.Category &&
		a.Release == b.Release && a.File == b.File &&
		a.Season == b.Season && a.Episode == b.Episode
}

func sortByID(entries []models.Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
}
