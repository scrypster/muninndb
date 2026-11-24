package auth

import (
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

// TestIsWorkflowVaultName pins the structural guard the whole workflow-vault
// lifecycle hangs on (#973): creation, expiry stamping, status notices, and
// the reclamation sweep all ask this one predicate, and an operator vault
// must be incapable of matching it. The "wf-" prefix alone is not enough —
// "wf-" with no body is a bare namespace, and a name that fails
// IsValidVaultName (uppercase, punctuation, over-long) must never be treated
// as reclaimable workflow state.
func TestIsWorkflowVaultName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"wf-demo", true},
		{"wf-a1b2c3d4", true},
		{"wf-with_underscore", true},
		{"wf-", false},        // prefix with no body
		{"wf", false},         // prefix misspelled
		{"default", false},    // operator vault
		{"production", false}, // operator vault
		{"WF-upper", false},   // case-sensitive prefix; also invalid vault name
		{"", false},
		{"swf-demo", false}, // prefix must be at the start
		{"wf-UPPER", false}, // body must satisfy IsValidVaultName
		{"wf-sp ace", false},
		{maxLenWFName(t), true},        // 64 chars total: the longest legal name
		{maxLenWFName(t) + "x", false}, // 65 chars: one over the limit
	}
	for _, tc := range cases {
		if got := IsWorkflowVaultName(tc.name); got != tc.want {
			t.Errorf("IsWorkflowVaultName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// maxLenWFName builds a 64-character wf- name (the IsValidVaultName maximum).
func maxLenWFName(t *testing.T) string {
	t.Helper()
	name := "wf-"
	for len(name) < 64 {
		name += "a"
	}
	if len(name) != 64 {
		t.Fatalf("test helper built a %d-char name, want 64", len(name))
	}
	return name
}

// TestVaultConfigExpiresAtRoundTrip pins the persistence of the optional
// workflow-vault expiry: set through SetVaultConfig, read back through
// GetVaultConfig with the instant intact, and absent (nil) when never set —
// an absent expiry is never a delete reason, so the nil case is load-bearing.
func TestVaultConfigExpiresAtRoundTrip(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db)

	exp := time.Date(2026, 11, 30, 9, 0, 0, 0, time.UTC)
	if err := store.SetVaultConfig(VaultConfig{Name: "wf-roundtrip", ExpiresAt: &exp}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	got, err := store.GetVaultConfig("wf-roundtrip")
	if err != nil {
		t.Fatalf("GetVaultConfig: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt round-trip = %v, want %v", got.ExpiresAt, exp)
	}

	// A config with no expiry decodes to nil, not zero-time.
	if err := store.SetVaultConfig(VaultConfig{Name: "wf-noexpiry"}); err != nil {
		t.Fatalf("SetVaultConfig (no expiry): %v", err)
	}
	got, err = store.GetVaultConfig("wf-noexpiry")
	if err != nil {
		t.Fatalf("GetVaultConfig (no expiry): %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v after never setting one, want nil (absent expiry must stay absent)", got.ExpiresAt)
	}
}
