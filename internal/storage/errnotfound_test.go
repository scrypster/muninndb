package storage

import (
	"context"
	"errors"
	"testing"
)

// TestErrNotFound_Sentinel pins the contract that "not found" storage errors
// wrap the ErrNotFound sentinel, so callers can use errors.Is instead of
// fragile strings.Contains(err.Error(), "not found") matching.
func TestErrNotFound_Sentinel(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	var ws [8]byte

	missing := NewULID()

	if _, err := store.GetEngram(ctx, ws, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetEngram(missing): err = %v, want errors.Is(..., ErrNotFound)", err)
	}
	if err := store.UpdateTrust(ctx, ws, missing, TrustVerified); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateTrust(missing): err = %v, want errors.Is(..., ErrNotFound)", err)
	}
}
