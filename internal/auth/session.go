package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

const sessionTTL = 24 * time.Hour

// ErrEmptySessionSecret is returned when a session token is requested under an
// empty signing secret. No token is ever issued or accepted under one.
var ErrEmptySessionSecret = errors.New("auth: session secret is empty")

// NewSessionToken creates a signed session token for an admin user.
// Format: base64url(username + "|" + expiry_unix) + "." + base64url(HMAC)
func NewSessionToken(username string, secret []byte) (string, error) {
	if len(secret) == 0 {
		return "", ErrEmptySessionSecret
	}
	expiry := time.Now().Add(sessionTTL).Unix()
	payload := username + "|" + strconv.FormatInt(expiry, 10)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	sig := computeHMAC(encoded, secret)
	return encoded + "." + sig, nil
}

// validateSessionToken verifies the HMAC and expiry. Returns true if valid.
func validateSessionToken(token string, secret []byte) bool {
	// An empty key authenticates nothing: anyone can compute an HMAC under it,
	// so a token that verifies under an empty secret says nothing about who
	// made it. Rejected here so no caller can accept one, however it was wired.
	if len(secret) == 0 {
		return false
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	encoded, sig := parts[0], parts[1]
	computed := computeHMAC(encoded, secret)
	if !hmac.Equal([]byte(computed), []byte(sig)) {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	username, expiryStr, ok := strings.Cut(string(payload), "|")
	if !ok || username == "" {
		return false
	}
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < expiry
}

func computeHMAC(data string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// GenerateSecret creates a 32-byte random secret for session signing.
// Call once at startup and persist to the data directory.
func GenerateSecret() ([]byte, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return b, err
}
