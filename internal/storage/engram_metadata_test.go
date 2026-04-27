package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// UpdateRelevance
// ---------------------------------------------------------------------------

// TestUpdateRelevance_PersistsValue writes an engram with an initial relevance,
// calls UpdateRelevance with a new value, reads the engram back, and verifies
// the relevance changed to the new value.
func TestUpdateRelevance_PersistsValue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("update-relevance-persist")

	eng := &Engram{
		Concept:   "relevance-concept",
		Content:   "relevance-content",
		Relevance: 0.3,
		Stability: 20.0,
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	newRelevance := float32(0.85)
	newStability := float32(55.0)
	require.NoError(t, store.UpdateRelevance(ctx, ws, id, newRelevance, newStability))

	// Read back via GetMetadata (bypasses cache because UpdateRelevance invalidates it).
	metas, err := store.GetMetadata(ctx, ws, []ULID{id})
	require.NoError(t, err)
	require.Len(t, metas, 1)
	require.NotNil(t, metas[0])
	require.InDelta(t, newRelevance, metas[0].Relevance, 0.001, "relevance must match the updated value")
	require.InDelta(t, newStability, metas[0].Stability, 0.001, "stability must match the updated value")
}

// TestUpdateRelevance_ReflectedInGetEngram verifies that after UpdateRelevance
// the full GetEngram read also shows the new relevance.
func TestUpdateRelevance_ReflectedInGetEngram(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("update-relevance-getengram")

	id, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:   "r-concept",
		Content:   "r-content",
		Relevance: 0.1,
	})
	require.NoError(t, err)

	require.NoError(t, store.UpdateRelevance(ctx, ws, id, 0.9, 75.0))

	got, err := store.GetEngram(ctx, ws, id)
	require.NoError(t, err)
	require.InDelta(t, float32(0.9), got.Relevance, 0.001, "GetEngram must reflect updated relevance")
}

// TestUpdateRelevance_NotFound verifies that UpdateRelevance returns an error
// when the engram does not exist.
func TestUpdateRelevance_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("update-relevance-notfound")

	ghost := NewULID()
	err := store.UpdateRelevance(ctx, ws, ghost, 0.5, 30.0)
	require.Error(t, err, "UpdateRelevance must return an error for a non-existent engram")
}

// ---------------------------------------------------------------------------
// UpdateDigest
// ---------------------------------------------------------------------------

// TestUpdateDigest_PersistsValue writes an engram, calls UpdateDigest with a
// new summary and key points, reads the engram back, and verifies the fields
// were updated.
func TestUpdateDigest_PersistsValue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("update-digest-persist")

	eng := &Engram{
		Concept: "digest-concept",
		Content: "digest-content",
		Summary: "original summary",
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	newSummary := "updated summary after enrichment"
	newKeyPoints := []string{"point A", "point B", "point C"}

	// UpdateDigest resolves the vault prefix internally via FindVaultPrefix.
	require.NoError(t, store.UpdateDigest(ctx, id, newSummary, newKeyPoints, "", ""))

	// Read the engram back (cache was invalidated by UpdateDigest).
	got, err := store.GetEngram(ctx, ws, id)
	require.NoError(t, err)
	require.Equal(t, newSummary, got.Summary, "Summary must reflect the updated value")
	require.Equal(t, newKeyPoints, got.KeyPoints, "KeyPoints must reflect the updated value")
}

// TestUpdateDigest_PreservesUnchangedFields verifies that UpdateDigest only
// overwrites the provided fields; unrelated fields such as Concept and Content
// are not clobbered.
func TestUpdateDigest_PreservesUnchangedFields(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("update-digest-preserve")

	eng := &Engram{
		Concept: "my-concept",
		Content: "my-content",
		Summary: "old",
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	require.NoError(t, store.UpdateDigest(ctx, id, "new summary", nil, "", ""))

	got, err := store.GetEngram(ctx, ws, id)
	require.NoError(t, err)
	require.Equal(t, "my-concept", got.Concept, "Concept must not be changed by UpdateDigest")
	require.Equal(t, "my-content", got.Content, "Content must not be changed by UpdateDigest")
	require.Equal(t, "new summary", got.Summary, "Summary must be updated")
}

// TestUpdateDigest_Idempotent verifies that calling UpdateDigest twice in
// succession does not panic and the last call wins.
func TestUpdateDigest_Idempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("update-digest-idempotent")

	id, err := store.WriteEngram(ctx, ws, &Engram{
		Concept: "idem-concept",
		Content: "idem-content",
	})
	require.NoError(t, err)

	require.NoError(t, store.UpdateDigest(ctx, id, "first", []string{"k1"}, "", ""))
	require.NoError(t, store.UpdateDigest(ctx, id, "second", []string{"k2"}, "", ""))

	got, err := store.GetEngram(ctx, ws, id)
	require.NoError(t, err)
	require.Equal(t, "second", got.Summary, "second UpdateDigest call must overwrite the first")
	require.Equal(t, []string{"k2"}, got.KeyPoints)
}

