package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
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
	// Weight deliberately distinct from the auto-detected contradiction marker's
	// weight. The contradiction worker emits its own (different-RelType) detected
	// edge between these near-identical facts; if both edges land at the same
	// (src, weight, target) the assoc_reltype_guard rejects the second write and
	// the fixture flakes. The readout counts Declared edges regardless of weight.
	if err := eng.Store().WriteAssociation(ctx, ws, idB, idA, &storage.Association{
		TargetID: idA, RelType: storage.RelContradicts, Weight: 0.5, Confidence: 1,
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

// BenchmarkContradictionDebt_WithDebt is the §11 MERGE GATE, measured in the
// STEADY STATE an orientation call actually pays: the declared-edge scan cached,
// everything downstream of it (the 0x0A read, the endpoint fill, and the whole
// resolution pass) re-derived on every call as it must be.
// benchDebtVault builds the shared benchmark fixture: 20 declared, unresolved
// pairs inside a vault carrying ~2,000 ordinary associations. The association
// count is what the declared-edge scan's cost actually rides on — it is O(edges)
// with no prefix that isolates contradicts edges.
func benchDebtVault(b *testing.B) (*Engine, func()) {
	b.Helper()
	eng, cleanup := testEnv(b)
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

	if d, err := eng.ContradictionDebt(context.Background(), "test"); err != nil || d == nil || d.Count != 20 {
		b.Fatalf("fixture did not produce 20 unresolved declared pairs: %+v (err %v)", d, err)
	}
	return eng, cleanup
}

func BenchmarkContradictionDebt_WithDebt(b *testing.B) {
	eng, cleanup := benchDebtVault(b)
	defer cleanup()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
			b.Fatal(err)
		}
	}
}

// TestContradictionDebt_DoesNotStampAccessRecencyOnItsEndpoints is COG-11 for
// this readout, and it was a live defect: the derivation's endpoint read ran on
// the raw handler ctx, so every orientation call stamped the L1 cache's recency
// clock on BOTH members of every declared pair — engrams the call never
// returned, on a query about something else. EngramLastAccessNs feeds real
// recency SCORING in a LATER, unrelated recall, so the readout was quietly
// making the very memories it demotes look freshly used.
//
// The control arm is the point: a default-mode recall on an unrelated topic
// leaves both endpoints at 0, which is what proves the stamp came from the debt
// derivation and not from the fixture.
func TestContradictionDebt_DoesNotStampAccessRecencyOnItsEndpoints(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	a, b := debtPairFixture(t, eng, "test", "trestle bridge decking width",
		"the trestle bridge decking is 1.2 metres wide",
		"the trestle bridge decking is 1.6 metres wide", time.Now().Add(-26*time.Hour))
	idA, _ := storage.ParseULID(a)
	idB, _ := storage.ParseULID(b)
	ws := eng.Store().ResolveVaultPrefix("test")

	// CONTROL: an unrelated read_only recall must not touch these two engrams.
	if _, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: "test",
		Context: []string{"apiary hive inspection"}, MaxResults: 5, Threshold: 0.001, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if ns := eng.Store().EngramLastAccessNs(ws, idA); ns != 0 {
		t.Fatalf("control: an unrelated read_only recall already stamped endpoint A (%d) — the fixture cannot isolate the debt path", ns)
	}
	if ns := eng.Store().EngramLastAccessNs(ws, idB); ns != 0 {
		t.Fatalf("control: an unrelated read_only recall already stamped endpoint B (%d)", ns)
	}

	debt, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if debt == nil || debt.Count != 1 {
		t.Fatalf("precondition: want one unresolved pair, got %+v", debt)
	}

	if ns := eng.Store().EngramLastAccessNs(ws, idA); ns != 0 {
		t.Errorf("COG-11: the debt readout stamped access recency on endpoint A (EngramLastAccessNs=%d, want 0) — it never returned that memory", ns)
	}
	if ns := eng.Store().EngramLastAccessNs(ws, idB); ns != 0 {
		t.Errorf("COG-11: the debt readout stamped access recency on endpoint B (EngramLastAccessNs=%d, want 0)", ns)
	}
}

