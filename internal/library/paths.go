package library

import (
	"strings"

	"github.com/leleasley/rex-alt/internal/models"
)

const pathSeparator = "/"

// ItemID is the stable per-source key REX binds watch history to. It is the
// WebDAV-relative path: unique per source file, permanent across re-enumerations,
// URL-safe once REX percent-encodes path segments, and readable in the feed.
// The release directory is part of the path, so two releases with the same
// filename still get different ids.
func ItemID(e models.Entry) string {
	return ItemPath(e)
}

// ItemPath is the file's path relative to the registered WebDAV base, e.g.
// "movies/Release/file.mkv".
func ItemPath(e models.Entry) string {
	return strings.Join([]string{e.Category, e.Release, e.File}, pathSeparator)
}
