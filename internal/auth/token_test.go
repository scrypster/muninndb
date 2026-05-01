package auth_test

import (
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

func TestParseBearerToken(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		wantToken string
		wantOK    bool
	}{
		{"valid", "Bearer abc123", "abc123", true},
		{"empty header", "", "", false},
		{"no Bearer prefix", "Token abc123", "", false},
		{"Bearer only no token", "Bearer ", "", false},
		{"Basic auth", "Basic dXNlcjpwYXNz", "", false},
		{"Bearer with spaces in token", "Bearer tok en", "tok en", true},
		{"mk_ key", "Bearer mk_abc123", "mk_abc123", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := auth.ParseBearerToken(tc.header)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.wantToken {
				t.Errorf("token = %q, want %q", got, tc.wantToken)
			}
		})
	}
}

func TestValidateStaticToken(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		required string
		want     bool
	}{
		{"matching", "mdb_secret", "mdb_secret", true},
		{"mismatch", "mdb_wrong", "mdb_secret", false},
		{"empty required (open-server)", "anything", "", false},
		{"empty token", "", "mdb_secret", false},
		{"both empty", "", "", false},
		{"oversized token", strings.Repeat("x", 4097), "mdb_secret", false},
		{"exactly at limit", strings.Repeat("x", 4096), strings.Repeat("x", 4096), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := auth.ValidateStaticToken(tc.token, tc.required); got != tc.want {
				t.Errorf("ValidateStaticToken(%q, %q) = %v, want %v", tc.token, tc.required, got, tc.want)
			}
		})
	}
}

func TestIsValidVaultName(t *testing.T) {
	valid := []string{
		"default", "my-vault", "vault_1", "a", "abc123",
		strings.Repeat("a", 64), // exactly 64
	}
	invalid := []string{
		"", "UPPER", "with space", "with/slash", "../etc/passwd",
		"vault\u200b", // zero-width space
		strings.Repeat("a", 65), // 65 chars
		"vault!", "vault@name",
	}
	for _, name := range valid {
		if !auth.IsValidVaultName(name) {
			t.Errorf("IsValidVaultName(%q) = false, want true", name)
		}
	}
	for _, name := range invalid {
		if auth.IsValidVaultName(name) {
			t.Errorf("IsValidVaultName(%q) = true, want false", name)
		}
	}
}
