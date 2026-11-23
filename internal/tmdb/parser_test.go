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
