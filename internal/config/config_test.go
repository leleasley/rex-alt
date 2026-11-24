package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// envKeys enumerates every variable Load reads. Clearing them all keeps tests
// independent of whatever happens to be present in the ambient environment.
var envKeys = []string{
	"APP_PORT",
	"PUBLIC_URL",
	"DATABASE_PATH",
	"API_TOKEN",
	"ENCRYPTION_KEY",
	"REX_URL",
	"REX_SOURCE_NAME",
	"REX_SOURCE_USER",
	"REX_SOURCE_PASSWORD",
	"REX_SEND_TMDB_IDS",
	"ALTMOUNT_WEBDAV_URL",
	"ALTMOUNT_WEBDAV_USER",
	"ALTMOUNT_WEBDAV_PASSWORD",
	"ALTMOUNT_USER",
	"ALTMOUNT_PASSWORD",
	"ALTMOUNT_POLL_INTERVAL",
	"TMDB_READ_ACCESS_TOKEN",
	"TMDB_READ_TOKEN",
	"TMDB_API_KEY",
	"PROBE_WORKERS",
	"PROBE_TIMEOUT",
	"HTTP_TIMEOUT",
	"SYNC_INTERVAL",
	"SYNC_START_HOUR",
	"SYNC_MAX_MOVIES",
	"SYNC_MAX_SERIES",
	"PUSH_BATCH",
	"LOG_LEVEL",
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range envKeys {
		t.Setenv(key, "")
	}
}

// testKey is a valid base64 encoding of exactly 32 bytes.
var testKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xA5}, 32))

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with cleared env: %v", err)
	}
	if cfg.AppPort != 9982 {
		t.Errorf("AppPort: got %d, want 9982", cfg.AppPort)
	}
	if cfg.ProbeWorkers != 4 {
		t.Errorf("ProbeWorkers: got %d, want 4", cfg.ProbeWorkers)
	}
	if cfg.ProbeTimeout != 45*time.Second {
		t.Errorf("ProbeTimeout: got %s, want 45s", cfg.ProbeTimeout)
	}
	if cfg.PushBatch != 100 {
		t.Errorf("PushBatch: got %d, want 100", cfg.PushBatch)
	}
	if cfg.RexSendTMDBIDs {
		t.Error("RexSendTMDBIDs: got true, want false")
	}
	if cfg.SyncInterval != 6*time.Hour {
		t.Errorf("SyncInterval: got %s, want 6h", cfg.SyncInterval)
	}
	if cfg.AltmountPollInterval != 5*time.Minute {
		t.Errorf("AltmountPollInterval: got %s, want 5m", cfg.AltmountPollInterval)
	}
	if cfg.DatabasePath != "/data/rex-alt.db" {
		t.Errorf("DatabasePath: got %q, want /data/rex-alt.db", cfg.DatabasePath)
	}
	if cfg.RexSourceName != "rex-alt" {
		t.Errorf("RexSourceName: got %q, want rex-alt", cfg.RexSourceName)
	}
	if cfg.SyncMaxMovies != 0 || cfg.SyncMaxSeries != 0 {
		t.Errorf("sync limits default to unlimited: got %d/%d", cfg.SyncMaxMovies, cfg.SyncMaxSeries)
	}
}

func TestLoadRejectsCredentialInURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("REX_URL", "http://rex:secret@10.77.77.2:32400")
	if _, err := Load(); err == nil {
		t.Fatal("expected error")
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	c, _ := Load()
	out := c.Redacted()
	for _, s := range []string{c.APIToken, c.RexSourcePassword, c.TMDBReadToken, c.AltmountPassword} {
		if s != "" && strings.Contains(out, s) {
			t.Fatalf("secret leaked: %s", s)
		}
	}
}

