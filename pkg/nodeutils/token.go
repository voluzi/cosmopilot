package nodeutils

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

const (
	// ShutdownTokenEnvironmentVariable is the server-only environment variable carrying the
	// credential used to authorize process shutdown.
	ShutdownTokenEnvironmentVariable = "NODE_UTILS_SHUTDOWN_TOKEN"
	// ExpectedShutdownTokenHashEnvironmentVariable carries the Pod-stamped credential identity.
	ExpectedShutdownTokenHashEnvironmentVariable = "NODE_UTILS_EXPECTED_SHUTDOWN_TOKEN_HASH"
	// ShutdownTokenSecretKey is the Kubernetes Secret data key used by the operator.
	ShutdownTokenSecretKey = "shutdown-token"
	shutdownTokenBytes     = 32
)

// GenerateShutdownToken returns a 256-bit cryptographically random base64url credential.
func GenerateShutdownToken() (string, error) {
	raw := make([]byte, shutdownTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ValidShutdownToken reports whether token has the canonical generated credential format.
func ValidShutdownToken(token string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(raw) == shutdownTokenBytes && base64.RawURLEncoding.EncodeToString(raw) == token
}

// ShutdownTokenHash returns the canonical SHA-256 digest for a shutdown credential.
func ShutdownTokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

// ValidShutdownTokenHash reports whether hash is a canonical lowercase SHA-256 digest.
func ValidShutdownTokenHash(hash string) bool {
	raw, err := hex.DecodeString(hash)
	return err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == hash
}