// TestContradictionDebt_CachedScanIsInvalidatedByANewDeclaration — F2(a). A
// vault whose scan is cached as clean must still report a contradiction declared
// afterwards. Under-warning is the failure this invalidation exists to prevent.
func TestContradictionDebt_CachedScanIsInvalidatedByANewDeclaration(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Seed one declared pair so the fast-path gate is open and the scan gets
	// cached, then declare a SECOND pair and demand both.
	debtPairFixture(t, eng, "test", "waterbar spacing",
		"waterbars on the ridge trail sit 8 metres apart",
		"waterbars on the ridge trail sit 12 metres apart", time.Now().Add(-30*time.Hour))
	if d, err := eng.ContradictionDebt(ctx, "test"); err != nil || d == nil || d.Count != 1 {
		t.Fatalf("precondition: want one pair cached, got %+v (err %v)", d, err)
	}

	debtPairFixture(t, eng, "test", "culvert diameter",
		"the north loop culvert is 300 millimetres",
		"the north loop culvert is 450 millimetres", time.Now().Add(-2*time.Hour))

	got, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Count != 2 {
		t.Fatalf("a contradiction declared after the scan was cached is invisible: got %+v, want Count 2", got)
	}
}

// TestContradictionDebt_CachedScanStillSeesAResolution — F2(b). Resolution is
// NEVER cached: it depends on engram state and on the clock, and a stale
// resolution is the "resolved it and the theater continued" bug #764 closed.
// The scan cache must not be able to keep a resolved pair alive.
func TestContradictionDebt_CachedScanStillSeesAResolution(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	a, _ := debtPairFixture(t, eng, "test", "switch frog number",
		"the yard switch is a number 6 frog",
		"the yard switch is a number 8 frog", time.Now().Add(-40*time.Hour))
	if d, err := eng.ContradictionDebt(ctx, "test"); err != nil || d == nil || d.Count != 1 {
		t.Fatalf("precondition: want one pair cached, got %+v (err %v)", d, err)
	}
	runsAfterFirst := eng.DeclaredScanRunsForTest()

	// Resolve WITHOUT writing any contradicts edge, so the scan cache stays
	// valid and only the fresh resolution pass can drop the pair.
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: "test", ID: a}); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()

	got, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("the pair was resolved and the cached scan kept it alive: %+v", got)
	}
	if runs := eng.DeclaredScanRunsForTest(); runs != runsAfterFirst {
		t.Errorf("the scan re-ran (%d -> %d) — this test is meant to prove resolution is seen WITHOUT re-scanning", runsAfterFirst, runs)
	}
}

// TestContradictionDebt_GateAndCacheAreBothPinned is F3. Both properties the
// cost story rests on are invisible to behaviour — delete either and the whole
// suite stays green, because they change only I/O. The derivation counter is the
// seam that makes them assertable.
func TestContradictionDebt_GateAndCacheAreBothPinned(t *testing.T) {
	t.Run("clean vault performs ZERO scans (the fast-path gate)", func(t *testing.T) {
		eng, cleanup := testEnv(t)
		defer cleanup()
		ctx := context.Background()
		for i := 0; i < 5; i++ {
			if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test",
				Concept: fmt.Sprintf("trail segment %d surface", i),
				Content: fmt.Sprintf("segment %d is crushed limestone", i)}); err != nil {
				t.Fatal(err)
			}
		}
		eng.waitWriteTimeIdle()

		// Measured from ZERO, deliberately. The gate's own once-per-process
		// probe scans through the store directly and is not counted here, so a
		// clean vault must drive the DERIVATION's scan counter exactly 0 times
		// — never once. Warming up first would let the scan cache stand in for
		// the gate and the assertion would survive the gate's deletion.
		base := eng.DeclaredScanRunsForTest()
		if base != 0 {
			t.Fatalf("precondition: counter starts at %d, want 0", base)
		}
		for i := 0; i < 11; i++ {
			d, err := eng.ContradictionDebt(ctx, "test")
			if err != nil {
				t.Fatal(err)
			}
			if d != nil {
				t.Fatalf("clean vault returned debt: %+v", d)
			}
		}
		if runs := eng.DeclaredScanRunsForTest(); runs != base {
			t.Errorf("a clean vault ran the declared-edge scan %d time(s) over 10 orientation calls — the fast-path gate is not short-circuiting", runs-base)
		}
	})

	t.Run("debt-carrying vault scans ONCE across repeated calls (the cache)", func(t *testing.T) {
		eng, cleanup := testEnv(t)
		defer cleanup()
		ctx := context.Background()
		debtPairFixture(t, eng, "test", "coupler height",
			"the coupler height standard is 26 millimetres",
			"the coupler height standard is 24 millimetres", time.Now().Add(-6*time.Hour))

		base := eng.DeclaredScanRunsForTest()
		for i := 0; i < 10; i++ {
			d, err := eng.ContradictionDebt(ctx, "test")
			if err != nil {
				t.Fatal(err)
			}
			if d == nil || d.Count != 1 {
				t.Fatalf("call %d: want one pair, got %+v", i, d)
			}
		}
		if runs := eng.DeclaredScanRunsForTest() - base; runs != 1 {
			t.Errorf("10 orientation calls ran the declared-edge scan %d times, want exactly 1 — the scan cache is not holding", runs)
		}
	})

	t.Run("a RESOLVED vault stops paying for the scan", func(t *testing.T) {
		// The measured pathology: the fast-path flag is sticky and resolution
		// never deletes the declaring edge, so before the cache this vault paid
		// the full O(associations) scan on every orientation call forever, to
		// emit nothing.
		eng, cleanup := testEnv(t)
		defer cleanup()
		ctx := context.Background()
		a, _ := debtPairFixture(t, eng, "test", "tie plate gauge",
			"the branch line tie plates are 4 millimetres",
			"the branch line tie plates are 6 millimetres", time.Now().Add(-6*time.Hour))
		if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: "test", ID: a}); err != nil {
			t.Fatal(err)
		}
		eng.waitWriteTimeIdle()
		if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
			t.Fatal(err)
		}
		base := eng.DeclaredScanRunsForTest()
		for i := 0; i < 10; i++ {
			if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
				t.Fatal(err)
			}
		}
		if runs := eng.DeclaredScanRunsForTest() - base; runs != 0 {
			t.Errorf("a fully-resolved vault re-ran the scan %d time(s) over 10 calls — it pays the full scan forever to emit nothing", runs)
		}
	})
}

