// Package config loads rex-alt's runtime configuration from the environment.
// It is the only source of configuration: no files, flags or defaults files are
// read. Secrets are never logged; use Redacted before printing a Config.
package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAppPort      = 9982
	defaultDatabasePath = "/data/rex-alt.db"
	defaultSourceName   = "rex-alt"
	defaultProbeWorkers = 4
	maxProbeWorkers     = 8
	defaultProbeTimeout = 45 * time.Second
	defaultHTTPTimeout  = 15 * time.Second
	defaultSyncInterval = 6 * time.Hour
	defaultPushBatch    = 100
	defaultLogLevel     = "info"
	minAPITokenLength   = 16
	encryptionKeyBytes  = 32
	redactionMarker     = "***"
)

// Config is the complete, validated runtime configuration. Later packages
// consume these exact field names. Load is the only constructor.
type Config struct {
	AppPort           int
	PublicURL         string
	DatabasePath      string
	APIToken          string
	EncryptionKey     []byte
	RexURL            string
	RexSourceName     string
	RexSourceUser     string
	RexSourcePassword string
	RexSendTMDBIDs    bool
	AltmountWebDAVURL string
	AltmountUser      string
	AltmountPassword  string
	TMDBReadToken     string
	TMDBAPIKey        string
	ProbeWorkers      int
	ProbeTimeout      time.Duration
	HTTPTimeout       time.Duration
	SyncInterval      time.Duration
	SyncStartHour     int
	SyncMaxMovies     int
	SyncMaxSeries     int
	PushBatch         int
	LogLevel          string
}

// secretFields are the Config fields whose values must never be printed.
var secretFields = []string{
	"APIToken",
	"EncryptionKey",
	"RexSourceUser",
	"RexSourcePassword",
	"AltmountUser",
	"AltmountPassword",
	"TMDBReadToken",
	"TMDBAPIKey",
}