func TestLoadRejectsUnsafeURLs(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{"rex userinfo", "REX_URL", "http://rex:secret@10.77.77.2:32400"},
		{"rex non-http scheme", "REX_URL", "ftp://10.77.77.2:32400"},
		{"rex no host", "REX_URL", "http:///path"},
		{"altmount userinfo", "ALTMOUNT_WEBDAV_URL", "https://user:pw@example.com/webdav/"},
		{"altmount non-https scheme", "ALTMOUNT_WEBDAV_URL", "file:///webdav/"},
		{"public url userinfo", "PUBLIC_URL", "http://user:pw@example.com:9982"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tt.key, tt.val)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s", tt.key)
			}
		})
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{"api token too short", "API_TOKEN", "short"},
		{"encryption key not base64", "ENCRYPTION_KEY", "!!!not-base64!!!"},
		{"encryption key wrong length", "ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte("too short"))},
		{"probe workers zero", "PROBE_WORKERS", "0"},
		{"probe workers above cap", "PROBE_WORKERS", "9"},
		{"probe workers not a number", "PROBE_WORKERS", "many"},
		{"probe timeout below floor", "PROBE_TIMEOUT", "44s"},
		{"probe timeout malformed", "PROBE_TIMEOUT", "soon"},
		{"app port out of range", "APP_PORT", "70000"},
		{"sync interval zero", "SYNC_INTERVAL", "0s"},
		{"http timeout zero", "HTTP_TIMEOUT", "0s"},
		{"push batch zero", "PUSH_BATCH", "0"},
		{"sync start hour out of range", "SYNC_START_HOUR", "24"},
		{"sync start hour too low", "SYNC_START_HOUR", "-2"},
		{"sync max movies negative", "SYNC_MAX_MOVIES", "-1"},
		{"sync max series negative", "SYNC_MAX_SERIES", "-1"},
		{"poll interval below floor", "ALTMOUNT_POLL_INTERVAL", "29s"},
		{"poll interval negative", "ALTMOUNT_POLL_INTERVAL", "-1m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tt.key, tt.val)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s", tt.key)
			}
		})
	}
}

func TestLoadAcceptsBoundaryValues(t *testing.T) {
	clearEnv(t)
	t.Setenv("API_TOKEN", strings.Repeat("x", 16))
	t.Setenv("ENCRYPTION_KEY", testKey)
	t.Setenv("PROBE_WORKERS", "8")
	t.Setenv("PROBE_TIMEOUT", "45s")
	t.Setenv("SYNC_START_HOUR", "0")
	t.Setenv("ALTMOUNT_POLL_INTERVAL", "30s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load at boundaries: %v", err)
	}
	if cfg.ProbeWorkers != 8 || cfg.ProbeTimeout != 45*time.Second {
		t.Errorf("boundaries: got workers=%d timeout=%s", cfg.ProbeWorkers, cfg.ProbeTimeout)
	}
	if cfg.AltmountPollInterval != 30*time.Second {
		t.Errorf("AltmountPollInterval: got %s, want 30s", cfg.AltmountPollInterval)
	}
}

func TestLoadAllowsDisablingAltmountPoll(t *testing.T) {
	clearEnv(t)
	t.Setenv("ALTMOUNT_POLL_INTERVAL", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with polling disabled: %v", err)
	}
	if cfg.AltmountPollInterval != 0 {
		t.Errorf("AltmountPollInterval: got %s, want 0 (disabled)", cfg.AltmountPollInterval)
	}
}

