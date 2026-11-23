package config

import "fmt"

// ValidateRequired enforces the fail-closed startup requirements that Load
// deliberately does not check. Load must stay permissive so its cleared-env
// defaults test passes; main calls this method before opening any resource and
// refuses to start when a required value is missing or too weak.
//
// Errors name only environment variables — never their values — so a secret
// cannot be echoed through a startup log line.
func (c Config) ValidateRequired() error {
	if len(c.APIToken) < minAPITokenLength {
		return fmt.Errorf("config: API_TOKEN is required and must be at least %d characters", minAPITokenLength)
	}
	if len(c.EncryptionKey) != encryptionKeyBytes {
		return fmt.Errorf("config: ENCRYPTION_KEY is required and must decode to exactly %d bytes", encryptionKeyBytes)
	}
	if c.RexURL == "" {
		return fmt.Errorf("config: REX_URL is required")
	}
	if c.AltmountWebDAVURL == "" {
		return fmt.Errorf("config: ALTMOUNT_WEBDAV_URL is required")
	}
	return nil
}
