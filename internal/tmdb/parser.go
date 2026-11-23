// Package tmdb parses release names and resolves them to TMDB metadata. It
// stores only metadata in SQLite — never media bytes or provider URLs.
package tmdb

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

var (
	// Release groups suffix their tags with a hyphen (x264-GROUP, h265-ETHEL).
	seasonEpisodeRE = regexp.MustCompile(`(?i)\bs(\d{1,2})e(\d{1,4})\b`)
	yearRE          = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)
	bracketRE       = regexp.MustCompile(`\[[^]]*\]|\([^)]*\)`)
	resolutionRE    = regexp.MustCompile(`^\d{3,4}[pi]$`)
)

// videoExtensions are the only suffixes stripped from a filename. A release
// group tag like "-DST" looks like an extension but must be kept for the junk
// filter (which drops it as part of the codec token).
var videoExtensions = map[string]bool{
	".3gp": true, ".avi": true, ".flv": true, ".m2ts": true, ".m4v": true,
	".mkv": true, ".mov": true, ".mp4": true, ".mpeg": true, ".mpg": true,
	".ts": true, ".vob": true, ".webm": true, ".wmv": true,
}

// junkTokens are release-name tokens that never belong to a title: video
// resolution, source, codec, audio format and upload-group markers.
var junkTokens = map[string]bool{
	"4k": true, "uhd": true, "hdr": true, "hdr10": true, "10bit": true, "8bit": true,
	"bluray": true, "blu": true, "ray": true, "web": true, "webdl": true,
	"webrip": true, "web-dl": true, "hdtv": true, "dvdrip": true, "bdrip": true,
	"remux": true, "x264": true, "x265": true, "h264": true, "h265": true,
	"hevc": true, "av1": true, "xvid": true, "divx": true, "aac": true,
	"ac3": true, "eac3": true, "dts": true, "truehd": true, "atmos": true,
	"ddp": true, "dd": true, "flac": true, "mp3": true, "amzn": true,
	"nf": true, "dsnp": true, "hmax": true, "atvp": true, "pcok": true,
	"proper": true, "repack": true, "internal": true, "extended": true,
	"unrated": true, "limited": true, "sample": true, "dubbed": true,
	"subbed": true, "multi": true, "dual": true, "eng": true, "english": true,
	"subs": true, "srt": true, "ass": true, "req": true, "yify": true, "yts": true,
}

// ParseRelease turns a WebDAV file or release name into the fields TMDB needs.
// Dots and underscores become spaces; extension, bracketed group tags,
// quality/source/codec tokens and the year or SxxExx marker are removed. The
// original capitalisation of the surviving title words is preserved.
// A season/episode is only reported for a tv category; the marker is stripped
// from movies too so it cannot leak into their title.
func ParseRelease(name, category string) (title string, year int, season, episode int) {
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if ext := strings.ToLower(path.Ext(base)); videoExtensions[ext] {
		base = strings.TrimSuffix(base, path.Ext(base))
	}
	base = strings.NewReplacer(".", " ", "_", " ").Replace(base)

	// The S/E marker is removed for every category, but only reported for tv.
	if loc := seasonEpisodeRE.FindStringSubmatchIndex(base); loc != nil {
		s, _ := strconv.Atoi(base[loc[2]:loc[3]])
		e, _ := strconv.Atoi(base[loc[4]:loc[5]])
		if isTVCategory(category) {
			season, episode = s, e
		}
		base = base[:loc[0]] + " " + base[loc[1]:]
	}

	// The last four-digit year is the release year: titles such as
	// "Blade Runner 2049 2017" put the film's title number first.
	if locs := yearRE.FindAllStringIndex(base, -1); len(locs) > 0 {
		loc := locs[len(locs)-1]
		year, _ = strconv.Atoi(base[loc[0]:loc[1]])
		base = base[:loc[0]] + " " + base[loc[1]:]
	}

	// Group tags in brackets are removed last so a year such as "(2022)" is
	// still available to the extraction above.
	base = bracketRE.ReplaceAllString(base, " ")

	words := strings.Fields(base)
	kept := make([]string, 0, len(words))
	for _, word := range words {
		if !isJunkToken(word) {
			kept = append(kept, word)
		}
	}
	return strings.Join(kept, " "), year, season, episode
}

func isTVCategory(category string) bool {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "tv", "shows", "series":
		return true
	default:
		return false
	}
}

func isJunkToken(word string) bool {
	lower := strings.ToLower(strings.Trim(word, "-[](){}.,'\""))
	if lower == "" {
		return true
	}
	if junkTokens[lower] || resolutionRE.MatchString(lower) {
		return true
	}
	// "x264-DST" and "web-DL": the codec/source half identifies the token.
	if i := strings.IndexByte(lower, '-'); i > 0 {
		head := lower[:i]
		return junkTokens[head] || resolutionRE.MatchString(head)
	}
	return false
}