// Load reads the environment and returns a validated Config. Empty variables
// fall back to safe defaults; variables that are set must be well formed.
// Error messages name the variable but never include its value.
func Load() (Config, error) {
	cfg := Config{
		AppPort:       defaultAppPort,
		DatabasePath:  defaultDatabasePath,
		RexSourceName: defaultSourceName,
		ProbeWorkers:  defaultProbeWorkers,
		ProbeTimeout:  defaultProbeTimeout,
		HTTPTimeout:   defaultHTTPTimeout,
		SyncInterval:  defaultSyncInterval,
		SyncStartHour: -1, // -1 disables the start-hour gate
		PushBatch:     defaultPushBatch,
		LogLevel:      defaultLogLevel,
	}

	var err error
	if cfg.AppPort, err = intEnv("APP_PORT", cfg.AppPort); err != nil {
		return Config{}, err
	}
	cfg.PublicURL = os.Getenv("PUBLIC_URL")
	cfg.DatabasePath = stringEnv("DATABASE_PATH", cfg.DatabasePath)
	cfg.APIToken = os.Getenv("API_TOKEN")
	if encoded := os.Getenv("ENCRYPTION_KEY"); encoded != "" {
		if cfg.EncryptionKey, err = decodeKey(encoded); err != nil {
			return Config{}, err
		}
	}

	cfg.RexURL = os.Getenv("REX_URL")
	cfg.RexSourceName = stringEnv("REX_SOURCE_NAME", cfg.RexSourceName)
	cfg.RexSourceUser = os.Getenv("REX_SOURCE_USER")
	cfg.RexSourcePassword = os.Getenv("REX_SOURCE_PASSWORD")
	if cfg.RexSendTMDBIDs, err = boolEnv("REX_SEND_TMDB_IDS", false); err != nil {
		return Config{}, err
	}

	cfg.AltmountWebDAVURL = os.Getenv("ALTMOUNT_WEBDAV_URL")
	// ALTMOUNT_WEBDAV_* is the documented spelling; the shorter aliases are
	// accepted so both the design spec and the Config field names work.
	cfg.AltmountUser = firstEnv("ALTMOUNT_WEBDAV_USER", "ALTMOUNT_USER")
	cfg.AltmountPassword = firstEnv("ALTMOUNT_WEBDAV_PASSWORD", "ALTMOUNT_PASSWORD")

	cfg.TMDBReadToken = firstEnv("TMDB_READ_ACCESS_TOKEN", "TMDB_READ_TOKEN")
	cfg.TMDBAPIKey = os.Getenv("TMDB_API_KEY")

	if cfg.ProbeWorkers, err = intEnv("PROBE_WORKERS", cfg.ProbeWorkers); err != nil {
		return Config{}, err
	}
	if cfg.ProbeTimeout, err = durationEnv("PROBE_TIMEOUT", cfg.ProbeTimeout); err != nil {
		return Config{}, err
	}
	if cfg.HTTPTimeout, err = durationEnv("HTTP_TIMEOUT", cfg.HTTPTimeout); err != nil {
		return Config{}, err
	}
	if cfg.SyncInterval, err = durationEnv("SYNC_INTERVAL", cfg.SyncInterval); err != nil {
		return Config{}, err
	}
	if cfg.SyncStartHour, err = intEnv("SYNC_START_HOUR", cfg.SyncStartHour); err != nil {
		return Config{}, err
	}
	if cfg.SyncMaxMovies, err = intEnv("SYNC_MAX_MOVIES", 0); err != nil {
		return Config{}, err
	}
	if cfg.SyncMaxSeries, err = intEnv("SYNC_MAX_SERIES", 0); err != nil {
		return Config{}, err
	}
	if cfg.PushBatch, err = intEnv("PUSH_BATCH", cfg.PushBatch); err != nil {
		return Config{}, err
	}
	cfg.LogLevel = stringEnv("LOG_LEVEL", cfg.LogLevel)

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.AppPort < 1 || c.AppPort > 65535 {
		return fmt.Errorf("config: APP_PORT must be between 1 and 65535")
	}
	if c.APIToken != "" && len(c.APIToken) < minAPITokenLength {
		return fmt.Errorf("config: API_TOKEN must be at least %d characters", minAPITokenLength)
	}
	for _, u := range []struct{ name, value string }{
		{"REX_URL", c.RexURL},
		{"ALTMOUNT_WEBDAV_URL", c.AltmountWebDAVURL},
		{"PUBLIC_URL", c.PublicURL},
	} {
		if err := checkURL(u.name, u.value); err != nil {
			return err
		}
	}
	if c.ProbeWorkers < 1 || c.ProbeWorkers > maxProbeWorkers {
		return fmt.Errorf("config: PROBE_WORKERS must be between 1 and %d", maxProbeWorkers)
	}
	if c.ProbeTimeout < defaultProbeTimeout {
		return fmt.Errorf("config: PROBE_TIMEOUT must be at least %s", defaultProbeTimeout)
	}
	if c.HTTPTimeout <= 0 {
		return fmt.Errorf("config: HTTP_TIMEOUT must be positive")
	}
	if c.SyncInterval <= 0 {
		return fmt.Errorf("config: SYNC_INTERVAL must be positive")
	}
	if c.SyncStartHour < -1 || c.SyncStartHour > 23 {
		return fmt.Errorf("config: SYNC_START_HOUR must be -1 or between 0 and 23")
	}
	if c.SyncMaxMovies < 0 || c.SyncMaxSeries < 0 {
		return fmt.Errorf("config: SYNC_MAX_MOVIES and SYNC_MAX_SERIES must not be negative")
	}
	if c.PushBatch < 1 {
		return fmt.Errorf("config: PUSH_BATCH must be positive")
	}
	return nil
}

// checkURL rejects anything that is not a plain http(s) URL. In particular a
// URL with userinfo is refused so credentials can never be embedded in one.
func checkURL(name, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("config: %s is not a valid URL", name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("config: %s must use http or https", name)
	}
	if u.User != nil {
		return fmt.Errorf("config: %s must not contain credentials", name)
	}
	if u.Host == "" {
		return fmt.Errorf("config: %s must include a host", name)
	}
	return nil
}

func decodeKey(encoded string) ([]byte, error) {
	value := strings.TrimSpace(encoded)
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil || len(decoded) != encryptionKeyBytes {
		return nil, fmt.Errorf("config: ENCRYPTION_KEY must be base64 of exactly %d bytes", encryptionKeyBytes)
	}
	return decoded, nil
}

func stringEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// firstEnv returns the first non-empty value, letting the canonical name win.
func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

func intEnv(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer", key)
	}
	return n, nil
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a duration such as 45s or 6h", key)
	}
	return d, nil
}

func boolEnv(key string, fallback bool) (bool, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("config: %s must be true or false", key)
	}
	return b, nil
}

// Redacted returns a JSON rendering of the Config with every secret field
// replaced by ***. Safe to write to logs or API responses.
func (c Config) Redacted() string {
	raw, err := json.Marshal(c)
	if err != nil {
		return "{}" // Config contains only JSON-marshallable types.
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "{}"
	}
	for _, key := range secretFields {
		fields[key] = redactionMarker
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return "{}"
	}
	return string(out)
}
