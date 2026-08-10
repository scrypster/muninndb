package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// COG-29 amendment — Engine.ContradictionDebt, the vault-wide unresolved-declared
// readout. All fixtures are invented and sit in a domain this project has no
// client in (trail maintenance / model railways / beekeeping).

// debtPairFixture writes two rival facts about one invented subject and declares
// the contradiction between them, returning both IDs. declaredAt, when non-zero,
// backdates the declaring edge by writing it through the store directly — the
// same shape a declaration made by another process (or before this binary
// started) has on disk.
func debtPairFixture(t *testing.T, eng *Engine, vault, subject, factA, factB string, declaredAt time.Time) (string, string) {
	t.Helper()
	ctx := context.Background()
	// An explicit past ValidFrom, so a forget(not_true_since=now) resolution is
	// unambiguously inside the fact's window without a sleep to separate the
	// write from the invalidation (#722: never synchronize with wall clock).
	validFrom := time.Now().Add(-48 * time.Hour)
	wa, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: subject, Content: factA, ValidFrom: &validFrom})
	if err != nil {
		t.Fatal(err)
	}
	wb, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: subject + " revised", Content: factB, ValidFrom: &validFrom})
	if err != nil {
		t.Fatal(err)
	}
	idA, err := storage.ParseULID(wa.ID)
	if err != nil {
		t.Fatal(err)
	}
	idB, err := storage.ParseULID(wb.ID)
	if err != nil {
		t.Fatal(err)
	}
	ws := eng.Store().ResolveVaultPrefix(vault)
	if err := eng.Store().WriteAssociation(ctx, ws, idB, idA, &storage.Association{
		TargetID: idA, RelType: storage.RelContradicts, Weight: 0.8, Confidence: 1,
		CreatedAt: declaredAt,
	}); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()
	return wa.ID, wb.ID
}

// TestContradictionDebt_CleanVaultIsSilent — A2's engine half. A vault with no
// declared contradiction returns nil, not an empty struct, so the surfaces have
// nothing to attach and the zero-debt response is byte-identical to today's.
func TestContradictionDebt_CleanVaultIsSilent(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "culvert clearing",
		Content: "the north loop culvert is cleared every spring"}); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()

	debt, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		t.Fatalf("ContradictionDebt on a clean vault: %v", err)
	}
	if debt != nil {
		t.Fatalf("clean vault must return nil (absent, not empty), got %+v", debt)
	}
}

// TestContradictionDebt_ResolvedVaultIsSilentWithTheGateOPEN is the other half
// of A2, and the one the gate cannot answer. Once a contradiction has been
// declared the fast-path flag is sticky by design, so the derivation DOES run —
// and it must still return nil, not a &ContradictionDebt{Count:0}, or every
// orientation call on a vault that ever had a conflict carries an empty object
// forever.
func TestContradictionDebt_ResolvedVaultIsSilentWithTheGateOPEN(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	a, _ := debtPairFixture(t, eng, "test", "waterbar spacing",
		"waterbars on the ridge trail sit 8 metres apart",
		"waterbars on the ridge trail sit 12 metres apart", time.Now().Add(-5*time.Hour))
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: "test", ID: a}); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()

	// The gate must be OPEN — otherwise this test proves nothing beyond the
	// clean-vault case above.
	if !eng.vaultMayHaveContradictions(ctx, eng.Store().ResolveVaultPrefix("test")) {
		t.Fatal("precondition: the COG-29 fast-path gate is closed, so the derivation never ran")
	}

	debt, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if debt != nil {
		t.Fatalf("a fully-resolved vault must return nil (absent, not empty), got %+v", debt)
	}
}

