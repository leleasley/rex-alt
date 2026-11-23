// Command rex-alt is the entry point for the REX Altmount source add-on.
package main

import (
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/leleasley/rex-alt/internal/api"
)

const (
	defaultHost = "0.0.0.0"
	defaultPort = "9982"
)

func main() {
	host := envOrDefault("APP_HOST", defaultHost)
	port := envOrDefault("APP_PORT", defaultPort)
	if version := os.Getenv("APP_VERSION"); version != "" {
		api.Version = version
	}

	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/health", api.HealthHandler())
	server := &http.Server{
		Addr:              net.JoinHostPort(host, port),
		Handler:           mux,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		ReadHeaderTimeout: 15 * time.Second,
	}

	log.Printf("rex-alt listening on %s (version %s)", server.Addr, api.Version)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
