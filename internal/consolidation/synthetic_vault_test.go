package consolidation

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
)

// syntheticClass identifies the labeled class of a synthetic engram.
// Each class maps to a specific success criterion in issue #311.
type syntheticClass int

const (
	classDuplicateA       syntheticClass = iota // cosine >= 0.95 — representative (higher confidence)
	classDuplicateB                             // cosine >= 0.95 — should be archived after dedup
	classNearDuplicateA                         // cosine 0.85–0.95 — review band, must NOT be auto-merged
	classNearDuplicateB                         // cosine 0.85–0.95 — review band, must NOT be auto-merged
	classUniqueA                                // orthogonal — must survive dedup unchanged
	classUniqueB                                // orthogonal — must survive dedup unchanged
	classTemporalOld                            // same entity, older fact — may be superseded
	classTemporalNew                            // same entity, newer fact — should win
	classLowAccessUnique                        // rarely accessed, old — must not be weakened
)

// syntheticVaultEntry is one labeled engram in a synthetic vault.
type syntheticVaultEntry struct {
	ID    storage.ULID
	Class syntheticClass
}

// buildSyntheticVault writes a controlled set of labeled engrams into the store
// and returns the entries with their assigned IDs.
//
// Embedding design (all unit vectors in R^4, cosine similarities are exact):
//
//	dupA  = [1, 0, 0, 0]
//	dupB  = [0.97, 0.2431, 0, 0]               cosine(dupA, dupB) = 0.97   >= 0.95 ✓
//	nearA = [0, 1, 0, 0]
//	nearB = [0, 0.90, 0.4359, 0]               cosine(nearA, nearB) = 0.90  0.85–0.95 ✓
//	uniA  = [0, 0, 1, 0]                        orthogonal to dupA, dupB, nearA, nearB
//	uniB  = [0, 0, 0, 1]                        orthogonal to all above
//	temporal/lowAccess: spread direction, low cross-similarity with all above
//
// See https://github.com/scrypster/muninndb/issues/311 for labeled-class definitions.
func buildSyntheticVault(
	t *testing.T,
	ctx context.Context,
	store *storage.PebbleStore,
	db *pebble.DB,
	wsPrefix [8]byte,
) []syntheticVaultEntry {
	t.Helper()

	// Unit vectors — all cosine similarities verified analytically.
	dupAEmbed   := []float32{1.000000, 0.000000, 0.000000, 0.000000}
	dupBEmbed   := []float32{0.970000, 0.243105, 0.000000, 0.000000}  // cosine(dupA,dupB) = 0.9700
	nearAEmbed  := []float32{0.000000, 1.000000, 0.000000, 0.000000}
	nearBEmbed  := []float32{0.000000, 0.900000, 0.435890, 0.000000}  // cosine(nearA,nearB) = 0.9000
	uniAEmbed   := []float32{0.000000, 0.000000, 1.000000, 0.000000}
	uniBEmbed   := []float32{0.000000, 0.000000, 0.000000, 1.000000}
	temporalEmbed := []float32{0.300000, 0.300000, 0.300000, 0.854400}
	lowAccEmbed := []float32{0.200000, 0.200000, 0.200000, 0.938083}

	// Verify cosine similarities at runtime to catch any float precision issues.
	if got := cosineF32(dupAEmbed, dupBEmbed); got < 0.95 {
		t.Fatalf("synthetic: dupA/dupB cosine = %.4f, want >= 0.95", got)
	}
	if got := cosineF32(nearAEmbed, nearBEmbed); got < 0.85 || got >= 0.95 {
		t.Fatalf("synthetic: nearA/nearB cosine = %.4f, want 0.85–0.95", got)
	}

	now := time.Now()
	old := now.Add(-60 * 24 * time.Hour) // 60 days ago

	type spec struct {
		class   syntheticClass
		engram  *storage.Engram
		embed   []float32
	}

	specs := []spec{
		{
			class: classDuplicateA,
			engram: &storage.Engram{
				Concept:     "duplicate-representative",
				Content:     "The capital of France is Paris.",
				Confidence:  0.9,
				Relevance:   0.8,
				Stability:   30,
				AccessCount: 5,
				LastAccess:  now,
			},
			embed: dupAEmbed,
		},
		{
			class: classDuplicateB,
			engram: &storage.Engram{
				Concept:     "duplicate-member",
				Content:     "Paris is the capital city of France.",
				Confidence:  0.5, // lower — will be archived, not the representative
				Relevance:   0.5,
				Stability:   20,
				AccessCount: 2,
				LastAccess:  now,
			},
			embed: dupBEmbed,
		},
		{
			class: classNearDuplicateA,
			engram: &storage.Engram{
				Concept:     "near-dup-a",
				Content:     "Tony works as a software engineer in Dublin.",
				Confidence:  0.8,
				Relevance:   0.7,
				Stability:   25,
				AccessCount: 3,
				LastAccess:  now,
			},
			embed: nearAEmbed,
		},
		{
			class: classNearDuplicateB,
			engram: &storage.Engram{
				Concept:     "near-dup-b",
				Content:     "Tony is a software developer based in Dublin.",
				Confidence:  0.75,
				Relevance:   0.65,
				Stability:   22,
				AccessCount: 2,
				LastAccess:  now,
			},
			embed: nearBEmbed,
		},
		{
			class: classUniqueA,
			engram: &storage.Engram{
				Concept:     "unique-a",
				Content:     "The API rate limit is 1000 requests per minute.",
				Confidence:  0.95,
				Relevance:   0.9,
				Stability:   40,
				AccessCount: 8,
				LastAccess:  now,
			},
			embed: uniAEmbed,
		},
		{
			class: classUniqueB,
			engram: &storage.Engram{
				Concept:     "unique-b",
				Content:     "Database backups run every Sunday at 02:00 UTC.",
				Confidence:  0.9,
				Relevance:   0.85,
				Stability:   35,
				AccessCount: 6,
				LastAccess:  now,
			},
			embed: uniBEmbed,
		},
		{
			class: classTemporalOld,
			engram: &storage.Engram{
				Concept:     "temporal-old",
				Content:     "Alice lives in Berlin.",
				Confidence:  0.7,
				Relevance:   0.6,
				Stability:   15,
				AccessCount: 1,
				CreatedAt:   old,
				LastAccess:  old,
			},
			embed: temporalEmbed,
		},
		{
			class: classTemporalNew,
			engram: &storage.Engram{
				Concept:     "temporal-new",
				Content:     "Alice moved to Madrid.",
				Confidence:  0.9,
				Relevance:   0.85,
				Stability:   30,
				AccessCount: 4,
				CreatedAt:   now,
				LastAccess:  now,
			},
			embed: temporalEmbed, // same semantic direction — intentional
		},
		{
			class: classLowAccessUnique,
			engram: &storage.Engram{
				Concept:     "low-access-unique",
				Content:     "The signing key rotation period is 90 days.",
				Confidence:  0.95,
				Relevance:   0.9,
				Stability:   50,
				AccessCount: 1,  // rarely accessed
				CreatedAt:   old,
				LastAccess:  old, // last accessed 60 days ago
			},
			embed: lowAccEmbed,
		},
	}

	var entries []syntheticVaultEntry
	for _, s := range specs {
		id := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, s.engram)
		entries = append(entries, syntheticVaultEntry{ID: id, Class: s.class})
	}
	return entries
}

// findByClass returns the IDs of all entries with the given class.
func findByClass(entries []syntheticVaultEntry, class syntheticClass) []storage.ULID {
	var ids []storage.ULID
	for _, e := range entries {
		if e.Class == class {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// cosineF32 computes cosine similarity between two float32 slices.
// Duplicates cosineSimilarity() but avoids a package-level name collision in tests.
func cosineF32(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, magA, magB float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		magA += fa * fa
		magB += fb * fb
	}
	magA = math.Sqrt(magA)
	magB = math.Sqrt(magB)
	if magA == 0 || magB == 0 {
		return 0
	}
	return float32(dot / (magA * magB))
}
