package tmdb

import "testing"

func TestParseReleaseMovie(t *testing.T) {
	cases := []struct {
		name       string
		category   string
		wantTitle  string
		wantYear   int
		wantSeason int
		wantEp     int
	}{
		{
			name:      "Swallowed.2022.1080p.BluRay.x264-DST",
			category:  "movies",
			wantTitle: "Swallowed",
			wantYear:  2022,
		},
		{
			name:      "Movie.Name.2019.720p.WEB-DL.x264-GROUP.mkv",
			category:  "movies",
			wantTitle: "Movie Name",
			wantYear:  2019,
		},
		{
			name:      "The.Thing.1982.2160p.UHD.BluRay.Remux.HEVC-DST",
			category:  "movies",
			wantTitle: "The Thing",
			wantYear:  1982,
		},
		{
			name:      "Movie.Title.(2022).1080p.BluRay-DST",
			category:  "movies",
			wantTitle: "Movie Title",
			wantYear:  2022,
		},
		{
			name:      "Swallowed.2022.1080p.Blueray.x264-DST",
			category:  "movies",
			wantTitle: "Swallowed",
			wantYear:  2022,
		},
		{
			name:      "Movie.Title.2022.1080p.Blu-Ray.x264-DST",
			category:  "movies",
			wantTitle: "Movie Title",
			wantYear:  2022,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, year, season, ep := ParseRelease(tc.name, tc.category)
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
			if year != tc.wantYear {
				t.Errorf("year = %d, want %d", year, tc.wantYear)
			}
			if season != tc.wantSeason {
				t.Errorf("season = %d, want %d", season, tc.wantSeason)
			}
			if ep != tc.wantEp {
				t.Errorf("episode = %d, want %d", ep, tc.wantEp)
			}
		})
	}
}

// Audio-codec tags must not survive into the title: the TMDB guard requires an
// exact normalised-title match, so a leftover "DD5 1" prevents enrichment.
func TestParseReleaseStripsAudioCodecs(t *testing.T) {
	cases := []struct {
		name      string
		wantTitle string
		wantYear  int
	}{
		// Live-verification finding and its DDP / DD+ / DTS-HD.MA variants.
		{"10.Cloverfield.Lane.2016.720p.BluRay.DD5.1.x264-HiDt", "10 Cloverfield Lane", 2016},
		{"10.Cloverfield.Lane.2016.720p.BluRay.DDP5.1.x264-HiDt", "10 Cloverfield Lane", 2016},
		{"10.Cloverfield.Lane.2016.720p.BluRay.DD+5.1.x264-HiDt", "10 Cloverfield Lane", 2016},
		{"10.Cloverfield.Lane.2016.720p.BluRay.DTS-HD.MA.5.1.x264-HiDt", "10 Cloverfield Lane", 2016},
		// Spaced forms and other common audio tags.
		{"Movie.Title.2020.1080p.BluRay.DTS-HD.x264-GROUP", "Movie Title", 2020},
		{"Movie.Title.2020.1080p.WEB.DD5 1.x264-GROUP", "Movie Title", 2020},
		{"Movie.Title.2020.1080p.WEB.TrueHD.7.1.Atmos.x264-GROUP", "Movie Title", 2020},
		{"Movie.Title.2020.1080p.WEB.EAC3.AAC.FLAC-GROUP", "Movie Title", 2020},
		// Controls: a plain title still parses, and leading title numbers survive.
		{"Swallowed.2022.1080p.BluRay.x264-DST", "Swallowed", 2022},
		{"12.12.12.2022.720p.WEB-DL.x264-GROUP", "12 12 12", 2022},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, year, season, ep := ParseRelease(tc.name, "movies")
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
			if year != tc.wantYear {
				t.Errorf("year = %d, want %d", year, tc.wantYear)
			}
			if season != 0 || ep != 0 {
				t.Errorf("season/episode = %d/%d, want 0/0", season, ep)
			}
		})
	}
}

func TestParseReleaseEpisode(t *testing.T) {
	title, year, season, ep := ParseRelease(
		"Something.Very.Bad.is.Going.to.Happen.S01E03.2160p.WEB.h265-ETHEL", "tv")
	if title != "Something Very Bad is Going to Happen" {
		t.Errorf("title = %q, want %q", title, "Something Very Bad is Going to Happen")
	}
	if year != 0 {
		t.Errorf("year = %d, want 0", year)
	}
	if season != 1 {
		t.Errorf("season = %d, want 1", season)
	}
	if ep != 3 {
		t.Errorf("episode = %d, want 3", ep)
	}
}

func TestParseReleaseDoesNotReadSeasonFromMovieName(t *testing.T) {
	title, year, season, ep := ParseRelease("Swallowed.2022.1080p.S01E02.BluRay-DST", "movies")
	if title != "Swallowed" {
		t.Errorf("title = %q, want Swallowed", title)
	}
	if year != 2022 || season != 0 || ep != 0 {
		t.Errorf("got year=%d season=%d episode=%d, want 2022/0/0", year, season, ep)
	}
}

// Live-discovered root cause: everything after the SxxExx marker is the
// episode-specific subtitle, not part of the show name. Keeping it makes the
// TMDB normalised-title guard reject the hit, so these episodes get no ids or
// artwork. The title must stop at the marker and the marker must be reported.
func TestParseReleaseTVEpisodeTitleStopsAtMarker(t *testing.T) {
	cases := []struct {
		name       string
		wantTitle  string
		wantSeason int
		wantEp     int
	}{
		{
			name:       "12.12.12.S01E01.Im.Robbing.the.House.on.Christmas.Eve.While.the.Familys.at.Mass.1080p.ATVP.WEB-DL.DDP5.1.Atmos.H.264-RAWR.mkv",
			wantTitle:  "12 12 12",
			wantSeason: 1,
			wantEp:     1,
		},
		{
			name:       "Something.Very.Bad.is.Going.to.Happen.S123E04.2160p.WEB.h265-ETHEL",
			wantTitle:  "Something Very Bad is Going to Happen",
			wantSeason: 123,
			wantEp:     4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, year, season, ep := ParseRelease(tc.name, "tv")
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
			if year != 0 {
				t.Errorf("year = %d, want 0", year)
			}
			if season != tc.wantSeason || ep != tc.wantEp {
				t.Errorf("season/episode = %d/%d, want %d/%d", season, ep, tc.wantSeason, tc.wantEp)
			}
		})
	}
}

