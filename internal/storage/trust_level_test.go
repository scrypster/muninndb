package storage_test

import (
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

func TestParseTrustLevel(t *testing.T) {
	tests := []struct {
		input   string
		want    storage.TrustLevel
		wantErr bool
	}{
		{"verified", storage.TrustVerified, false},
		{"inferred", storage.TrustInferred, false},
		{"external", storage.TrustExternal, false},
		{"untrusted", storage.TrustUntrusted, false},
		{"bogus", 0, true},
		{"", 0, true},
	}
	for _, tc := range tests {
		got, err := storage.ParseTrustLevel(tc.input)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseTrustLevel(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("ParseTrustLevel(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestTrustLevelString(t *testing.T) {
	tests := []struct {
		level storage.TrustLevel
		want  string
	}{
		{storage.TrustUnset, "inferred"},
		{storage.TrustVerified, "verified"},
		{storage.TrustInferred, "inferred"},
		{storage.TrustExternal, "external"},
		{storage.TrustUntrusted, "untrusted"},
		{storage.TrustLevel(99), "inferred"}, // unknown → inferred
	}
	for _, tc := range tests {
		if got := tc.level.String(); got != tc.want {
			t.Errorf("TrustLevel(%d).String() = %q, want %q", tc.level, got, tc.want)
		}
	}
}
