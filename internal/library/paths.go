package library

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/leleasley/rex-alt/internal/models"
)

const (
	pathSeparator = "/"
	idSeparator   = "|"
	idHexLength   = 24
)

// ItemID is the stable per-source key REX binds watch history to. It hashes
// the three WebDAV path segments so a file keeps its id across re-enumerations
// and so two releases with the same filename do not collide.
func ItemID(e models.Entry) string {
	key := strings.Join([]string{e.Category, e.Release, e.File}, idSeparator)
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:idHexLength]
}

// ItemPath is the file's path relative to the registered WebDAV base, e.g.
// "movies/Release/file.mkv".
func ItemPath(e models.Entry) string {
	return strings.Join([]string{e.Category, e.Release, e.File}, pathSeparator)
}
