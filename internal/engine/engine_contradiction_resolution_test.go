package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestContradictionResolution_EveryPath is R5 for #764 D3.
//
// Before this, NO operation in the product cleared a declared contradiction.
// evolve, forget (soft), forget(not_true_since), hard-delete and
// muninn_decide all left BOTH the 0x03 edge and the 0x0A marker in place, so
// muninn_contradictions listed the pair forever — a hard-deleted endpoint
// rendering a permanently blank-concept row. The evaluators resolved their
// conflict the way the product tells them to, expected the theater to stop,
// and nothing in the product could have stopped it.
//
// One liveness-and-resolution rule, applied identically in recall (COG-29) and
// in GetContradictionReport, closes all of them at once. Each arm below is
// RED at bb10f30 on the report half.
func TestContradictionResolution_EveryPath(t *testing.T) {
	cases := []struct {
		name string
		// resolve performs the resolution; a and b are the fixture's two sides
		// (b contradicts a).
		resolve func(t *testing.T, eng *Engine, vault, a, b string)
		// wantListed is false only for hard delete, where the 0x03 edges are
		// gone and only a dangling 0x0A marker remains: there is nothing left
		// to name, so the pair is dropped rather than rendered blank.
		wantListed bool
		wantReason string
	}{
		{
			name: "evolve the losing side",
			resolve: func(t *testing.T, eng *Engine, vault, a, b string) {
				if _, err := eng.Evolve(context.Background(), vault, a,
					"the request timeout limit is 320ms", "corrected", nil, ""); err != nil {
					t.Fatal(err)
				}
			},
			wantListed: true,
			wantReason: ContradictionResolvedByRetirement,
		},
		{
			name: "forget (soft)",
			resolve: func(t *testing.T, eng *Engine, vault, a, b string) {
				if _, err := eng.Forget(context.Background(), &mbp.ForgetRequest{Vault: vault, ID: a}); err != nil {
					t.Fatal(err)
				}
			},
			wantListed: true,
			wantReason: ContradictionResolvedByRetirement,
		},
		{
			name: "forget (not_true_since)",
			resolve: func(t *testing.T, eng *Engine, vault, a, b string) {
				// Must be after the fact's own ValidFrom (the engine rejects a
				// window valid at no instant) and at or before now, so the fact
				// is genuinely expired by the time recall reads it.
				time.Sleep(5 * time.Millisecond)
				when := time.Now()
				if _, err := eng.Forget(context.Background(), &mbp.ForgetRequest{
					Vault: vault, ID: a, NotTrueSince: &when}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(5 * time.Millisecond)
			},
			wantListed: true,
			wantReason: ContradictionResolvedByRetirement,
		},
		{
			name: "forget (hard)",
			resolve: func(t *testing.T, eng *Engine, vault, a, b string) {
				if _, err := eng.Forget(context.Background(), &mbp.ForgetRequest{
					Vault: vault, ID: a, Hard: true}); err != nil {
					t.Fatal(err)
				}
			},
			wantListed: false,
		},
		{
			name: "link(supersedes) declares a winner",
			resolve: func(t *testing.T, eng *Engine, vault, a, b string) {
				// A DISTINCT weight is load-bearing: the forward
				// association key is 0x03|ws|src|weightComplement|dst, so a
				// supersedes link written at the same weight as the existing
				// contradicts link between the same pair lands on the same key
				// and REPLACES it (pre-existing storage behaviour, not
				// something this increment introduces). Using a distinct
				// weight keeps both edges, which is the case the resolution
				// rule actually has to handle.
				if _, err := eng.Link(context.Background(), &mbp.LinkRequest{
					Vault: vault, SourceID: b, TargetID: a, Weight: 0.9,
					RelType: uint16(storage.RelSupersedes)}); err != nil {
					t.Fatal(err)
				}
			},
			wantListed: true,
			wantReason: ContradictionResolvedBySupersedes,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, cleanup := testEnv(t)
			defer cleanup()
			ctx := context.Background()
			a, b := contradictionFixture(t, eng, "test")

			// Precondition: the theater is running on both surfaces.
			pre, err := eng.GetContradictionReport(ctx, "test")
			if err != nil {
				t.Fatal(err)
			}
			if pre.PendingCount+pre.DetectedCount != 1 || pre.ResolvedCount != 0 {
				t.Fatalf("precondition: want one live pair, got %+v", pre)
			}
			if recallContradiction(t, eng, "test", nil).Conflict == nil {
				t.Fatal("precondition: recall must report the unresolved conflict")
			}

			tc.resolve(t, eng, "test", a, b)
			eng.waitWriteTimeIdle()

			// The report surface.
			post, err := eng.GetContradictionReport(ctx, "test")
			if err != nil {
				t.Fatal(err)
			}
			if post.PendingCount != 0 || post.DetectedCount != 0 {
				t.Errorf("report still lists a LIVE contradiction after resolution: %+v", post)
			}
			if tc.wantListed {
				if len(post.Pairs) != 1 {
					t.Fatalf("want the resolved pair still listed (history is real), got %+v", post.Pairs)
				}
				if post.Pairs[0].Status != ContradictionResolved {
					t.Errorf("Status = %q, want %q", post.Pairs[0].Status, ContradictionResolved)
				}
				if post.Pairs[0].ResolvedBy != tc.wantReason {
					t.Errorf("ResolvedBy = %q, want %q", post.Pairs[0].ResolvedBy, tc.wantReason)
				}
				if post.ResolvedCount != 1 {
					t.Errorf("ResolvedCount = %d, want 1", post.ResolvedCount)
				}
			} else if len(post.Pairs) != 0 {
				t.Errorf("a hard-deleted endpoint leaves nothing to name; want the dangling pair dropped, got %+v", post.Pairs)
			}

			// The recall surface. include_invalid is used so a retired side is
			// still retrievable — the point is that it is no longer presented
			// as a live conflict, not that it vanished.
			for _, includeInvalid := range []bool{false, true} {
				resp := recallContradiction(t, eng, "test", func(r *mbp.ActivateRequest) {
					r.IncludeInvalid = includeInvalid
				})
				if resp.Conflict != nil {
					t.Errorf("recall (include_invalid=%v) still reports a conflict after resolution: %+v",
						includeInvalid, resp.Conflict)
				}
				for _, it := range resp.Activations {
					if it.UnresolvedContradiction != nil {
						t.Errorf("recall (include_invalid=%v): row %s still annotated unresolved_contradiction",
							includeInvalid, it.ID)
					}
				}
			}
		})
	}
}

// TestContradictionResolution_DanglingMarkerIsNotRenderedBlank pins the
// hard-delete half on its own: DeleteEngram removes both directions of the
// 0x03 edge but leaves the 0x0A marker behind, and the report used to render
// that as a "detected" pair with a permanently blank concept — an unknown
// presented as a known.
func TestContradictionResolution_DanglingMarkerIsNotRenderedBlank(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	a, b := contradictionFixture(t, eng, "test")

	ws := eng.store.ResolveVaultPrefix("test")
	idA, _ := storage.ParseULID(a)
	idB, _ := storage.ParseULID(b)
	// Flag the marker the way the detector would, THEN hard-delete one side,
	// so the dangling marker is the only thing left.
	if _, err := eng.store.FlagContradiction(ctx, ws, idA, idB); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: "test", ID: a, Hard: true}); err != nil {
		t.Fatal(err)
	}

	rep, err := eng.GetContradictionReport(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rep.Pairs {
		if p.ConceptA == "" || p.ConceptB == "" {
			t.Errorf("report renders a pair with an unresolvable endpoint: %+v", p)
		}
	}
	if rep.DetectedCount != 0 || rep.PendingCount != 0 {
		t.Errorf("dangling marker still counted as a live contradiction: %+v", rep)
	}
}
