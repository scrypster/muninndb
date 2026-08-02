package storage

import (
	"context"
	"testing"
)

// seedEndpoints writes a minimal engram for each id so association fixtures
// satisfy STO-12 (an edge may only exist while both endpoints have a live 0x01
// record). Before #803 these fixtures minted edges between ULIDs that had no
// engram at all — i.e. they encoded as normal exactly the state the invariant
// now forbids, which is part of why nothing in the suite caught the leak.
func seedEndpoints(t testing.TB, ps *PebbleStore, ws [8]byte, ids ...ULID) {
	t.Helper()
	ctx := context.Background()
	for _, id := range ids {
		if _, err := ps.WriteEngram(ctx, ws, &Engram{
			ID:      id,
			Concept: "fixture endpoint",
			Content: "fixture endpoint content",
		}); err != nil {
			t.Fatalf("seedEndpoints %s: %v", id.String(), err)
		}
	}
}
