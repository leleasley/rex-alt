package api

import (
	"net/http"
	"time"

	"github.com/leleasley/rex-alt/internal/sync"
)

// statusResponse is the shape of GET /api/v1/status. It reports only counts,
// timestamps and generic state: no URL, credential or catalogue item is ever
// exposed through the admin API.
type statusResponse struct {
	Status      string      `json:"status"`
	Version     string      `json:"version"`
	Entries     int         `json:"entries"`
	SyncRunning bool        `json:"sync_running"`
	LastSync    string      `json:"last_sync,omitempty"`
	LastStats   *sync.Stats `json:"last_stats,omitempty"`
	LastError   string      `json:"last_error,omitempty"`
}

// status returns the stored catalogue size plus the outcome of the most recent
// API-triggered sync. The store is read through its public Entries accessor so
// status never needs a new privileged database query.
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	entries := 0
	if s.st != nil {
		list, err := s.st.Entries()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "status unavailable")
			return
		}
		entries = len(list)
	}

	s.mu.Lock()
	resp := statusResponse{
		Status:      "ok",
		Version:     Version,
		Entries:     entries,
		SyncRunning: s.running,
	}
	if !s.lastRun.IsZero() {
		resp.LastSync = s.lastRun.UTC().Format(time.RFC3339)
		stats := s.lastStats
		resp.LastStats = &stats
		resp.LastError = s.lastError
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}