func TestLoadReadsEnv(t *testing.T) {
	clearEnv(t)
	vals := map[string]string{
		"APP_PORT":                 "9999",
		"PUBLIC_URL":               "http://rex-alt.lan:9999",
		"DATABASE_PATH":            "/data/custom.db",
		"API_TOKEN":                strings.Repeat("t", 20),
		"ENCRYPTION_KEY":           testKey,
		"REX_URL":                  "http://10.77.77.2:32400",
		"REX_SOURCE_NAME":          "my-rex-alt",
		"REX_SOURCE_USER":          "rexuser",
		"REX_SOURCE_PASSWORD":      "rexpass",
		"REX_SEND_TMDB_IDS":        "true",
		"ALTMOUNT_WEBDAV_URL":      "http://10.77.77.49:8090/webdav/",
		"ALTMOUNT_WEBDAV_USER":     "davuser",
		"ALTMOUNT_WEBDAV_PASSWORD": "davpass",
		"ALTMOUNT_POLL_INTERVAL":   "2m",
		"TMDB_READ_ACCESS_TOKEN":   "tmdbreadtoken",
		"TMDB_API_KEY":             "tmdbapikey",
		"PROBE_WORKERS":            "6",
		"PROBE_TIMEOUT":            "90s",
		"HTTP_TIMEOUT":             "30s",
		"SYNC_INTERVAL":            "12h",
		"SYNC_START_HOUR":          "3",
		"SYNC_MAX_MOVIES":          "10",
		"SYNC_MAX_SERIES":          "2",
		"PUSH_BATCH":               "50",
		"LOG_LEVEL":                "debug",
	}
	for k, v := range vals {
		t.Setenv(k, v)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with full env: %v", err)
	}

	if cfg.AppPort != 9999 || cfg.PublicURL != vals["PUBLIC_URL"] || cfg.DatabasePath != vals["DATABASE_PATH"] {
		t.Errorf("basic fields: %+v", cfg)
	}
	if cfg.APIToken != vals["API_TOKEN"] {
		t.Error("APIToken not loaded")
	}
	wantKey, _ := base64.StdEncoding.DecodeString(testKey)
	if !bytes.Equal(cfg.EncryptionKey, wantKey) {
		t.Error("EncryptionKey not decoded from base64")
	}
	if cfg.RexURL != vals["REX_URL"] || cfg.RexSourceName != vals["REX_SOURCE_NAME"] {
		t.Errorf("rex fields: %+v", cfg)
	}
	if cfg.RexSourceUser != "rexuser" || cfg.RexSourcePassword != "rexpass" || !cfg.RexSendTMDBIDs {
		t.Errorf("rex credentials/flag: %+v", cfg)
	}
	if cfg.AltmountWebDAVURL != vals["ALTMOUNT_WEBDAV_URL"] || cfg.AltmountUser != "davuser" || cfg.AltmountPassword != "davpass" {
		t.Errorf("altmount fields: %+v", cfg)
	}
	if cfg.AltmountPollInterval != 2*time.Minute {
		t.Errorf("AltmountPollInterval: got %s, want 2m", cfg.AltmountPollInterval)
	}
	if cfg.TMDBReadToken != "tmdbreadtoken" || cfg.TMDBAPIKey != "tmdbapikey" {
		t.Errorf("tmdb fields: %+v", cfg)
	}
	if cfg.ProbeWorkers != 6 || cfg.ProbeTimeout != 90*time.Second || cfg.HTTPTimeout != 30*time.Second {
		t.Errorf("probe/http: %+v", cfg)
	}
	if cfg.SyncInterval != 12*time.Hour || cfg.SyncStartHour != 3 || cfg.SyncMaxMovies != 10 || cfg.SyncMaxSeries != 2 {
		t.Errorf("sync: %+v", cfg)
	}
	if cfg.PushBatch != 50 || cfg.LogLevel != "debug" {
		t.Errorf("push/log: %+v", cfg)
	}
}

func TestRedactedMarksEverySecretField(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(cfg.Redacted()), &fields); err != nil {
		t.Fatalf("Redacted is not valid JSON: %v", err)
	}
	for _, key := range secretFields {
		if fields[key] != redactionMarker {
			t.Errorf("%s: got %v, want %s", key, fields[key], redactionMarker)
		}
	}
}

func TestLoadAcceptsEnvAliases(t *testing.T) {
	clearEnv(t)
	t.Setenv("ALTMOUNT_USER", "alias-user")
	t.Setenv("ALTMOUNT_PASSWORD", "alias-password")
	t.Setenv("TMDB_READ_TOKEN", "alias-read-token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with aliases: %v", err)
	}
	if cfg.AltmountUser != "alias-user" || cfg.AltmountPassword != "alias-password" {
		t.Errorf("altmount aliases: got %q/%q", cfg.AltmountUser, cfg.AltmountPassword)
	}
	if cfg.TMDBReadToken != "alias-read-token" {
		t.Errorf("TMDB alias: got %q", cfg.TMDBReadToken)
	}
}

func TestRedactedHidesConfiguredSecrets(t *testing.T) {
	clearEnv(t)
	secrets := map[string]string{
		"API_TOKEN":                strings.Repeat("A", 24),
		"ENCRYPTION_KEY":           testKey,
		"REX_SOURCE_USER":          "secret-rex-user",
		"REX_SOURCE_PASSWORD":      "secret-rex-password",
		"ALTMOUNT_WEBDAV_USER":     "secret-dav-user",
		"ALTMOUNT_WEBDAV_PASSWORD": "secret-dav-password",
		"TMDB_READ_ACCESS_TOKEN":   "secret-tmdb-read-token",
		"TMDB_API_KEY":             "secret-tmdb-api-key",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := cfg.Redacted()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Fatal("secret leaked")
		}
	}
	wantKey, _ := base64.StdEncoding.DecodeString(testKey)
	if strings.Contains(out, string(wantKey)) {
		t.Fatal("decoded encryption key leaked")
	}
	if !strings.Contains(out, "***") {
		t.Fatalf("redaction marker missing: %s", out)
	}
	if !strings.Contains(out, "rex-alt") || !strings.Contains(out, "9982") {
		t.Fatalf("non-secret fields missing from redacted output: %s", out)
	}
}
