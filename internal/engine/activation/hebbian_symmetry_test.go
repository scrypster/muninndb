package activation

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// #800 / COG-31 — phase4HebbianBoost must score a symmetric edge identically
// from BOTH endpoints.
//
// The Hebbian worker canonicalises each co-activated pair by byte-wise ULID
// sort (internal/cognitive/hebbian.go canonicalPair), and ULIDs are
// time-ordered, so the edge is ALWAYS written older→newer. A forward-only read
// therefore scored the pair at full strength when the OLDER engram was the
// candidate and at exactly ZERO when the NEWER one was — for the same single
// relationship, silently.
//
// This is an exact algebraic identity, not a statistic, so the RED/GREEN pair
// IS the proof and "sample size" does not apply. Everything that could make it
// a statistic is removed: a real Pebble store, a hand-built activation log with
// a FIXED timestamp (so the 3600 s recency term is a constant, not a
// wall-clock race), and a direct call into phase4HebbianBoost.
// ---------------------------------------------------------------------------

// newRealStoreEngine builds an ActivationEngine over a REAL PebbleStore, with
// no logCh/drainLog goroutine — phase4HebbianBoost only reads assocLog and the
// store, and Record() writes the log synchronously.
func newRealStoreEngine(t *testing.T) (*ActivationEngine, *storage.PebbleStore) {
	t.Helper()
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 128})
	t.Cleanup(func() { _ = store.Close() })
	return &ActivationEngine{store: store, assocLog: &ActivationLog{}}, store
}

// seedEndpoints writes a minimal engram for each id. #803/STO-12: every
// association writer REFUSES an edge unless both endpoints have a live 0x01
// record, so these fixtures — which previously minted edges between bare ULIDs
// — would otherwise fail at the write, long before reaching the symmetry
// behaviour they exist to measure. It is also the more faithful fixture: the
// Hebbian worker only ever co-activates engrams that exist.
func seedEndpoints(t *testing.T, store *storage.PebbleStore, ws [8]byte, ids ...storage.ULID) {
	t.Helper()
	for _, id := range ids {
		if _, err := store.WriteEngram(context.Background(), ws, &storage.Engram{
			ID:      id,
			Concept: "fixture endpoint",
			Content: "fixture endpoint content",
		}); err != nil {
			t.Fatalf("seedEndpoints %s: %v", id.String(), err)
		}
	}
}

// olderNewer returns two ULIDs with a guaranteed byte-wise order, so "older"
// and "newer" mean what canonicalPair means by them without depending on clock
// resolution between two NewULID() calls.
func olderNewer() (older, newer storage.ULID) {
	base := time.Unix(1_700_000_000, 0)
	older = storage.NewULIDWithTime(base)
	newer = storage.NewULIDWithTime(base.Add(24 * time.Hour))
	return
}

// bothArms seeds ONE association edge src→dst at seedWeight, primes BOTH
// endpoints into a SINGLE activation-log entry, and scores BOTH endpoints as
// candidates in a SINGLE phase4HebbianBoost call.
//
// The single call is load-bearing, not tidiness. phase4HebbianBoost reads
// time.Now() ITSELF (engine.go: `now := time.Now().Unix()`) and weights each
// log entry by exp(-(now-At)/3600). Two separate calls straddling a one-second
// boundary therefore apply recency factors that differ by ~2.8e-4 — enough to
// blow an exact-equality assertion, and enough that an earlier draft of this
// test passed only by luck. One call means one `now`, one recency factor, and
// an equality that is exact by construction rather than by timing. No
// time.Sleep, no wall-clock deadline, no injected clock needed.
//
// Priming both endpoints is safe for the measurement: the only edge in the
// store is src→dst, so candidate src can only be boosted by dst's presence in
// the log and candidate dst only by src's, and both draw the same recency.
func bothArms(
	t *testing.T,
	src, dst storage.ULID,
	seedWeight float32,
	relType storage.RelType,
) (fwdArm, revArm float64) {
	t.Helper()
	e, store := newRealStoreEngine(t)
	ctx := context.Background()
	ws := store.VaultPrefix("symmetry")
	const vaultID uint32 = 7

	seedEndpoints(t, store, ws, src, dst)

	// The canonical Hebbian write shape: WriteAssociation seeds the pair, then
	// UpdateAssocWeight (what UpdateAssocWeightBatch does per pair) sets the
	// co-activation weight. Both write 0x03 AND 0x04 — the reverse index has
	// been fully maintained all along, which is why this fix is retroactive
	// and needs no migration.
	if err := store.WriteAssociation(ctx, ws, src, dst, &storage.Association{
		TargetID: dst, Weight: 0.01, RelType: relType,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, seedWeight, 1); err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}

	e.assocLog.Record(LogEntry{
		VaultID: vaultID, At: time.Now(), EngramIDs: []storage.ULID{src, dst},
	})

	cands := []fusedCandidate{{id: src, rrfScore: 0.5}, {id: dst, rrfScore: 0.5}}
	e.phase4HebbianBoost(ctx, ws, vaultID, cands)
	// cands[0] is the edge's SOURCE — reached over the forward 0x03 index.
	// cands[1] is the edge's DESTINATION — reachable only over 0x04.
	return cands[0].hebbianBoost, cands[1].hebbianBoost
}

