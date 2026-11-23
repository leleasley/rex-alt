package api

import (
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"strings"
	stdsync "sync"
	"time"

	"github.com/leleasley/rex-alt/internal/config"
	"github.com/leleasley/rex-alt/internal/store"
	"github.com/leleasley/rex-alt/internal/sync"
)

const (
	// defaultRateLimit is the per-IP budget for authenticated admin calls in
	// one rateWindow. Health is unlimited so container probes never trip it.
	defaultRateLimit = 120
	rateWindow       = time.Minute
	retryAfter       = "60"
)

// server holds the state shared by the admin handlers. It is built once per
// NewServer call; the sync trigger and status endpoint coordinate through mu
// so status can report what the last run actually did.
type server struct {
	cfg config.Config
	svc *sync.Service
	st  *store.Store
	lim *ipLimiter

	mu        stdsync.Mutex
	running   bool
	lastRun   time.Time
	lastStats sync.Stats
	lastError string
}

// NewServer assembles the admin API: an unauthenticated liveness route for
// container probes and two Bearer-protected, per-IP rate-limited routes.
func NewServer(cfg config.Config, svc *sync.Service, st *store.Store) http.Handler {
	return newServerWithLimiter(cfg, svc, st, newIPLimiter(defaultRateLimit, rateWindow))
}

// newServerWithLimiter is the test seam: tests inject a small budget so the
// rate-limit behaviour can be asserted without 120 requests.
func newServerWithLimiter(cfg config.Config, svc *sync.Service, st *store.Store, lim *ipLimiter) http.Handler {
	if lim == nil {
		lim = newIPLimiter(defaultRateLimit, rateWindow)
	}
	s := &server{cfg: cfg, svc: svc, st: st, lim: lim}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", HealthHandler())
	mux.Handle("GET /api/v1/status", s.auth(http.HandlerFunc(s.status)))
	mux.Handle("POST /api/v1/sync", s.auth(http.HandlerFunc(s.triggerSync)))
	return mux
}

// auth rejects anything that is not a Bearer token equal to API_TOKEN, then
// applies the per-IP budget. Authentication runs first so an attacker without
// the token cannot consume a victim's quota or learn rate-limit state.
func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tokenMatches(bearerToken(r.Header.Get("Authorization")), s.cfg.APIToken) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !s.lim.Allow(clientIP(r)) {
			w.Header().Set("Retry-After", retryAfter)
			writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken accepts the RFC 7235 scheme in any case and exactly one token.
// A malformed or missing header yields an empty string, which tokenMatches
// always rejects even when API_TOKEN itself is empty.
func bearerToken(header string) string {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return fields[1]
}

// tokenMatches compares in constant time. The empty case is rejected up front
// so a disabled/misconfigured token can never be matched by an empty header.
func tokenMatches(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// clientIP uses the transport peer address. Forwarded-* headers are ignored:
// they are client-controlled and would let one caller evade the per-IP budget.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// triggerSync runs one pipeline pass and records its outcome for status. Only
// generic errors leave this handler; the detailed error goes to the log, whose
// producers already redact credentials.
func (s *server) triggerSync(w http.ResponseWriter, r *http.Request) {
	if s.svc == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "sync unavailable")
		return
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict, "sync already running")
		return
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	stats, err := s.svc.SyncOnce(r.Context())
	s.mu.Lock()
	s.lastRun = time.Now()
	if err != nil {
		s.lastStats = sync.Stats{}
		s.lastError = "sync failed"
		s.mu.Unlock()
		log.Printf("admin sync failed: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "sync failed")
		return
	}
	s.lastStats = stats
	s.lastError = ""
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "stats": stats})
}

// ipLimiter is a fixed-window sliding log keyed by client IP. A timestamp log
// (rather than a counter with a reset time) keeps the limit exact across the
// window boundary without a background goroutine.
type ipLimiter struct {
	mu     stdsync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	if limit < 1 {
		limit = 1
	}
	if window <= 0 {
		window = rateWindow
	}
	return &ipLimiter{limit: limit, window: window, hits: make(map[string][]time.Time)}
}

func (l *ipLimiter) Allow(ip string) bool {
	now := time.Now()
	cutoff := now.Add(-l.window)
	l.mu.Lock()
	defer l.mu.Unlock()

	recent := l.recent(ip, cutoff)
	if len(recent) >= l.limit {
		l.hits[ip] = recent
		return false
	}
	l.hits[ip] = append(recent, now)
	l.prune(cutoff)
	return true
}

// recent returns only timestamps inside the current window, reusing the
// backing array so steady-state traffic does not allocate per request. The
// caller must hold mu.
func (l *ipLimiter) recent(ip string, cutoff time.Time) []time.Time {
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

// prune bounds memory in a long-running process that sees many one-off IPs.
// The caller must hold mu.
func (l *ipLimiter) prune(cutoff time.Time) {
	if len(l.hits) <= 1024 {
		return
	}
	for ip := range l.hits {
		if len(l.recent(ip, cutoff)) == 0 {
			delete(l.hits, ip)
		}
	}
}