// TestContradictionDebt_ExcludeTagsDoesNotFilterTheReadout records a NAMED
// RESIDUAL, and it asserts the CURRENT behaviour on purpose — it is not a bug
// report dressed as a test.
//
// #713's per-vault ExcludeTags drops a tagged memory from recall RANKING. It is
// applied inside the activation pipeline (activation/engine.go, from
// resolved.ExcludeTags stamped at engine.go's Activate path), and this readout
// never builds an ActivateRequest, so the exclusion is bypassed by
// construction: a memory the operator excluded from ranking can still have its
// CONCEPT named in the debt block.
//
// That is judged acceptable for this increment and is the same exposure
// muninn_contradictions already has to the same credential (D5): ExcludeTags is
// documented as ranking-only and explicitly NOT a hiding mechanism — the engram
// is not deleted and stays visible to direct-id reads. If ExcludeTags is ever
// re-scoped into a visibility control, this test fails and is the flag that this
// surface must then filter too.
func TestContradictionDebt_ExcludeTagsDoesNotFilterTheReadout(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-debt-excludetags-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := &noopEmbedder{}
	as := auth.NewStore(db)
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name: "test", Public: true,
		Plasticity: &auth.PlasticityConfig{ExcludeTags: []string{"archive-noise"}},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	eng := NewEngine(EngineConfig{Store: store, AuthStore: as, FTSIndex: ftsIdx,
		ActivationEngine: activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder),
		TriggerSystem:    trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder),
		Embedder:         embedder})
	defer func() {
		eng.Stop()
		store.Close()
	}()
	ctx := context.Background()

	if got := eng.ResolveVaultPlasticity("test").ExcludeTags; len(got) != 1 || got[0] != "archive-noise" {
		t.Fatalf("precondition: ExcludeTags did not resolve, got %v", got)
	}

	wa, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "sleeper spacing",
		Content: "the ridge trail sleepers sit 600 millimetres apart", Tags: []string{"archive-noise"}})
	if err != nil {
		t.Fatal(err)
	}
	wb, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "sleeper spacing revised",
		Content: "the ridge trail sleepers sit 900 millimetres apart"})
	if err != nil {
		t.Fatal(err)
	}
	idA, _ := storage.ParseULID(wa.ID)
	idB, _ := storage.ParseULID(wb.ID)
	ws := eng.Store().ResolveVaultPrefix("test")
	if err := eng.Store().WriteAssociation(ctx, ws, idB, idA, &storage.Association{
		TargetID: idA, RelType: storage.RelContradicts, Weight: 0.8, Confidence: 1,
		CreatedAt: time.Now().Add(-8 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()

	// CONTROL: recall genuinely drops the excluded memory, so the fixture is live.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: "test",
		Context: []string{"ridge trail sleeper spacing"}, MaxResults: 10, Threshold: 0.001})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range resp.Activations {
		if item.ID == wa.ID {
			t.Fatalf("control: ExcludeTags did not drop the tagged memory from recall — the residual cannot be demonstrated")
		}
	}

	debt, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if debt == nil || debt.Count != 1 {
		t.Fatalf("want one unresolved pair, got %+v", debt)
	}
	named := debt.Pairs[0].ConceptA + "|" + debt.Pairs[0].ConceptB
	if !strings.Contains(named, "sleeper spacing") {
		t.Fatalf("the readout did not name the pair at all: %q", named)
	}
	// RECORDED, not asserted-as-desirable: the excluded memory IS named here.
	t.Logf("named residual confirmed: ExcludeTags excludes %q from recall ranking, "+
		"and the debt readout still names it (%q). Ranking-only by design; see §8.", wa.ID, named)
}

