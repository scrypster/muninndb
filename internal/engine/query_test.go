package engine

import (
	"context"
	"testing"
)

// TestEngineGetContradictions_Empty verifies that querying contradictions on a
// fresh vault returns an empty slice without error.
func TestEngineGetContradictions_Empty(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	pairs, err := eng.GetContradictions(ctx, "empty-vault")
	if err != nil {
		t.Fatalf("GetContradictions on empty vault returned error: %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("expected 0 contradiction pairs on fresh vault, got %d", len(pairs))
	}
}

// TestEngineExplain_UnknownID verifies that Explain with an unknown engram ID
// returns a valid ExplainData (WouldReturn=false) rather than panicking or
// returning an error. The method is defined to return descriptive data, not an
// error, when an engram simply doesn't appear in activation results.
func TestEngineExplain_UnknownID(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Use an ID that was never written to the vault.
	unknownID := "01HNKZ5F0000000000000000"

	data, err := eng.Explain(ctx, "test-vault", unknownID, []string{"anything"}, nil)
	if err != nil {
		t.Fatalf("Explain with unknown ID returned unexpected error: %v", err)
	}
	if data == nil {
		t.Fatal("Explain returned nil ExplainData, expected non-nil struct")
	}
	if data.EngramID != unknownID {
		t.Errorf("ExplainData.EngramID = %q, want %q", data.EngramID, unknownID)
	}
	if data.WouldReturn {
		t.Errorf("WouldReturn = true for an engram that was never written; expected false")
	}
}

// TestQueryMethodsCompilable is a compile-time proof that all four query
// methods are callable on *Engine via the query.go file. If any method
// were missing or had the wrong signature, this file would fail to compile.
func TestQueryMethodsCompilable(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// GetContradictions
	_, err := eng.GetContradictions(ctx, "vault")
	_ = err

	// GetAssociations
	_, err = eng.GetAssociations(ctx, "vault", "01HNKZ5F0000000000000000", 10)
	_ = err

	// Traverse
	_, _, err = eng.Traverse(ctx, "vault", "01HNKZ5F0000000000000000", 2, 10, false)
	_ = err

	// Explain
	_, err = eng.Explain(ctx, "vault", "01HNKZ5F0000000000000000", []string{"query"}, nil)
	_ = err
}
