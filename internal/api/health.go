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
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"version": Version,
		})
	}
}

// writeJSON is the single JSON response writer for every admin route, so a
// successful body and an error body can never differ in content type.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeJSONError emits a fixed, secret-free message. Callers must never pass
// request data or configuration values as message.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