// TestHebbianBoost_IsSymmetricInPairOrder is design P1: exact equality across
// the two pair orders, with the K4 underpowered-run guard.
func TestHebbianBoost_IsSymmetricInPairOrder(t *testing.T) {
	older, newer := olderNewer()
	// A weight in the same order of magnitude as the reported case. Steady
	// state clamps Hebbian edges to peakWeight x 0.05 (COG-27), so a real
	// co-activation edge sits near 0.0005 — this is deliberately a plausible
	// magnitude and not a round number that could hide an arithmetic slip.
	const seedWeight float32 = 0.0103

	// The edge is written older→newer, exactly as canonicalPair produces it.
	arm1, arm2 := bothArms(t, older, newer, seedWeight, storage.RelCoActivated)

	t.Logf("arm1 (candidate = OLDER endpoint, forward): hebbianBoost = %.9g", arm1)
	t.Logf("arm2 (candidate = NEWER endpoint, reverse): hebbianBoost = %.9g", arm2)

	// K4: both arms at zero proves nothing — it would mean the log never
	// primed or the edge was never read. Assert a live signal BEFORE comparing.
	if arm1 == 0 && arm2 == 0 {
		t.Fatal("UNDERPOWERED, not a pass: both arms scored 0 — the activation log " +
			"did not prime or the seeded edge was not read")
	}
	if arm1 <= 0 {
		t.Fatalf("arm1 must be strictly positive, got %v", arm1)
	}

	if diff := math.Abs(arm1 - arm2); diff >= 1e-9 {
		t.Fatalf("#800: the SAME association scores differently depending on which "+
			"endpoint is the candidate.\n  arm1 (older candidate) = %.9g\n"+
			"  arm2 (newer candidate) = %.9g\n  |diff| = %.9g, want < 1e-9",
			arm1, arm2, diff)
	}
}

// TestHebbianBoost_DirectionalEdgeStaysOneWay is the counterpart to P1 and the
// reason BidirectionalForRanking is not simply "true". A directional relation
// must keep scoring from ONE endpoint only; making supersession symmetric for
// ranking is a step toward "the OLD version supersedes the NEW one".
func TestHebbianBoost_DirectionalEdgeStaysOneWay(t *testing.T) {
	older, newer := olderNewer()
	// evolve writes successor -RelSupersedes-> predecessor.
	fwd, rev := bothArms(t, newer, older, 0.5, storage.RelSupersedes)

	t.Logf("supersedes forward = %.9g, reverse = %.9g", fwd, rev)
	if fwd <= 0 {
		t.Fatalf("the forward supersedes edge must still score, got %v", fwd)
	}
	if rev != 0 {
		t.Fatalf("RelSupersedes leaked backwards into ranking: reverse boost = %v, want 0", rev)
	}
}

// TestHebbianBoost_PairCountedOnce: an edge written in BOTH directions (a
// caller's link plus the neighbour worker's independent write) must contribute
// its weight ONCE, not twice. Without dedup in the merge, phase4HebbianBoost's
// sum double-counts the pair and inflates the boost.
func TestHebbianBoost_PairCountedOnce(t *testing.T) {
	e, store := newRealStoreEngine(t)
	ctx := context.Background()
	ws := store.VaultPrefix("symmetry")
	const vaultID uint32 = 7
	const w float32 = 0.25

	a, b := olderNewer()
	seedEndpoints(t, store, ws, a, b)
	for _, pair := range [][2]storage.ULID{{a, b}, {b, a}} {
		if err := store.WriteAssociation(ctx, ws, pair[0], pair[1], &storage.Association{
			TargetID: pair[1], Weight: 0.01, RelType: storage.RelRelatesTo,
		}); err != nil {
			t.Fatalf("WriteAssociation: %v", err)
		}
		if err := store.UpdateAssocWeight(ctx, ws, pair[0], pair[1], w, 1); err != nil {
			t.Fatalf("UpdateAssocWeight: %v", err)
		}
	}

	logAt := time.Now()
	e.assocLog.Record(LogEntry{VaultID: vaultID, At: logAt, EngramIDs: []storage.ULID{b}})
	cands := []fusedCandidate{{id: a, rrfScore: 0.5}}
	e.phase4HebbianBoost(ctx, ws, vaultID, cands)

	// recency is exp(0/3600) = 1 for a same-instant entry, so the boost is the
	// edge weight exactly — once.
	got := cands[0].hebbianBoost
	t.Logf("boost for a pair written in BOTH directions at %v: %.9g", w, got)
	if math.Abs(got-float64(w)) >= 1e-6 {
		t.Fatalf("pair counted more than once: boost = %.9g, want %.9g (the single edge weight)",
			got, float64(w))
	}
}

