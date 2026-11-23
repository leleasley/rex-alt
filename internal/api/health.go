// Package api exposes the small administrative HTTP surface of rex-alt.
package api

import (
	"encoding/json"
	"net/http"
)

// Version is the value reported as "version" by the health endpoint.
// main overrides it from APP_VERSION; "dev" keeps standalone use meaningful.
var Version = "dev"

// HealthHandler returns a handler that reports service liveness and the
// running build version as {"status":"ok","version":"..."}. The concrete
// http.HandlerFunc return type keeps the result callable in tests while still
// satisfying http.Handler everywhere it is registered.
func HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"version": Version,
		})
	}
}
