package nodeutils

import (
	"crypto/rand"
	"encoding/base64"
)

const (
	// ShutdownTokenEnvironmentVariable is the server-only environment variable carrying the
	// credential used to authorize process shutdown.
	ShutdownTokenEnvironmentVariable = "NODE_UTILS_SHUTDOWN_TOKEN"
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
