package activation

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestPhase5Traverse_InertAtTheMeasuredSeedCeiling pins the #801 finding as a
// fact of the shipped code rather than as a comment that can rot.
//
// phase5Traverse gates `seed.rrfScore * weight * boost * 0.7^depth` on
// minHopScore = 0.05. The seed score is a raw RRF sum, so it is bounded by the
// number of candidate lists that can contribute to ONE candidate:
//
//	unfiltered recall  1/41 + 1/61 + 1/121 + 1/51 = 0.0686559  (hnsw, fts, decay, pas)
//	best real seed measured                       = 0.04078    (rank-1 hnsw + rank-1 fts)
//
// The `time` and `tag` lists only populate when the request carries a time or
// tag filter, and across 460 seeds on two production vaults NO seed ever
// reached five or six lists. Edge weight is bounded by 1.0 and the `default`
// profile only dampens (every Boost entry <= 1.0), so at the unfiltered
// ceiling a hop would need weight x boost >= 1.041 — above the maximum. This
// test asserts that with the MAXIMUM representable edge weight and a seed at
// the MEASURED ceiling, the phase still emits nothing.
//
// It cannot pass vacuously. Its RED control is the neighbouring
// TestPhase5Traverse_ReachesSymmetricEdgeFromEitherEndpoint, which drives the
// same function over the same fixture at rrfScore 1.0 and DOES get hops: break
// the BFS and that one fails, make traversal live and this one fails.
//
// If this test starts failing, do not raise the ceiling constants to make it
// pass. #801 measured 0 hops on 150/150 real queries over a 3,458-engram
// production vault with 127,798 edges, and — at a fully-open gate — traversal
// strictly dominated by raising CandidatesPerIndex (0/150 wins at cap 2+,
// p < 1e-45, replicated on a second vault). Read
// docs/internals/decision-record.md (#801) first.
func TestPhase5Traverse_InertAtTheMeasuredSeedCeiling(t *testing.T) {
	// Recomputed here rather than hardcoded, so the arithmetic is visible and a
	// change to the k-constants shows up as a changed ceiling, not a stale one.
	const (
		unfilteredCeiling = 1.0/41 + 1.0/61 + 1.0/121 + 1.0/51 // 0.0686559
		bestRealSeed      = 1.0/41 + 1.0/61                    // 0.04078
	)

	cases := []struct {
		name    string
		seed    float64
		relType storage.RelType
	}{
		{"unfiltered 4-list ceiling, relates_to", unfilteredCeiling, storage.RelRelatesTo},
		{"unfiltered 4-list ceiling, co-activated", unfilteredCeiling, storage.RelCoActivated},
		{"best seed measured on a production vault", bestRealSeed, storage.RelRelatesTo},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			older, newer := olderNewer()
			e, store := newRealStoreEngine(t)
			ctx := context.Background()
			ws := store.VaultPrefix("traverse-inert")

			// #803/STO-12: every association writer refuses an edge unless both
			// endpoints have a live 0x01 record, so the fixture must seed the
			// endpoints before writing the edge below.
			seedEndpoints(t, store, ws, older, newer)

			// Weight 1.0 is the maximum representable association weight. The
			// heaviest edge in a 127,798-edge production census was 0.799, so
			// this fixture is strictly more favourable to traversal than any
			// real vault measured.
			if err := store.WriteAssociation(ctx, ws, older, newer, &storage.Association{
				TargetID: newer, Weight: 1.0, RelType: tc.relType,
			}); err != nil {
				t.Fatalf("WriteAssociation: %v", err)
			}

			profile := GetProfile("default")
			if profile == nil {
				t.Fatal(`GetProfile("default") returned nil`)
			}
			if got := profile.BoostFor(tc.relType); got > 1.0 {
				t.Fatalf("default profile boosts %v to %.3f; this test's premise is that the "+
					"default profile only dampens", tc.relType, got)
			}

			got := e.phase5Traverse(ctx, &ActivateRequest{HopDepth: 2}, ws, profile,
				[]fusedCandidate{{id: older, rrfScore: tc.seed}})

			if len(got) != 0 {
				t.Fatalf("phase5Traverse emitted %d hop(s) at seed %.7f with a weight-1.0 edge; "+
					"#801 measured this phase as inert on every real corpus. If traversal has "+
					"been deliberately made live, update docs/internals/decision-record.md, "+
					"CLAUDE.md and docs/feature-reference.md, which all currently say it is not.",
					len(got), tc.seed)
			}
		})
	}
}
