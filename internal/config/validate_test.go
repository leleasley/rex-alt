package config

import (
	"bytes"
	"strings"
	"testing"
)

// requiredConfig is a Config that satisfies every startup requirement. Each
// case below breaks exactly one field so the failing variable name can be
// asserted in the error.
func requiredConfig() Config {
	return Config{
		APIToken:          strings.Repeat("a", 16),
		EncryptionKey:     bytes.Repeat([]byte{0xA5}, 32),
		RexURL:            "http://rex.local:32400",
		AltmountWebDAVURL: "http://dav.local:8090/webdav/",
	}
}

func TestValidateRequiredAcceptsCompleteConfig(t *testing.T) {
	if err := requiredConfig().ValidateRequired(); err != nil {
		t.Fatalf("ValidateRequired with all required values: %v", err)
	}
}

func TestValidateRequiredRejectsMissingOrInvalidValues(t *testing.T) {
	const shortToken = "shorttoken12345" // 15 chars, deliberately distinctive
	tests := []struct {
		name   string
		field  string
		mutate func(*Config)
	}{
		{"api token empty", "API_TOKEN", func(c *Config) { c.APIToken = "" }},
		{"api token too short", "API_TOKEN", func(c *Config) { c.APIToken = shortToken }},
		{"encryption key empty", "ENCRYPTION_KEY", func(c *Config) { c.EncryptionKey = nil }},
		{"encryption key wrong length", "ENCRYPTION_KEY", func(c *Config) { c.EncryptionKey = bytes.Repeat([]byte{0xA5}, 31) }},
		{"rex url empty", "REX_URL", func(c *Config) { c.RexURL = "" }},
		{"altmount url empty", "ALTMOUNT_WEBDAV_URL", func(c *Config) { c.AltmountWebDAVURL = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := requiredConfig()
			tt.mutate(&cfg)
			err := cfg.ValidateRequired()
			if err == nil {
				t.Fatal("expected error")
			}
			// Errors must name the VARIABLE, never its value.
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("error %q does not name %s", err, tt.field)
			}
			if strings.Contains(err.Error(), shortToken) {
				t.Errorf("error echoed the token value: %q", err)
			}
			if strings.Contains(err.Error(), cfg.RexURL) && cfg.RexURL != "" {
				t.Errorf("error echoed the REX URL: %q", err)
			}
		})
	}
}

func TestValidateRequiredAcceptsMinimumTokenAnd32ByteKey(t *testing.T) {
	cfg := requiredConfig()
	cfg.APIToken = strings.Repeat("z", minAPITokenLength)
	cfg.EncryptionKey = bytes.Repeat([]byte{0x01}, encryptionKeyBytes)
	if err := cfg.ValidateRequired(); err != nil {
		t.Fatalf("boundary values: %v", err)
	}
}
