// local_auth.go provides authentication token validation and key generation for
// local mode.
package agent

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// GenerateLocalToken creates a bearer token for the loopback-only local API.
func GenerateLocalToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate local token: %w", err)
	}
	return "lt_" + hex.EncodeToString(bytes), nil
}

func ValidateLocalToken(expected string, r *http.Request) bool {
	if expected == "" {
		return false
	}
	got := r.Header.Get("X-Local-Token")
	if got == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}
