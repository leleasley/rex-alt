package altmount

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/leleasley/rex-alt/internal/library"
	"github.com/leleasley/rex-alt/internal/models"
)

func catalogueEntry(category, release, file string) models.Entry {
	e := models.Entry{Category: category, Release: release, File: file}
	e.Path = library.ItemPath(e)
	e.ID = library.ItemID(e)
	return e
}

func numberedCatalogue(n int) []models.Entry {
	out := make([]models.Entry, n)
	for i := range out {
		out[i] = catalogueEntry("movies", "Rel", fmt.Sprintf("film%02d.mkv", i))
	}
	return out
}

func TestDiffRefusesEmptyOrTruncatedCatalogue(t *testing.T) {
	old := numberedCatalogue(10)

	t.Run("empty next", func(t *testing.T) {
		for _, next := range [][]models.Entry{nil, {}} {
			added, changed, removed, err := Diff(old, next)
			if err == nil {
				t.Fatalf("Diff with empty next returned added=%v changed=%v removed=%v, want an error", added, changed, removed)
			}
			if added != nil || changed != nil || removed != nil {
				t.Errorf("Diff returned slices alongside an error: %v %v %v", added, changed, removed)
			}
		}
	})

	t.Run("60 percent removed", func(t *testing.T) {
		// Keep only 4 of 10 ids: removing 6 is more than len(old)/2.
		next := append([]models.Entry(nil), old[:4]...)
		added, changed, removed, err := Diff(old, next)
		if err == nil {
			t.Fatalf("Diff returned added=%v changed=%v removed=%v, want a truncation error", added, changed, removed)
		}
		if added != nil || changed != nil || removed != nil {
			t.Errorf("Diff returned slices alongside an error: %v %v %v", added, changed, removed)
		}
	})

	t.Run("exactly half removed is allowed", func(t *testing.T) {
		next := append([]models.Entry(nil), old[:5]...)
		_, _, removed, err := Diff(old, next)
		if err != nil {
			t.Fatalf("Diff with 5 of 10 removed: %v", err)
		}
		if len(removed) != 5 {
			t.Errorf("removed = %d, want 5", len(removed))
		}
	})

	t.Run("empty old is allowed", func(t *testing.T) {
		next := numberedCatalogue(1)
		added, _, _, err := Diff(nil, next)
		if err != nil {
			t.Fatalf("Diff(nil, one entry): %v", err)
		}
		if len(added) != 1 {
			t.Errorf("added = %d, want 1", len(added))
		}
	})
}

func TestDiffDetectsAddedAndChanged(t *testing.T) {
	oldFile := catalogueEntry("movies", "Rel", "file.mkv")
	stable := catalogueEntry("movies", "Other", "stable.mkv")
	old := []models.Entry{oldFile, stable}

	// A replaced file keeps its id but presents a new filename (the nearest
	// thing to a "new size" in models.Entry, which has no size field).
	replaced := models.Entry{
		ID: oldFile.ID, Path: "movies/Rel/file-v2.mkv",
		Category: "movies", Release: "Rel", File: "file-v2.mkv",
	}
	fresh := catalogueEntry("tv", "Show", "S01E01.mkv")
	next := []models.Entry{stable, replaced, fresh}

	added, changed, removed, err := Diff(old, next)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(added) != 1 || added[0].ID != fresh.ID {
		t.Errorf("added = %+v, want just %s", added, fresh.ID)
	}
	if len(changed) != 1 || changed[0].ID != replaced.ID {
		t.Errorf("changed = %+v, want just %s", changed, replaced.ID)
	} else if len(changed) == 1 && changed[0].Path != replaced.Path {
		t.Errorf("changed path = %q, want %q", changed[0].Path, replaced.Path)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %+v, want none", removed)
	}

	t.Run("season and episode changes count too", func(t *testing.T) {
		before := []models.Entry{stable}
		after := []models.Entry{stable, stable}
		after[1].Season = 2
		after[1].Episode = 4
		_, changed, _, err := Diff(before, after)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		// Duplicate ids collapse to one entry; the last one wins.
		if len(changed) != 1 || changed[0].Season != 2 || changed[0].Episode != 4 {
			t.Errorf("changed = %+v, want season 2 episode 4", changed)
		}
	})
}

func TestDiffIsPureAndSorted(t *testing.T) {
	old := numberedCatalogue(4)
	next := append([]models.Entry(nil), old...)
	next[0].Release = "Renamed"
	next = append(next, catalogueEntry("tv", "Show", "new.mkv"))
	oldCopy := append([]models.Entry(nil), old...)
	nextCopy := append([]models.Entry(nil), next...)

	added, changed, removed, err := Diff(old, next)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !reflect.DeepEqual(old, oldCopy) || !reflect.DeepEqual(next, nextCopy) {
		t.Error("Diff mutated one of its input slices")
	}
	if len(removed) != 0 {
		t.Errorf("removed = %+v, want none", removed)
	}
	for _, group := range [][]models.Entry{added, changed} {
		for i := 1; i < len(group); i++ {
			if group[i-1].ID > group[i].ID {
				t.Errorf("results are not sorted by id: %+v", group)
			}
		}
	}
}

func TestVideoExtensionFilter(t *testing.T) {
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"film.mkv", true},
		{"film.MKV", true},
		{"film.m2ts", true},
		{"film.ts", true},
		{"film.webm", true},
		{"film.srt", false},
		{"film.nfo", false},
		{"film.iso", false},
		{"film.m3u8", false},
		{"sample.mkv", false},
		{"film-sample.mkv", false},
		{"Sample.avi", false},
		{"sampled.avi", true},
	} {
		if got := isVideoFile(tc.file); got != tc.want {
			t.Errorf("isVideoFile(%q) = %v, want %v", tc.file, got, tc.want)
		}
	}
}