// TestPhase5Traverse_ReachesSymmetricEdgeFromEitherEndpoint is the second call
// site. BFS could only ever walk a symmetric edge from the endpoint the writer
// happened to pick as source.
//
// Context, and why this is not dressed up as a user-visible win: #801 measured
// that phase5Traverse emits NOTHING under the default profile, because
// minHopScore (0.05) gates a raw rrfScore whose theoretical maximum is 0.0885.
// This test therefore drives phase5Traverse DIRECTLY with a seed score high
// enough to clear that gate — it proves the graph read is correct, not that
// traversal is live. When #801 repairs the gate, it lands on a correct read.
func TestPhase5Traverse_ReachesSymmetricEdgeFromEitherEndpoint(t *testing.T) {
	older, newer := olderNewer()

	walk := func(seed, want storage.ULID) []traversedCandidate {
		t.Helper()
		e, store := newRealStoreEngine(t)
		ctx := context.Background()
		ws := store.VaultPrefix("traverse-symmetry")

		seedEndpoints(t, store, ws, older, newer)

		// One symmetric edge, written older -> newer.
		if err := store.WriteAssociation(ctx, ws, older, newer, &storage.Association{
			TargetID: newer, Weight: 0.9, RelType: storage.RelRelatesTo,
		}); err != nil {
			t.Fatalf("WriteAssociation: %v", err)
		}

		req := &ActivateRequest{HopDepth: 2}
		profile := GetProfile("default")
		if profile == nil {
			t.Fatal("GetProfile(\"default\") returned nil")
		}
		// rrfScore 1.0 is far above anything phase 3 produces; see the note
		// above — the gate itself is #801's problem, not this test's subject.
		return e.phase5Traverse(ctx, req, ws, profile, []fusedCandidate{{id: seed, rrfScore: 1.0}})
	}

	fromSource := walk(older, newer)
	fromTarget := walk(newer, older)

	found := func(got []traversedCandidate, want storage.ULID) bool {
		for _, c := range got {
			if c.id == want {
				return true
			}
		}
		return false
	}

	if !found(fromSource, newer) {
		t.Fatalf("BFS from the edge SOURCE did not reach the target (%d discovered)", len(fromSource))
	}
	if !found(fromTarget, older) {
		t.Fatalf("#800: BFS from the edge TARGET did not reach the source — a symmetric "+
			"edge is only walkable in the direction the writer picked (%d discovered)", len(fromTarget))
	}
}

// rankingNeighborsFailingStore is a real store whose UNION read always fails,
// standing in for a reverse (0x04) iterator error. The forward half is
// untouched and still answers, which is exactly the shape that makes a
// forward-only fallback look attractive.
type rankingNeighborsFailingStore struct {
	ActivationStore
	calls int
}

func (s *rankingNeighborsFailingStore) GetRankingNeighbors(
	_ context.Context, _ [8]byte, _ []storage.ULID, _ int,
) (map[storage.ULID][]storage.Association, error) {
	s.calls++
	return nil, errors.New("synthetic reverse-scan failure")
}