// TestContradictionDebt_SurfacesADeclaredPairOnAnUnqueriedTopic is the
// derivation half of A1: the debt is reported without anyone querying either
// member, and the age is the DECLARED age, not the write age.
func TestContradictionDebt_SurfacesADeclaredPairOnAnUnqueriedTopic(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	declaredAt := time.Now().Add(-26 * time.Hour).Truncate(time.Millisecond)
	a, b := debtPairFixture(t, eng, "test", "trestle bridge decking width",
		"the trestle bridge decking is 1.2 metres wide",
		"the trestle bridge decking is 1.6 metres wide", declaredAt)

	debt, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if debt == nil {
		t.Fatal("ContradictionDebt = nil on a vault with one unresolved declared pair")
	}
	if debt.Count != 1 {
		t.Errorf("Count = %d, want 1", debt.Count)
	}
	if len(debt.Pairs) != 1 {
		t.Fatalf("Pairs = %d, want 1", len(debt.Pairs))
	}
	p := debt.Pairs[0]
	if !(p.IDa == a && p.IDb == b) && !(p.IDa == b && p.IDb == a) {
		t.Errorf("pair = (%s,%s), want the fixture pair (%s,%s)", p.IDa, p.IDb, a, b)
	}
	if p.ConceptA == "" || p.ConceptB == "" {
		t.Errorf("pair concepts unresolved: %q / %q", p.ConceptA, p.ConceptB)
	}
	if !p.DeclaredAt.Equal(declaredAt) {
		t.Errorf("DeclaredAt = %v, want the backdated declaration %v", p.DeclaredAt, declaredAt)
	}
	if !debt.Oldest.Equal(declaredAt) {
		t.Errorf("Oldest = %v, want %v", debt.Oldest, declaredAt)
	}
	if debt.Truncated {
		t.Error("Truncated = true on a single pair")
	}
	if !debt.ScanComplete {
		t.Error("ScanComplete = false on a tiny vault")
	}
}

// TestContradictionDebt_DetectedOnlyPairsAreExcluded pins D2. A 0x0A marker with
// no declaring 0x03 edge is a DETECTED pair — and after COG-23 R2, an
// un-migrated fabricated marker is mechanically indistinguishable from a genuine
// one. Counting them would greet an upgraded vault with a standing notice about
// conflicts that never existed.
func TestContradictionDebt_DetectedOnlyPairsAreExcluded(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	wa, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "signal block length",
		Content: "the branch line signal block is 4 metres"})
	if err != nil {
		t.Fatal(err)
	}
	wb, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "signal block length revised",
		Content: "the branch line signal block is 6 metres"})
	if err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()
	idA, _ := storage.ParseULID(wa.ID)
	idB, _ := storage.ParseULID(wb.ID)
	ws := eng.Store().ResolveVaultPrefix("test")
	if err := func() error { _, e := eng.Store().FlagContradiction(ctx, ws, idA, idB); return e }(); err != nil {
		t.Fatal(err)
	}

	// Sanity: the marker IS visible to the pull-only report, so this test is
	// asserting an exclusion, not an empty vault.
	rep, err := eng.GetContradictionReport(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pairs) != 1 || rep.Pairs[0].Status != ContradictionDetected {
		t.Fatalf("fixture did not produce one DETECTED pair: %+v", rep.Pairs)
	}

	debt, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if debt != nil {
		t.Fatalf("detected-only pair leaked into the debt readout: %+v", debt)
	}
}

// TestContradictionDebt_FiftyPairsStaysBounded is A3: the true count is never
// capped, the enumeration always is, and the serialized block stays small.
func TestContradictionDebt_FiftyPairsStaysBounded(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Declaration order is the REVERSE of write order, deliberately: engram
	// ULIDs are monotonic, so declaring oldest-first would make the storage
	// iterator's own order identical to oldest-first and the sort untestable.
	base := time.Now().Add(-100 * time.Hour)
	for i := 0; i < 50; i++ {
		debtPairFixture(t, eng, "test", fmt.Sprintf("apiary hive %d queen age", i),
			fmt.Sprintf("hive %d has a first-year queen", i),
			fmt.Sprintf("hive %d has a third-year queen", i),
			base.Add(time.Duration(49-i)*time.Minute))
	}

	debt, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if debt == nil {
		t.Fatal("ContradictionDebt = nil on a 50-pair vault")
	}
	if debt.Count != 50 {
		t.Errorf("Count = %d, want the TRUE total 50 (never capped)", debt.Count)
	}
	if len(debt.Pairs) != debtPairsShown {
		t.Errorf("len(Pairs) = %d, want %d", len(debt.Pairs), debtPairsShown)
	}
	if !debt.Truncated {
		t.Error("Truncated = false while Count > len(Pairs)")
	}
	// Oldest first, and the three SHOWN are the three oldest of the fifty: the
	// fixture declared pair i at base+i minutes, so the shown ages must be
	// base, base+1m, base+2m in that order, and Oldest must be the first.
	for i, p := range debt.Pairs {
		want := base.Add(time.Duration(i) * time.Minute)
		if !p.DeclaredAt.Equal(want) {
			t.Errorf("shown pair %d declared %v, want the %d-th oldest %v", i, p.DeclaredAt, i, want)
		}
	}
	if !debt.Oldest.Equal(debt.Pairs[0].DeclaredAt) {
		t.Errorf("Oldest %v is not the first listed pair's declaration %v", debt.Oldest, debt.Pairs[0].DeclaredAt)
	}
	raw, err := json.Marshal(debt)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 1500 {
		t.Errorf("serialized debt = %d bytes at 50 pairs, want < 1500", len(raw))
	}
}

