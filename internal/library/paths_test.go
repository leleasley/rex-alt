package library

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/leleasley/rex-alt/internal/models"
)

func TestItemIDStableAndPathIsWebDAVRelative(t *testing.T) {
	e := models.Entry{Category: "movies", Release: "Rel", File: "file.mkv"}
	sum := sha256.Sum256([]byte("movies|Rel|file.mkv"))
	wantID := hex.EncodeToString(sum[:])[:24]
	if got := ItemID(e); got != wantID {
		t.Errorf("ItemID = %q, want %q", got, wantID)
	}
	if got := ItemPath(e); got != "movies/Rel/file.mkv" {
		t.Errorf("ItemPath = %q, want %q", got, "movies/Rel/file.mkv")
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
