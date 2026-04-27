package erf_test

import (
	"testing"

	"github.com/scrypster/muninndb/internal/storage/erf"
)

func TestPatchTrust(t *testing.T) {
	eng := &erf.Engram{
		Concept:   "test",
		Content:   "hello world",
		CreatedBy: "tester",
	}
	raw, err := erf.Encode(eng)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Default trust is 0 (unset)
	if got := erf.GetTrust(raw); got != 0x00 {
		t.Errorf("initial GetTrust = %d, want 0", got)
	}

	// Patch to verified (0x01)
	if err := erf.PatchTrust(raw, 0x01); err != nil {
		t.Fatalf("PatchTrust: %v", err)
	}
	if got := erf.GetTrust(raw); got != 0x01 {
		t.Errorf("GetTrust after patch = %d, want 1", got)
	}

	// CRC32 must still verify
	if !erf.VerifyCRC32(raw) {
		t.Error("CRC32 invalid after PatchTrust")
	}

	// Decode must succeed and trust survives
	decoded, err := erf.Decode(raw)
	if err != nil {
		t.Fatalf("Decode after PatchTrust: %v", err)
	}
	if decoded.Trust != 0x01 {
		t.Errorf("decoded.Trust = %d, want 1", decoded.Trust)
	}
}

func TestEncodeDecodeTrustRoundtrip(t *testing.T) {
	tests := []struct{ trust uint8 }{
		{0x00}, {0x01}, {0x02}, {0x03}, {0x04},
	}
	for _, tc := range tests {
		eng := &erf.Engram{
			Concept:   "trust-roundtrip",
			Content:   "content",
			CreatedBy: "test",
			Trust:     tc.trust,
		}
		raw, err := erf.Encode(eng)
		if err != nil {
			t.Fatalf("trust=%d Encode: %v", tc.trust, err)
		}
		decoded, err := erf.Decode(raw)
		if err != nil {
			t.Fatalf("trust=%d Decode: %v", tc.trust, err)
		}
		if decoded.Trust != tc.trust {
			t.Errorf("trust=%d: roundtrip got %d", tc.trust, decoded.Trust)
		}
	}
}

func TestPatchTrustTooShort(t *testing.T) {
	if err := erf.PatchTrust(make([]byte, 10), 0x01); err == nil {
		t.Error("expected error for too-short record")
	}
}

func TestGetTrustTooShort(t *testing.T) {
	if got := erf.GetTrust(make([]byte, 10)); got != 0x00 {
		t.Errorf("GetTrust on short record = %d, want 0", got)
	}
}