// BenchmarkContradictionDebt_WithDebtColdScan is the UNCACHED derivation — the
// number the §11 gate was originally about, and what every FIRST orientation
// call after a declaration pays. It evicts the scan cache each iteration, so it
// measures the O(all forward associations) declared-edge scan plus the full
// resolution pass. Reported alongside the steady state because the two answer
// different questions and quoting only the cached one would be flattering.
func BenchmarkContradictionDebt_WithDebtColdScan(b *testing.B) {
	eng, cleanup := benchDebtVault(b)
	defer cleanup()
	ctx := context.Background()
	ws := eng.Store().ResolveVaultPrefix("test")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		eng.declaredScanCache.Delete(ws)
		if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkContradictionDebt_ResolvedVault is the row the §4 cost table was
// missing. A vault whose only conflict has been RESOLVED still has the fast-path
// gate open forever (the flag is sticky and resolution does not delete the
// declaring edge), so before the scan cache it paid the full O(associations)
// scan on every orientation call to emit nothing at all. This measures that
// steady state.
func BenchmarkContradictionDebt_ResolvedVault(b *testing.B) {
	eng, cleanup := benchDebtVault(b)
	defer cleanup()
	ctx := context.Background()

	// Retire one endpoint of every declared pair, so every pair resolves and
	// the readout has nothing to say.
	debt, err := eng.ContradictionDebt(ctx, "test")
	if err != nil || debt == nil {
		b.Fatalf("fixture: %+v (err %v)", debt, err)
	}
	rep, err := eng.GetContradictionReport(ctx, "test")
	if err != nil {
		b.Fatal(err)
	}
	for _, p := range rep.Pairs {
		if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: "test", ID: p.IDa}); err != nil {
			b.Fatal(err)
		}
	}
	eng.waitWriteTimeIdle()
	if d, err := eng.ContradictionDebt(ctx, "test"); err != nil || d != nil {
		b.Fatalf("fixture did not fully resolve: %+v (err %v)", d, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
			b.Fatal(err)
		}
	}
}