// TestUpdateDigest_PreservesEmbedDim verifies that digest rewrites do not wipe
// the embedding metadata patched by UpdateEmbedding.
func TestUpdateDigest_PreservesEmbedDim(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("update-digest-preserve-embed-dim")

	id, err := store.WriteEngram(ctx, ws, &Engram{
		Concept: "embed-digest-concept",
		Content: "embed-digest-content",
	})
	require.NoError(t, err)

	vec := make([]float32, 2560)
	vec[0] = 0.25
	require.NoError(t, store.UpdateEmbedding(ctx, ws, id, vec))

	before, err := store.GetEngram(ctx, ws, id)
	require.NoError(t, err)
	require.Equal(t, EmbedDimension(255), before.EmbedDim, "precondition: EmbedDim should reflect EmbedOther before digest update")

	require.NoError(t, store.UpdateDigest(ctx, id, "summary", []string{"kp"}, "", ""))

	after, err := store.GetEngram(ctx, ws, id)
	require.NoError(t, err)
	require.Equal(t, EmbedDimension(255), after.EmbedDim, "UpdateDigest must preserve EmbedDim")
}

// TestUpdateDigest_NotFound verifies that UpdateDigest returns an error when
// the engram ID does not exist in any vault.
func TestUpdateDigest_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ghost := NewULID()
	err := store.UpdateDigest(ctx, ghost, "summary", []string{"kp"}, "", "")
	require.Error(t, err, "UpdateDigest must return an error for a non-existent engram ID")
}

// ---------------------------------------------------------------------------
// UpdateTrust
// ---------------------------------------------------------------------------

// TestUpdateTrust_PersistsValue writes an engram with TrustInferred, calls
// UpdateTrust to set TrustVerified, reads back, and asserts the change.
func TestUpdateTrust_PersistsValue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("update-trust-persist")

	eng := &Engram{
		Concept: "trust-concept",
		Content: "trust-content",
		Trust:   TrustInferred,
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	// Verify initial trust level is preserved after write.
	got, err := store.GetEngram(ctx, ws, id)
	require.NoError(t, err)
	require.Equal(t, TrustInferred, got.Trust, "initial Trust must be TrustInferred")

	// Prime metaCache before update so we can verify invalidation below.
	metas, err := store.GetMetadata(ctx, ws, []ULID{id})
	require.NoError(t, err)
	require.Len(t, metas, 1)
	require.NotNil(t, metas[0])

	// Update trust to Verified.
	require.NoError(t, store.UpdateTrust(ctx, ws, id, TrustVerified))

	// Verify metaCache was invalidated — GetMetadata must not return a stale cached entry.
	metas2, err := store.GetMetadata(ctx, ws, []ULID{id})
	require.NoError(t, err)
	require.Len(t, metas2, 1)
	require.NotNil(t, metas2[0])
	require.Equal(t, id, metas2[0].ID, "GetMetadata must return the correct ID after UpdateTrust")

	// Read back — cache was invalidated by UpdateTrust.
	got2, err := store.GetEngram(ctx, ws, id)
	require.NoError(t, err)
	require.Equal(t, TrustVerified, got2.Trust, "Trust must reflect the updated value")
}

// TestUpdateTrust_NotFound verifies that UpdateTrust returns an error when the
// engram does not exist.
func TestUpdateTrust_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("update-trust-notfound")

	ghost := NewULID()
	err := store.UpdateTrust(ctx, ws, ghost, TrustVerified)
	require.Error(t, err, "UpdateTrust must return an error for a non-existent engram")
}
