package library

import (
	"testing"

	"github.com/leleasley/rex-alt/internal/models"
)

func TestItemIDEqualsWebDAVRelativePath(t *testing.T) {
	e := models.Entry{Category: "movies", Release: "Rel", File: "file.mkv"}
	want := "movies/Rel/file.mkv"
	if got := ItemID(e); got != want {
		t.Errorf("ItemID = %q, want %q", got, want)
	}
	if got := ItemPath(e); got != want {
		t.Errorf("ItemPath = %q, want %q", got, want)
	}
}

func TestItemIDIsReadableForRealReleaseNames(t *testing.T) {
	release := "Train.Dreams.2025.REPACK.2160p.NF.WEB-DL.DDP5.1.Atmos.H.265-XEBEC"
	file := release + ".mkv"
	e := models.Entry{Category: "movies", Release: release, File: file}
	want := "movies/" + release + "/" + file
	if got := ItemID(e); got != want {
		t.Errorf("ItemID = %q, want the readable path %q", got, want)
	}
}

func TestItemIDChangesWithAnyPathPart(t *testing.T) {
	base := models.Entry{Category: "movies", Release: "Rel", File: "file.mkv"}
	cases := map[string]models.Entry{
		"category": {Category: "tv", Release: "Rel", File: "file.mkv"},
		"release":  {Category: "movies", Release: "Other", File: "file.mkv"},
		"file":     {Category: "movies", Release: "Rel", File: "other.mkv"},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ItemID(e); got == ItemID(base) {
				t.Errorf("ItemID = %q, unchanged after %s change", got, name)
			}
		})
	}
}

func TestItemIDDistinguishesFilesInSameRelease(t *testing.T) {
	a := models.Entry{Category: "movies", Release: "Rel", File: "first.mkv"}
	b := models.Entry{Category: "movies", Release: "Rel", File: "second.mkv"}
	if ItemID(a) == ItemID(b) {
		t.Errorf("different files share id %q", ItemID(a))
	}
}