// TestContradictionDebt_StagedDeclarationDoesNotPoisonTheCache is the TOCTOU
// arm of the write-generation ordering. The generation must be bumped AFTER the
// batch commit that makes the edge readable: a bump at stage time advertises a
// write Pebble cannot serve yet, so a derivation racing the window re-scans,
// sees nothing, and caches the EMPTY scan under the FRESH generation — a
// process-lifetime under-report of a declared conflict. The sequence below is
// the deterministic form of that race (stage → derive → commit), with the
// uncached GetContradictionReport as the ground-truth oracle.
//
// RED: fails with any noteContradictsWrite call restored to its pre-commit
// position (the shipped defect), because the mid-window derivation then caches
// count=1 under the post-declaration generation and the final read never
// invalidates.
func TestContradictionDebt_StagedDeclarationDoesNotPoisonTheCache(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Warm the cache with one real declared pair.
	debtPairFixture(t, eng, "test", "ballast depth", "the siding needs 200mm of ballast",
		"the siding needs 300mm of ballast", time.Now().Add(-24*time.Hour))
	warm, err := eng.ContradictionDebt(ctx, "test")
	if err != nil || warm == nil || warm.Count != 1 {
		t.Fatalf("warm-up: want cached count 1, got %+v err=%v", warm, err)
	}

	// Stage a SECOND declared pair in a store batch without committing it.
	validFrom := time.Now().Add(-48 * time.Hour)
	wa, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "points heater", Content: "the points heater trips at dawn", ValidFrom: &validFrom})
	if err != nil {
		t.Fatal(err)
	}
	wb, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "points heater revised", Content: "the points heater holds through dawn", ValidFrom: &validFrom})
	if err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()
	idA, _ := storage.ParseULID(wa.ID)
	idB, _ := storage.ParseULID(wb.ID)
	ws := eng.Store().ResolveVaultPrefix("test")

	batch := eng.Store().NewBatch()
	if err := batch.WriteAssociation(ctx, ws, idB, idA, &storage.Association{
		TargetID: idA, RelType: storage.RelContradicts, Weight: 0.8, Confidence: 1,
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Derive INSIDE the stage/commit window. Whatever it answers is allowed to
	// be stale (the edge is not durable yet) — but it must not poison the cache
	// for the post-commit world.
	if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
		t.Fatal(err)
	}

	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()

	// Oracle: the production report path, which never consults the scan cache.
	report, err := eng.GetContradictionReport(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, p := range report.Pairs {
		if p.Status == ContradictionDeclared {
			want++
		}
	}
	if want != 2 {
		t.Fatalf("oracle sanity: want 2 unresolved declared pairs on the ground truth, got %d", want)
	}

	debt, err := eng.ContradictionDebt(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if debt == nil || debt.Count != want {
		got := 0
		if debt != nil {
			got = debt.Count
		}
		t.Fatalf("SCAN CACHE UNDER-REPORTS after a staged-then-committed declaration: debt count = %d, ground truth = %d", got, want)
	}
}

// TestContradictionDebt_FollowerBypassesTheScanCache pins the cluster-follower
// arm. A follower's ContradictsWriteGen never moves for replicated writes
// (replication.Applier commits raw Pebble batches below the store — the #869
// layering), so on a node whose replicaProbe reports follower the derivation
// must not trust the cache at all: every call re-scans.
//
// RED: fails with the replicaProbe bypass removed from
// declaredContradictionsCached, because the second and later derivations are
// then served from the cache and the scan-run counter stays flat.
func TestContradictionDebt_FollowerBypassesTheScanCache(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	debtPairFixture(t, eng, "test", "signal lamp", "the up-line lamp is oil-lit",
		"the up-line lamp was electrified", time.Now().Add(-24*time.Hour))

	// Leader/standalone first: cache holds, exactly one scan across repeats.
	if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	base := eng.DeclaredScanRunsForTest()
	for i := 0; i < 5; i++ {
		if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if got := eng.DeclaredScanRunsForTest() - base; got != 0 {
		t.Fatalf("standalone control: %d scans over 5 warm derivations, want 0 (cache must hold)", got)
	}

	// Now the node is a follower. The cache is warm and valid by generation —
	// and must be ignored anyway.
	eng.SetReplicaProbe(func() bool { return true })
	base = eng.DeclaredScanRunsForTest()
	for i := 0; i < 5; i++ {
		debt, err := eng.ContradictionDebt(ctx, "test")
		if err != nil {
			t.Fatal(err)
		}
		if debt == nil || debt.Count != 1 {
			t.Fatalf("follower derivation %d: want count 1, got %+v", i, debt)
		}
	}
	if got := eng.DeclaredScanRunsForTest() - base; got != 5 {
		t.Fatalf("follower ran the scan %d time(s) over 5 orientation calls, want 5 — a follower must never trust the cache (its invalidation counter never moves for replicated writes)", got)
	}

	// And back: a probe reporting NOT-follower (leader or RoleUnknown) resumes
	// caching, matching LocalAppendFunc's fail-open asymmetry.
	eng.SetReplicaProbe(func() bool { return false })
	if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	base = eng.DeclaredScanRunsForTest()
	for i := 0; i < 3; i++ {
		if _, err := eng.ContradictionDebt(ctx, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if got := eng.DeclaredScanRunsForTest() - base; got != 0 {
		t.Fatalf("promoted node: %d scans over 3 warm derivations, want 0 (cache resumes)", got)
	}
}