// TestContradictionDebt_OrderingIsDeterministic is A5's first half. The COG-29
// lesson: map-range order made one query's partner choice flip 33/7 over 40
// calls. Forty identical reads must produce byte-identical output.
func TestContradictionDebt_OrderingIsDeterministic(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Deliberately all-identical declaration times, so ordering rests entirely
	// on the ULID tiebreak rather than on the timestamps.
	same := time.Now().Add(-3 * time.Hour).Truncate(time.Millisecond)
	for i := 0; i < 6; i++ {
		debtPairFixture(t, eng, "test", fmt.Sprintf("trail marker %d paint colour", i),
			fmt.Sprintf("marker %d is painted blue", i),
			fmt.Sprintf("marker %d is painted yellow", i), same)
	}

	var first string
	for i := 0; i < 40; i++ {
		debt, err := eng.ContradictionDebt(ctx, "test")
		if err != nil {
			t.Fatal(err)
		}
		// Vacuity guard: nil is byte-identical to nil forty times over. This
		// test only means something if there is a block to be unstable.
		if debt == nil || debt.Count != 6 || len(debt.Pairs) != debtPairsShown {
			t.Fatalf("call %d: want 6 pairs with %d shown, got %+v", i, debtPairsShown, debt)
		}
		raw, err := json.Marshal(debt)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = string(raw)
			continue
		}
		if string(raw) != first {
			t.Fatalf("call %d differs from call 0:\n  0: %s\n  %d: %s", i, first, i, raw)
		}
	}
}

// TestContradictionDebt_ZeroDeclaredAtIsUnknownNotEpoch is A5's second half. A
// legacy edge with no timestamp must sort FIRST (unknown age is the oldest thing
// in the vault by construction, and over-warning beats under-warning) and must
// keep a ZERO DeclaredAt so the wire renders it absent rather than as 1970.
func TestContradictionDebt_ZeroDeclaredAtIsUnknownNotEpoch(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	debtPairFixture(t, eng, "test", "switch frog number",
		"the yard switch is a number 6 frog",
		"the yard switch is a number 8 frog", time.Now().Add(-2*time.Hour))
	debtPairFixture(t, eng, "test", "coupler height",
		"the coupler height standard is 26 millimetres",
		"the coupler height standard is 24 millimetres", time.Time{})

	debt, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if debt == nil || debt.Count != 2 {
		t.Fatalf("want two pairs, got %+v", debt)
	}
	if !debt.Pairs[0].DeclaredAt.IsZero() {
		t.Errorf("the unknown-age pair must sort FIRST; got %v first", debt.Pairs[0].DeclaredAt)
	}
	if !debt.Oldest.IsZero() {
		t.Errorf("Oldest = %v, want the zero time (unknown), never an invented instant", debt.Oldest)
	}
	if debt.Pairs[1].DeclaredAt.IsZero() {
		t.Error("the dated pair lost its timestamp")
	}
}

