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
	seasonEpisodeRE = regexp.MustCompile(`(?i)\bs(\d{1,3})e(\d{1,4})\b`)
	// A season-only release carries a bare Sxx token with no episode number.
	seasonOnlyRE = regexp.MustCompile(`(?i)\bs\d{1,3}\b`)
	yearRE       = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)
	// Daily shows (wrestling, news, talk) are released as "Show.Name.2026.11.23"
	// with no SxxExx marker: that triplet is the episode air date, not the
	// show's first-air year. Dots have become spaces by now, so dot, dash and
	// space separators all match; month/day ranges reject non-date numbers.
	tvAirDateRE  = regexp.MustCompile(`\b(?:19|20)\d{2}[ .-](?:0[1-9]|1[0-2])[ .-](?:0[1-9]|[12]\d|3[01])\b`)
	bracketRE    = regexp.MustCompile(`\[[^]]*\]|\([^)]*\)`)
	resolutionRE = regexp.MustCompile(`^\d{3,4}[pi]$`)

	// Audio codecs arrive both dotted (DD5.1) and spaced (DD5 1, DTS HD MA).
	// Dots have already become spaces by the time these run, so every pattern
	// tolerates whitespace and separators. Channel counts are limited to 5.1
	// and 7.1 so title numbers such as "10" and "12.12.12" are never lost.
	ddCodecRE       = regexp.MustCompile(`(?i)\bddp?\+?\s*\d(?:\s*\d)?\b`)
	dtsCodecRE      = regexp.MustCompile(`(?i)\bdts(?:[\s.-]+hd)?(?:[\s.-]+ma)?\b`)
	channelCountRE  = regexp.MustCompile(`(?i)\b[57]\s*[.,]?\s*1\b`)
	codecHintRE     = regexp.MustCompile(`(?i)\b(?:ddp?\+?\d*|dts|truehd|atmos|e?ac3|aac|flac|mp3)`)
	audioFragmentRE = regexp.MustCompile(`(?i)\b(?:ddp|dd|hd|ma)\b|\bdd\+`)
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
	"bluray": true, "blueray": true, "blu-ray": true, "blu": true, "ray": true,
	"web": true, "webdl": true,
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
// For tv, the title ends at the first SxxExx marker because everything after
// it is the episode-specific subtitle; a bare Sxx (season-only release) is
// stripped too. Without a marker, a trailing YYYY.MM.DD air date ends the
// show name the same way because the date and all release tags after it are
// episode-specific; it is never reported as the show's first-air year.
// A season/episode is reported only for a tv category; the marker is stripped
// from movies as well so it cannot leak into their title.
func ParseRelease(name, category string) (title string, year int, season, episode int) {
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if ext := strings.ToLower(path.Ext(base)); videoExtensions[ext] {
		base = strings.TrimSuffix(base, path.Ext(base))
	}
	base = strings.NewReplacer(".", " ", "_", " ").Replace(base)

	datedShow := false
	if isTVCategory(category) {
		if loc := seasonEpisodeRE.FindStringSubmatchIndex(base); loc != nil {
			s, _ := strconv.Atoi(base[loc[2]:loc[3]])
			e, _ := strconv.Atoi(base[loc[4]:loc[5]])
			season, episode = s, e
			// The tail is the episode title, not part of the show name. Keeping
			// it would fail TMDB's strict normalised-title guard.
			base = base[:loc[0]]
		} else {
			if loc := seasonOnlyRE.FindStringIndex(base); loc != nil {
				base = base[:loc[0]] + " " + base[loc[1]:]
			}
			if loc := tvAirDateRE.FindStringIndex(base); loc != nil {
				// Daily-show air date: truncate just like an episode marker so
				// resolution/codec/group tags cannot leak into the show name.
				base = base[:loc[0]]
				datedShow = true
			}
		}
	} else if loc := seasonEpisodeRE.FindStringSubmatchIndex(base); loc != nil {
		// Movies never report a season/episode, but the marker must still be
		// removed so it cannot leak into the title.
		base = base[:loc[0]] + " " + base[loc[1]:]
	}

	// The last four-digit year is the release year: titles such as
	// "Blade Runner 2049 2017" put the film's title number first. A dated
	// daily show has no first-air year in the name, so its year stays 0 and
	// the ±1 year gate does not reject the real (often much older) series.
	if !datedShow {
		if locs := yearRE.FindAllStringIndex(base, -1); len(locs) > 0 {
			loc := locs[len(locs)-1]
			year, _ = strconv.Atoi(base[loc[0]:loc[1]])
			base = base[:loc[0]] + " " + base[loc[1]:]
		}
	}

	base = stripAudioCodecs(base)

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

// stripAudioCodecs removes composite audio tags that the single-token junk
// filter cannot see (DD5.1, DDP5.1, DD+5.1, DTS-HD.MA, bare 5.1/7.1). Bare
// fragment tokens such as HD or MA are removed only when some codec hint is
// elsewhere in the name, so a legitimate title word is never discarded.
func stripAudioCodecs(s string) string {
	s = ddCodecRE.ReplaceAllString(s, " ")
	s = dtsCodecRE.ReplaceAllString(s, " ")
	s = channelCountRE.ReplaceAllString(s, " ")
	if codecHintRE.MatchString(s) {
		s = audioFragmentRE.ReplaceAllString(s, " ")
	}
	return s
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
