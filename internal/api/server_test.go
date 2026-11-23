package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leleasley/rex-alt/internal/config"
	"github.com/leleasley/rex-alt/internal/models"
	"github.com/leleasley/rex-alt/internal/store"
)

const testToken = "testtoken-123456789"

func testConfig() config.Config {
	return config.Config{APIToken: testToken}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func authRequest(method, target, token string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// TestStatusRequiresBearerToken is the brief's first mandated test: no token
// is rejected, the correct token is accepted and the response carries counts.
func TestStatusRequiresBearerToken(t *testing.T) {
	st := testStore(t)
	if err := st.UpsertEntries([]models.Entry{{
		ID: "entry-1", Path: "movies/Foo (2020)/Foo (2020).mkv",
		Category: "movies", Release: "Foo (2020)", File: "Foo (2020).mkv",
	}}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	h := NewServer(testConfig(), nil, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authRequest(http.MethodGet, "/api/v1/status", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	var errBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("401 body is not JSON: %v", err)
	}
	if errBody["error"] == nil || errBody["error"] == "" {
		t.Fatalf("401 missing JSON error: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), testToken) {
		t.Fatal("401 echoed the configured token")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authRequest(http.MethodGet, "/api/v1/status", "wrong-token-123"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authRequest(http.MethodGet, "/api/v1/status", testToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("correct token: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type: got %q", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("200 body is not JSON: %v", err)
	}
	if body["entries"] != float64(1) {
		t.Errorf("entries count: got %v, want 1", body["entries"])
	}
	if body["status"] != "ok" {
		t.Errorf("status: got %v", body["status"])
	}
}

// TestSyncTriggerRateLimited is the brief's second mandated test. The limiter
// is built with a limit of 3 so the 4th call can be asserted quickly; the
// production limit stays at 120/min.
func TestSyncTriggerRateLimited(t *testing.T) {
	lim := newIPLimiter(3, time.Minute)
	h := newServerWithLimiter(testConfig(), nil, nil, lim)

	for i := 1; i <= 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authRequest(http.MethodPost, "/api/v1/sync", testToken))
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("call %d was rate limited too early", i)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authRequest(http.MethodPost, "/api/v1/sync", testToken))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th call: got %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("429 response missing Retry-After header")
	}
	var errBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("429 body is not JSON: %v", err)
	}
	if errBody["error"] == nil || errBody["error"] == "" {
		t.Fatalf("429 missing JSON error: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), testToken) {
		t.Fatal("429 echoed the configured token")
	}
}

func TestHealthIsUnauthenticated(t *testing.T) {
	h := NewServer(testConfig(), nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["version"] == "" {
		t.Fatalf("bad health body: %s", rec.Body.String())
	}
}

func TestProtectedRoutesRejectNonBearerSchemes(t *testing.T) {
	h := NewServer(testConfig(), nil, testStore(t))
	for _, value := range []string{
		"Basic " + testToken,
		"Bearer",
		"Bearer " + testToken + " extra",
		"bearer " + testToken, // scheme is case-insensitive per RFC 7235
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		req.Header.Set("Authorization", value)
		h.ServeHTTP(rec, req)
		want := http.StatusUnauthorized
		if value == "bearer "+testToken {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Errorf("Authorization %q: got %d, want %d", value, rec.Code, want)
		}
	}
}

func TestBearerToken(t *testing.T) {
	for _, tt := range []struct{ header, want string }{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},
		{"BEARER abc", "abc"},
		{"  Bearer   abc  ", "abc"},
		{"Bearer", ""},
		{"Basic abc", ""},
		{"Bearer abc extra", ""},
		{"", ""},
	} {
		if got := bearerToken(tt.header); got != tt.want {
			t.Errorf("bearerToken(%q): got %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestIPLimiterIsPerRemoteAddr(t *testing.T) {
	lim := newIPLimiter(1, time.Minute)
	reqA := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	reqA.RemoteAddr = "10.0.0.1:1111"
	reqB := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	reqB.RemoteAddr = "10.0.0.2:2222"

	if !lim.Allow(clientIP(reqA)) {
		t.Fatal("first request from A should be allowed")
	}
	if lim.Allow(clientIP(reqA)) {
		t.Fatal("second request from A should be denied")
	}
	if !lim.Allow(clientIP(reqB)) {
		t.Fatal("first request from B has its own budget")
	}
}