// A season-only release has no episode subtitle, but the bare Sxx token must
// still be removed from the show name.
func TestParseReleaseTVSeasonOnlyDropsBareSeason(t *testing.T) {
	title, year, season, ep := ParseRelease("The.Simpsons.S01.1080p.Web-DL.x264-OFT.mkv", "tv")
	if title != "The Simpsons" {
		t.Errorf("title = %q, want %q", title, "The Simpsons")
	}
	if year != 0 || season != 0 || ep != 0 {
		t.Errorf("got year=%d season=%d episode=%d, want 0/0/0", year, season, ep)
	}
}

// Daily wrestling shows carry an episode air date instead of an SxxExx
// marker. "2026.11.23" is the episode's air date, not the show's first-air
// year: it (and the release tags after it) must be stripped so the show name
// reaches TMDB clean and the year stays 0 (no year gate). For the known WWE
// brands the date yields season = year - firstAirYear + 1 and
// episode = ISOWeek(date) - 1; other dated shows stay at 0/0.
func TestParseReleaseDatedDailyShow(t *testing.T) {
	cases := []struct {
		name, wantTitle string
		wantSeason      int
		wantEp          int
	}{
		{
			name:       "WWE.Monday.Night.RAW.2026.11.23.1080p.WEBRip.h265-Star.mp4",
			wantTitle:  "WWE Monday Night RAW",
			wantSeason: 34,
			wantEp:     47, // 2026 is ISO week 48; Raw airs one episode per week
		},
		{
			name:       "WWE.Friday.Night.SmackDown.2026.11.20.1080p.WEBRip.AAC2.0.HFR.H.265-SkY.mkv",
			wantTitle:  "WWE Friday Night SmackDown",
			wantSeason: 28, // 2026 - 1999 + 1
			wantEp:     46,
		},
		{
			name:       "WWE.RAW.2026.11.16.1080p.WEBRip.x264-DEF.mkv",
			wantTitle:  "WWE RAW",
			wantSeason: 34,
			wantEp:     46, // ISO week 47 - 1
		},
		// Dash and space separators for the same air date.
		{
			name:       "WWE.RAW.2026-11-16.1080p.WEBRip.x264-DEF.mkv",
			wantTitle:  "WWE RAW",
			wantSeason: 34,
			wantEp:     46,
		},
		{
			name:       "WWE.NXT.2026 11 18.1080p.WEB-DL.x264-GRP.mkv",
			wantTitle:  "WWE NXT",
			wantSeason: 17, // 2026 - 2010 + 1
			wantEp:     46,
		},
		// The 1993 pilot is the very first Raw episode: season 1, episode 1.
		{
			name:       "WWE.Monday.Night.Raw.1993.01.11.1080p.WEBRip.x264-GRP.mkv",
			wantTitle:  "WWE Monday Night Raw",
			wantSeason: 1,
			wantEp:     1,
		},
		// A dated show that is not a known WWE brand keeps the old behaviour.
		{
			name:       "Some.Other.Daily.Show.2026.11.23.1080p.WEB-DL-GRP.mkv",
			wantTitle:  "Some Other Daily Show",
			wantSeason: 0,
			wantEp:     0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, year, season, ep := ParseRelease(tc.name, "tv")
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
			if year != 0 {
				t.Errorf("year = %d, want 0 (air date is not a first-air year)", year)
			}
			if season != tc.wantSeason || ep != tc.wantEp {
				t.Errorf("season/episode = %d/%d, want %d/%d", season, ep, tc.wantSeason, tc.wantEp)
			}
		})
	}
}

// The date rule is limited to TV releases without an SxxExx marker: an
// ordinary episode keeps its marker-derived season/episode and truncated
// title even when an air date follows it.
func TestParseReleaseDatedEpisodeKeepsMarkerBehaviour(t *testing.T) {
	title, year, season, ep := ParseRelease(
		"Show.Name.S02E04.2026.11.23.1080p.WEB-DL-GRP.mkv", "tv")
	if title != "Show Name" || year != 0 || season != 2 || ep != 4 {
		t.Errorf("got title=%q year=%d season=%d episode=%d, want Show Name/0/2/4",
			title, year, season, ep)
	}
}

// Movie controls: the TV-only truncation/stripping must not disturb movies,
// including titles that start with digits and carry audio codec tags.
func TestParseReleaseMovieControlUnaffected(t *testing.T) {
	cases := []struct {
		name      string
		wantTitle string
		wantYear  int
	}{
		{"10.Cloverfield.Lane.2016.720p.BluRay.DDP5.1.x264-HiDt", "10 Cloverfield Lane", 2016},
		{"12.12.12.2022.720p.WEB-DL.x264-GROUP.mkv", "12 12 12", 2022},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, year, season, ep := ParseRelease(tc.name, "movies")
			if title != tc.wantTitle || year != tc.wantYear || season != 0 || ep != 0 {
				t.Errorf("got title=%q year=%d season=%d episode=%d, want %q/%d/0/0",
					title, year, season, ep, tc.wantTitle, tc.wantYear)
			}
		})
	}
}
