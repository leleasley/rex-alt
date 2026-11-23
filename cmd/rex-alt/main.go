// Command rex-alt is the entry point for the REX Altmount source add-on.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/leleasley/rex-alt/internal/altmount"
	"github.com/leleasley/rex-alt/internal/api"
	"github.com/leleasley/rex-alt/internal/config"
	"github.com/leleasley/rex-alt/internal/rex"
	"github.com/leleasley/rex-alt/internal/store"
	"github.com/leleasley/rex-alt/internal/sync"
	"github.com/leleasley/rex-alt/internal/tmdb"
)

const (
	defaultHost     = "0.0.0.0"
	serverTimeout   = 15 * time.Second
	shutdownTimeout = 15 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// 1. Configuration, fail closed on values a container must provide. Errors
	// from both calls name variables only, never their values.
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if err := cfg.ValidateRequired(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if version := os.Getenv("APP_VERSION"); version != "" {
		api.Version = version
	}

	// 2. State and provider clients. One bounded HTTP client is shared by the
	// WebDAV, TMDB, probe and REX paths; credentials stay in Basic/auth headers.
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("database close: %v", err)
		}
	}()

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	dav := altmount.NewClient(cfg.AltmountWebDAVURL, cfg.AltmountUser, cfg.AltmountPassword, httpClient)
	mdb := tmdb.NewClient(cfg.TMDBReadToken, cfg.TMDBAPIKey, st, httpClient)
	feed := rex.NewClient(cfg.RexURL, cfg.RexSourceName, cfg.RexSourceUser, cfg.RexSourcePassword, cfg.PushBatch)
	svc := sync.New(cfg, st, dav, mdb, feed, nil)

	// 3. Signals first so the sync loop and the HTTP server share one context
	// and both stop on SIGTERM/SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 4. Source registration (source "rex-alt" and the movies/tv directories)
	// is performed exactly once inside SyncOnce. The loop's first pass runs
	// immediately in the background, so registration does not delay the health
	// endpoint; transient failures are logged and retried on the next tick.
	go runSyncLoop(ctx, svc, cfg.SyncInterval, cfg.SyncStartHour)

	// 5. Admin API. APP_HOST is intentionally read here: Config's authoritative
	// field list has no host field, while APP_PORT flows through cfg.AppPort.
	host := envOrDefault("APP_HOST", defaultHost)
	server := &http.Server{
		Addr:              net.JoinHostPort(host, strconv.Itoa(cfg.AppPort)),
		Handler:           api.NewServer(cfg, svc, st),
		ReadTimeout:       serverTimeout,
		WriteTimeout:      serverTimeout,
		ReadHeaderTimeout: serverTimeout,
		IdleTimeout:       serverTimeout,
	}

	served := make(chan error, 1)
	go func() {
		log.Printf("rex-alt listening on %s (version %s)", server.Addr, api.Version)
		served <- server.ListenAndServe()
	}()

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Print("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
	}
	return nil
}

// runSyncLoop performs one pass immediately, then waits interval between
// passes. When SYNC_START_HOUR is set, a pass outside that local hour is
// deferred in hour-sized steps so a coarse interval cannot skip the window.
func runSyncLoop(ctx context.Context, svc *sync.Service, interval time.Duration, startHour int) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			now := time.Now()
			if syncDue(now, startHour) {
				if _, err := svc.SyncOnce(ctx); err != nil {
					log.Printf("scheduled sync failed: %v", err)
				}
				timer.Reset(interval)
			} else {
				log.Printf("scheduled sync deferred until SYNC_START_HOUR=%d (local hour %d)", startHour, now.Hour())
				timer.Reset(retryDelay(interval))
			}
		}
	}
}

// syncDue applies the optional off-peak gate: -1 means no gate.
func syncDue(now time.Time, startHour int) bool {
	return startHour < 0 || now.Hour() == startHour
}

// retryDelay is the shorter cadence used while the start-hour gate is closed.
func retryDelay(interval time.Duration) time.Duration {
	if interval > 0 && interval < time.Hour {
		return interval
	}
	return time.Hour
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
