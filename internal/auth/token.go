package auth

import (
	"crypto/subtle"
	"strings"
)

// maxStaticTokenLen caps the token length accepted by ValidateStaticToken.
// This prevents a DoS vector where an enormous Authorization header forces a
// large heap allocation before subtle.ConstantTimeCompare can do its own
// length-equality short-circuit. Real tokens are well under 100 bytes.
const maxStaticTokenLen = 4096

// ParseBearerToken extracts the token value from an Authorization header.
// Returns ("", false) if the header is absent or does not begin with "Bearer ".
func ParseBearerToken(header string) (string, bool) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	return token, true
}

// ValidateStaticToken reports whether token matches required in constant time.
// Returns false if required is empty (open-server mode must be handled by the
// caller), if token exceeds maxStaticTokenLen, or if the values differ.
func ValidateStaticToken(token, required string) bool {
	if required == "" || len(token) > maxStaticTokenLen {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(required)) == 1
}

// IsValidVaultName reports whether name is a valid vault identifier:
// 1–64 characters, containing only lowercase letters, digits, hyphens, and underscores.
func IsValidVaultName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
