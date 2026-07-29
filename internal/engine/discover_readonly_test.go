package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// mutationCanaryStore wraps a real *storage.PebbleStore, embedding it so it
// structurally satisfies every method PebbleStore has (including the narrow
// discoverReader subset used by runDiscover) via Go's method promotion, but
// shadows every write method known to be reachable from the entity/engram
// graph with a t.Fatal — the "fail-on-mutate wrapper" pinning COG-22.
//
// runDiscover only ever holds a value typed as the narrow discoverReader
// interface, so it cannot call these shadowed methods even if it wanted to
// (Go static dispatch resolves to the interface's method set, not the
// wrapper's full set) — that compile-time fact IS the structural
// enforcement. This wrapper is the belt-and-suspenders runtime pin: if a
// future change ever widened discoverReader to include a mutating method, or
// swapped runDiscover to take the concrete store type, this test would start
// failing the instant discovery touched it, rather than silently starting to
// write.
type mutationCanaryStore struct {
	*storage.PebbleStore
	t *testing.T
}

func (m *mutationCanaryStore) WriteEntityEngramLink(ctx context.Context, ws [8]byte, engramID storage.ULID, entityName string) error {
	m.t.Fatal("COG-22 VIOLATION: discovery called WriteEntityEngramLink (a write)")
	return nil
}

func (m *mutationCanaryStore) IncrementEntityCoOccurrence(ctx context.Context, ws [8]byte, nameA, nameB string) error {
	m.t.Fatal("COG-22 VIOLATION: discovery called IncrementEntityCoOccurrence (a write)")
	return nil
}

func (m *mutationCanaryStore) UpsertEntityRecord(ctx context.Context, record storage.EntityRecord, source string) error {
	m.t.Fatal("COG-22 VIOLATION: discovery called UpsertEntityRecord (a write)")
	return nil
}

func (m *mutationCanaryStore) UpdateMetadata(ctx context.Context, wsPrefix [8]byte, id storage.ULID, meta *storage.EngramMeta) error {
	m.t.Fatal("COG-22 VIOLATION: discovery called UpdateMetadata (a write)")
	return nil
}

func (m *mutationCanaryStore) DeleteEngram(ctx context.Context, wsPrefix [8]byte, id storage.ULID) error {
	m.t.Fatal("COG-22 VIOLATION: discovery called DeleteEngram (a write)")
	return nil
}

func (m *mutationCanaryStore) TouchAccess(ctx context.Context, wsPrefix [8]byte, id storage.ULID) error {
	m.t.Fatal("COG-22 VIOLATION: discovery called TouchAccess (a write / access-metadata mutation)")
	return nil
}

// TestDiscover_COG22_ReadOnly_FailOnMutate pins COG-22: running the real
// discovery algorithm against a canary store that fails the test on any
// known write method must complete successfully and return real, non-empty
// candidates — proving discovery both works and never touches a write path.
func TestDiscover_COG22_ReadOnly_FailOnMutate(t *testing.T) {
	ctx := context.Background()
	eng, cleanup := testEnv(t)
	defer cleanup()
	buildDiscoverProofVault(t, ctx, eng, false)

	canary := &mutationCanaryStore{PebbleStore: eng.store, t: t}
	var reader discoverReader = canary // compiles only via the narrow interface

	result, err := runDiscover(ctx, reader, mustNormalize(t, discoverProofRequest()))
	if err != nil {
		t.Fatalf("runDiscover against canary store failed: %v", err)
	}
	if len(result.Candidates) == 0 {
		t.Fatalf("expected non-empty candidates against the planted vault (a no-op read-only stub would also pass trivially — this must be the real algorithm)")
	}
	found := false
	for _, c := range result.Candidates {
		if c.EntityA == "hot-day" && c.EntityB == "FLWR-rally" && c.LagDays == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the planted signal among candidates when run through the canary store; got %+v", result.Candidates)
	}
}

func mustNormalize(t *testing.T, req DiscoverRequest) DiscoverRequest {
	t.Helper()
	out, err := NormalizeDiscoverRequest(req)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return out
}