// TestHebbianBoost_UnionReadFailurePreservesSymmetricOrder is the ORDERING pin
// on COG-31's degradation path, and the reason phase4HebbianBoost does not fall
// back to a forward-only read.
//
// The fixture is #800's root cause verbatim: one recent engram R, two
// candidates A and B, one RelCoActivated edge of IDENTICAL weight each, differing
// only in the orientation the writer happened to pick — A→R forward, R→B inbound
// at B. The corpus says A and B are equally co-activated with R; nothing may make
// them unequal.
//
//	healthy union            A = w, B = w  (tie preserved)
//	forward-only fallback    A = w, B = 0  (tie DESTROYED — this is #800)
//	warn + skip              A = 0, B = 0  (tie preserved, signal dropped)
//
// hebbianBoost MULTIPLIES the final RRF score (final = rrf × (1+heb+trans)), so
// the middle row is a 33% final-score separation between two candidates the
// corpus says are equal. A forward-only fallback preserves absolute signal for
// related-vs-unrelated and re-enters the invariant's own failure mode for
// symmetric pairs — it is a TRADE, not a strict improvement. This test picks the
// side: a failed union drops the whole Hebbian signal uniformly (as an embed
// failure drops the whole vector signal rather than half-applying it) and says
// so in the log. Ranking degrades; it does not become confidently wrong.
func TestHebbianBoost_UnionReadFailurePreservesSymmetricOrder(t *testing.T) {
	e, store := newRealStoreEngine(t)
	ctx := context.Background()
	ws := store.VaultPrefix("symmetry")
	const vaultID uint32 = 7
	const w float32 = 0.5

	base := time.Unix(1_700_000_000, 0)
	a := storage.NewULIDWithTime(base)                     // A→R : forward at A
	r := storage.NewULIDWithTime(base.Add(24 * time.Hour)) // the recent engram
	b := storage.NewULIDWithTime(base.Add(48 * time.Hour)) // R→B : inbound at B

	seedEndpoints(t, store, ws, a, r, b)

	// Identical weight, identical RelType — only the orientation differs.
	for _, pair := range [][2]storage.ULID{{a, r}, {r, b}} {
		if err := store.WriteAssociation(ctx, ws, pair[0], pair[1], &storage.Association{
			TargetID: pair[1], Weight: 0.01, RelType: storage.RelCoActivated,
		}); err != nil {
			t.Fatalf("WriteAssociation: %v", err)
		}
		if err := store.UpdateAssocWeight(ctx, ws, pair[0], pair[1], w, 1); err != nil {
			t.Fatalf("UpdateAssocWeight: %v", err)
		}
	}

	// Only R is recent, so A's boost comes from A→R and B's from R→B.
	e.assocLog.Record(LogEntry{VaultID: vaultID, At: time.Now(), EngramIDs: []storage.ULID{r}})

	// Control arm: with a healthy union both candidates score, and score EQUALLY.
	healthy := []fusedCandidate{{id: a, rrfScore: 0.5}, {id: b, rrfScore: 0.5}}
	e.phase4HebbianBoost(ctx, ws, vaultID, healthy)
	t.Logf("HEALTHY UNION            A=%.9g B=%.9g", healthy[0].hebbianBoost, healthy[1].hebbianBoost)
	if healthy[0].hebbianBoost <= 0 || healthy[1].hebbianBoost <= 0 {
		t.Fatalf("UNDERPOWERED, not a pass: the control arm did not score "+
			"(A=%v B=%v) — the fixture, not the degradation path, is broken",
			healthy[0].hebbianBoost, healthy[1].hebbianBoost)
	}
	if math.Abs(healthy[0].hebbianBoost-healthy[1].hebbianBoost) >= 1e-9 {
		t.Fatalf("UNDERPOWERED: the control arm is already asymmetric (A=%.9g B=%.9g); "+
			"the degraded arm below cannot mean anything",
			healthy[0].hebbianBoost, healthy[1].hebbianBoost)
	}

	// Failure arm: the union read errors on every call.
	failing := &rankingNeighborsFailingStore{ActivationStore: store}
	e.store = failing
	degraded := []fusedCandidate{{id: a, rrfScore: 0.5}, {id: b, rrfScore: 0.5}}
	logged := captureWarn(t, func() {
		e.phase4HebbianBoost(ctx, ws, vaultID, degraded)
	})
	t.Logf("FAILED UNION             A=%.9g B=%.9g", degraded[0].hebbianBoost, degraded[1].hebbianBoost)

	if failing.calls == 0 {
		t.Fatal("UNDERPOWERED: the failing store was never consulted")
	}

	// THE ordering property. Equal in the corpus, equal under degradation.
	if degraded[0].hebbianBoost != degraded[1].hebbianBoost {
		t.Fatalf("a failed union read separated two EQUALLY co-activated candidates: "+
			"A=%.9g B=%.9g (healthy: A=%.9g B=%.9g). Only the writer-chosen orientation "+
			"differs between them — this is #800's defect reinstated on the degradation "+
			"path. Drop the whole Hebbian signal, do not keep the forward half.",
			degraded[0].hebbianBoost, degraded[1].hebbianBoost,
			healthy[0].hebbianBoost, healthy[1].hebbianBoost)
	}
	// Nothing may be fabricated either: the read failed, so there is no signal.
	if degraded[0].hebbianBoost != 0 {
		t.Errorf("a failed union read still produced a boost of %.9g, want 0",
			degraded[0].hebbianBoost)
	}

	// Principle #2's LOUD half. Silently zeroing the boost was the original
	// defect; zeroing it quietly-but-differently is the same defect.
	if !strings.Contains(logged, "hebbian ranking-neighbor read failed") {
		t.Errorf("no WARN for the failed union read; logged: %q", logged)
	}
	if !strings.Contains(logged, "synthetic reverse-scan failure") {
		t.Errorf("the WARN did not carry the underlying error; logged: %q", logged)
	}
}