// TestContradictionDebt_ResolvedPairsDisappear is A4, and it is the pin that
// keeps the debt readout and COG-29 on ONE definition of "unresolved". Each of
// the three verbs the action string names must drop the count to zero.
func TestContradictionDebt_ResolvedPairsDisappear(t *testing.T) {
	cases := []struct {
		name    string
		resolve func(t *testing.T, eng *Engine, vault, a, b string)
	}{
		{
			name: "evolve the losing side",
			resolve: func(t *testing.T, eng *Engine, vault, a, b string) {
				if _, err := eng.Evolve(context.Background(), vault, a,
					"the trestle bridge decking is 1.6 metres wide", "corrected", nil, ""); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "forget(not_true_since)",
			resolve: func(t *testing.T, eng *Engine, vault, a, b string) {
				when := time.Now()
				if _, err := eng.Forget(context.Background(), &mbp.ForgetRequest{
					Vault: vault, ID: a, NotTrueSince: &when}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "link(supersedes) declares a winner",
			resolve: func(t *testing.T, eng *Engine, vault, a, b string) {
				// A DISTINCT weight from the contradicts edge: the forward
				// association key carries the weight, so an equal-weight
				// supersedes link would land on the same key and REPLACE the
				// declaration instead of resolving it.
				if _, err := eng.Link(context.Background(), &mbp.LinkRequest{
					Vault: vault, SourceID: b, TargetID: a, Weight: 0.9,
					RelType: uint16(storage.RelSupersedes)}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, cleanup := testEnv(t)
			defer cleanup()
			ctx := context.Background()

			a, b := debtPairFixture(t, eng, "test", "trestle bridge decking width",
				"the trestle bridge decking is 1.2 metres wide",
				"the trestle bridge decking is 1.6 metres wide",
				time.Now().Add(-30*time.Hour))

			pre, err := eng.ContradictionDebt(ctx, "test")
			if err != nil {
				t.Fatal(err)
			}
			if pre == nil || pre.Count != 1 {
				t.Fatalf("precondition: want one unresolved pair, got %+v", pre)
			}

			tc.resolve(t, eng, "test", a, b)
			eng.waitWriteTimeIdle()

			post, err := eng.ContradictionDebt(ctx, "test")
			if err != nil {
				t.Fatal(err)
			}
			if post != nil {
				t.Fatalf("the pair was resolved and the debt readout still owes %d: %+v", post.Count, post)
			}
		})
	}
}

// BenchmarkContradictionDebt_CleanVault measures the gate-closed steady state:
// one sync.Map load plus one bounded 0x0A iterator seek, which must stay inside
// the noise of the existing COG-29 closed gate.
func BenchmarkContradictionDebt_CleanVault(b *testing.B) {
	eng, cleanup := testEnv(b)
	defer cleanup()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test",
			Concept: fmt.Sprintf("trail segment %d surface", i),
			Content: fmt.Sprintf("segment %d is crushed limestone", i)}); err != nil {
			b.Fatal(err)
		}
	}
	eng.waitWriteTimeIdle()
	// Warm the once-per-process declared-edge probe so the benchmark measures
	// the steady state, not the first call.
	if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkContradictionDebt_WithDebt is the §11 MERGE GATE. The design's R5
// line is ~50ms: above it, the deferred cache becomes a blocker rather than a
// deferral, because this derivation is attached to an orientation call.
func BenchmarkContradictionDebt_WithDebt(b *testing.B) {
	eng, cleanup := testEnv(b)
	defer cleanup()
	ctx := context.Background()

	// 20 declared, unresolved pairs inside a vault carrying ~2,000 ordinary
	// associations — the declared-edge scan is O(edges), so the association
	// count is what the cost actually rides on.
	var ids []storage.ULID
	for i := 0; i < 400; i++ {
		w, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test",
			Concept: fmt.Sprintf("apiary hive %d note", i),
			Content: fmt.Sprintf("hive %d was inspected in week %d", i, i%52)})
		if err != nil {
			b.Fatal(err)
		}
		id, err := storage.ParseULID(w.ID)
		if err != nil {
			b.Fatal(err)
		}
		ids = append(ids, id)
	}
	eng.waitWriteTimeIdle()
	ws := eng.Store().ResolveVaultPrefix("test")
	for i := 0; i+1 < len(ids); i++ {
		for k := 1; k <= 5 && i+k < len(ids); k++ {
			if err := eng.Store().WriteAssociation(ctx, ws, ids[i], ids[i+k], &storage.Association{
				TargetID: ids[i+k], RelType: storage.RelRelatesTo,
				Weight: float32(k) / 10, Confidence: 1, CreatedAt: time.Now(),
			}); err != nil {
				b.Fatal(err)
			}
		}
	}
	for i := 0; i < 20; i++ {
		if err := eng.Store().WriteAssociation(ctx, ws, ids[i], ids[len(ids)-1-i], &storage.Association{
			TargetID: ids[len(ids)-1-i], RelType: storage.RelContradicts,
			Weight: 0.8, Confidence: 1, CreatedAt: time.Now().Add(-time.Duration(i) * time.Hour),
		}); err != nil {
			b.Fatal(err)
		}
	}

	debt, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		b.Fatal(err)
	}
	if debt == nil || debt.Count != 20 {
		b.Fatalf("fixture did not produce 20 unresolved declared pairs: %+v", debt)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
			b.Fatal(err)
		}
	}
}
