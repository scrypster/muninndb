package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/stretchr/testify/require"
)

// TestEntityTokens verifies the fuzzy-match tokenizer: normalization,
// separator splitting, article-stopword dropping, and the all-stopword
// fallback.
func TestEntityTokens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want []string
	}{
		{"The Knock", []string{"knock"}},
		{"knock", []string{"knock"}},
		{"dream-cycle", []string{"dream", "cycle"}},
		{"dream cycle", []string{"dream", "cycle"}},
		{"Token Arcade", []string{"token", "arcade"}},
		{"a_b/c.d", []string{"b", "c", "d"}}, // "a" dropped as article
		{"The", []string{"the"}},             // all-stopword name falls back to raw tokens
		{"  Spaced  Name  ", []string{"spaced", "name"}},
	}
	for _, c := range cases {
		require.Equal(t, c.want, entityTokens(c.name), "entityTokens(%q)", c.name)
	}
}

// linkEntityEngram writes one engram and links it to entityName, upserting
// the entity record first.
func linkEntityEngram(t *testing.T, eng *Engine, ws [8]byte, concept, entityName string) storage.ULID {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{
		Name:   entityName,
		Type:   "concept",
		Source: "inline",
	}, "inline"))
	id, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    concept,
		Content:    concept + " content",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, id, entityName))
	return id
}

// TestFindByEntity_FuzzyArticleVariant is the issue #571 headline case:
// the entity was recorded as "The Knock"; the caller asks for "knock".
func TestFindByEntity_FuzzyArticleVariant(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "fuzzy-article"
	ws := eng.store.ResolveVaultPrefix(vault)
	id := linkEntityEngram(t, eng, ws, "knock-design", "The Knock")

	res, err := eng.FindByEntity(ctx, vault, "knock", 20)
	require.NoError(t, err)
	require.Len(t, res.Engrams, 1)
	require.Equal(t, id, res.Engrams[0].ID)
	require.Equal(t, "The Knock", res.MatchedEntity, "resolution must be reported")
	require.True(t, res.Fuzzy, "article-variant hit must be marked fuzzy")
}

// TestFindByEntity_FuzzySeparatorVariant covers hyphen/space separator
// variants: entity recorded "dream cycle", queried as "dream-cycle".
func TestFindByEntity_FuzzySeparatorVariant(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "fuzzy-separator"
	ws := eng.store.ResolveVaultPrefix(vault)
	id := linkEntityEngram(t, eng, ws, "cycle-log", "dream cycle")

	res, err := eng.FindByEntity(ctx, vault, "dream-cycle", 20)
	require.NoError(t, err)
	require.Len(t, res.Engrams, 1)
	require.Equal(t, id, res.Engrams[0].ID)
	require.Equal(t, "dream cycle", res.MatchedEntity)
	require.True(t, res.Fuzzy)
}

// TestFindByEntity_FuzzyConcatenationVariant covers concatenation variants
// via the joined-token comparison: entity "Token Arcade", query "TokenArcade".
func TestFindByEntity_FuzzyConcatenationVariant(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "fuzzy-concat"
	ws := eng.store.ResolveVaultPrefix(vault)
	id := linkEntityEngram(t, eng, ws, "arcade-platform", "Token Arcade")

	res, err := eng.FindByEntity(ctx, vault, "TokenArcade", 20)
	require.NoError(t, err)
	require.Len(t, res.Engrams, 1)
	require.Equal(t, id, res.Engrams[0].ID)
	require.Equal(t, "Token Arcade", res.MatchedEntity)
	require.True(t, res.Fuzzy)
}

// TestFindByEntity_FuzzyRanking verifies Jaccard ranking: a full token match
// outranks a partial one, and the runner-up is reported as a candidate.
func TestFindByEntity_FuzzyRanking(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "fuzzy-ranking"
	ws := eng.store.ResolveVaultPrefix(vault)
	full := linkEntityEngram(t, eng, ws, "board-doc", "Arcade Board")
	linkEntityEngram(t, eng, ws, "arcade-doc", "Arcade")

	// "the arcade board" is a token variant (article added) of "Arcade Board",
	// so the exact hash lookup misses and the fuzzy path must rank:
	// {arcade, board} vs "Arcade Board" → Jaccard 1.0; vs "Arcade" → 0.5.
	// (A bare case variant like "arcade board" would hit the exact path —
	// EntityNameHash already normalizes case.)
	res, err := eng.FindByEntity(ctx, vault, "the arcade board", 20)
	require.NoError(t, err)
	require.Equal(t, "Arcade Board", res.MatchedEntity, "full token overlap (Jaccard 1.0) must outrank partial (0.5)")
	require.True(t, res.Fuzzy)
	require.Len(t, res.Engrams, 1)
	require.Equal(t, full, res.Engrams[0].ID)
	require.Contains(t, res.Candidates, "Arcade", "runner-up must be reported as a candidate")
}

// TestFindByEntity_NoPhantomMatch verifies that queries sharing no
// non-stopword token with any vault entity return an empty, non-fuzzy
// result — fuzzy matching must not invent matches.
func TestFindByEntity_NoPhantomMatch(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "fuzzy-phantom"
	ws := eng.store.ResolveVaultPrefix(vault)
	linkEntityEngram(t, eng, ws, "knock-design", "The Knock")

	// Unrelated tokens: no match.
	res, err := eng.FindByEntity(ctx, vault, "zebra migration", 20)
	require.NoError(t, err)
	require.Empty(t, res.Engrams)
	require.Empty(t, res.MatchedEntity)
	require.False(t, res.Fuzzy)

	// Stopword-only query: "the" must not match "The Knock" — the article
	// carries no identity and the entity's non-stopword token is "knock".
	res, err = eng.FindByEntity(ctx, vault, "the", 20)
	require.NoError(t, err)
	require.Empty(t, res.Engrams, "an article must not fuzzy-match every article-bearing entity")
	require.False(t, res.Fuzzy)
}

// TestFindByEntity_NoPartialOverlapMatch verifies that a query sharing only
// one token with an unrelated multi-word entity does not qualify as a fuzzy
// match, even when it is the only candidate in the vault. "Token Arcade" and
// "Arcade Fire" share the token "arcade" but neither entity's token set is a
// subset of the other's — this is coincidental overlap, not a token-level
// variant of the same identity, and must not be served as a match.
func TestFindByEntity_NoPartialOverlapMatch(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "fuzzy-partial-overlap"
	ws := eng.store.ResolveVaultPrefix(vault)
	linkEntityEngram(t, eng, ws, "band-doc", "Arcade Fire")

	res, err := eng.FindByEntity(ctx, vault, "Token Arcade", 20)
	require.NoError(t, err)
	require.Empty(t, res.Engrams, "one shared token on an otherwise unrelated multi-word entity must not match")
	require.Empty(t, res.MatchedEntity)
	require.False(t, res.Fuzzy)
}
