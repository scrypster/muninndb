package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRelinkEntityEngramLink_CaseVariant_PreservesLink is the regression test
// for #503. Entity names are hashed case-insensitively (EntityNameHash
// lowercases/NFKC-normalizes), so "Foo" and "foo" share the same storage key.
// RelinkEntityEngramLink does Set(toHash) then Delete(fromHash) in one batch —
// when fromHash == toHash the Delete is sequenced after the Set and wins,
// silently destroying the engram's link to BOTH casings. The relink must be a
// no-op when the two names normalize to the same entity.
func TestRelinkEntityEngramLink_CaseVariant_PreservesLink(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("test")
	engID := NewULID()

	require.NoError(t, ps.WriteEntityEngramLink(ctx, ws, engID, "Foo"))

	// Relink across case variants — these normalize to the same entity key.
	require.NoError(t, ps.RelinkEntityEngramLink(ctx, ws, engID, "Foo", "foo"))

	var found []ULID
	require.NoError(t, ps.ScanEntityEngrams(ctx, "Foo", func(_ [8]byte, id ULID) error {
		found = append(found, id)
		return nil
	}))
	require.Len(t, found, 1, "engram link must survive a case-variant relink (#503 data loss)")
	assert.Equal(t, engID, found[0])
}
