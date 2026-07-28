package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// readTrust fetches an engram by id and returns its stored TrustLevel label.
func readTrust(t *testing.T, eng *Engine, vault, id string) string {
	t.Helper()
	resp, err := eng.Read(context.Background(), &mbp.ReadRequest{Vault: vault, ID: id, ReadOnly: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return storage.TrustLevel(resp.Trust).String()
}

// withMode returns a context carrying the given credential mode, as the auth
// middleware / MCP dispatch (S0) would have set it.
func withMode(mode string) context.Context {
	return context.WithValue(context.Background(), auth.ContextMode, mode)
}

// TestWrite_Trust_DefaultsToInferred: S8 must not change the pre-existing
// behaviour — a write that omits trust still lands "inferred".
func TestWrite_Trust_DefaultsToInferred(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	resp, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault: "trust-vault", Content: "no trust specified",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readTrust(t, eng, "trust-vault", resp.ID); got != "inferred" {
		t.Errorf("default trust = %q, want inferred", got)
	}
}

// TestWrite_Trust_VerifiedRejectedUnderObserve: the S8/D4 gate — an observe
// credential may never stamp "verified". The write must fail, not silently
// downgrade (a silent downgrade would let a reader believe the gate held while
// producing a differently-trusted record than requested).
func TestWrite_Trust_VerifiedRejectedUnderObserve(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	_, err := eng.Write(withMode(auth.ModeObserve), &mbp.WriteRequest{
		Vault: "trust-vault", Content: "trying to verify under observe", Trust: "verified",
	})
	if err == nil {
		t.Fatal("expected error setting trust=verified under observe credential, got nil")
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("error = %v, want ErrInvalidRequest", err)
	}
}

// TestWrite_Trust_VerifiedAllowedUnderWriteAndFull: write and full credentials
// may both stamp "verified" (D4: "write/full credential").
func TestWrite_Trust_VerifiedAllowedUnderWriteAndFull(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	for _, mode := range []string{auth.ModeWrite, auth.ModeFull} {
		resp, err := eng.Write(withMode(mode), &mbp.WriteRequest{
			Vault: "trust-vault", Content: "verified under " + mode, Trust: "verified",
		})
		if err != nil {
			t.Fatalf("Write verified under %s: %v", mode, err)
		}
		if got := readTrust(t, eng, "trust-vault", resp.ID); got != "verified" {
			t.Errorf("trust under %s = %q, want verified", mode, got)
		}
	}
}

// TestWrite_Trust_NonVerifiedLevelsUngated: external/untrusted/inferred are not
// privileged — they must succeed even under an observe credential (only
// "verified" is gated).
func TestWrite_Trust_NonVerifiedLevelsUngated(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	for _, level := range []string{"inferred", "external", "untrusted"} {
		resp, err := eng.Write(withMode(auth.ModeObserve), &mbp.WriteRequest{
			Vault: "trust-vault", Content: "level " + level, Trust: level,
		})
		if err != nil {
			t.Fatalf("Write %s under observe: %v", level, err)
		}
		if got := readTrust(t, eng, "trust-vault", resp.ID); got != level {
			t.Errorf("trust = %q, want %q", got, level)
		}
	}
}

// TestWrite_Trust_InvalidRejected: an unrecognized trust label is a client
// error, not a silent default.
func TestWrite_Trust_InvalidRejected(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	_, err := eng.Write(withMode(auth.ModeFull), &mbp.WriteRequest{
		Vault: "trust-vault", Content: "bad trust", Trust: "super-verified",
	})
	if err == nil {
		t.Fatal("expected error for invalid trust label, got nil")
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("error = %v, want ErrInvalidRequest", err)
	}
}

// TestWriteBatch_Trust_PerItemGate: the gate is enforced per item — a bad
// "verified" item fails alone while its siblings still commit.
func TestWriteBatch_Trust_PerItemGate(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	reqs := []*mbp.WriteRequest{
		{Vault: "trust-vault", Content: "batch item ok inferred"},
		{Vault: "trust-vault", Content: "batch item verified under observe", Trust: "verified"},
		{Vault: "trust-vault", Content: "batch item ok external", Trust: "external"},
	}
	responses, errs := eng.WriteBatch(withMode(auth.ModeObserve), reqs)

	if errs[1] == nil {
		t.Error("expected item 1 (verified under observe) to error")
	} else if !errors.Is(errs[1], ErrInvalidRequest) {
		t.Errorf("item 1 error = %v, want ErrInvalidRequest", errs[1])
	}
	if errs[0] != nil {
		t.Errorf("item 0 should commit, got error %v", errs[0])
	}
	if errs[2] != nil {
		t.Errorf("item 2 should commit, got error %v", errs[2])
	}
	if responses[0] == nil || responses[0].ID == "" {
		t.Error("item 0 should have a committed ID")
	}
	if responses[2] == nil || responses[2].ID == "" {
		t.Error("item 2 should have a committed ID")
	}
	if responses[2] != nil && responses[2].ID != "" {
		if got := readTrust(t, eng, "trust-vault", responses[2].ID); got != "external" {
			t.Errorf("item 2 trust = %q, want external", got)
		}
	}
}
