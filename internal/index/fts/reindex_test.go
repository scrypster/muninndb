package fts

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// stemOf returns the single index term a word is stored under, so an assertion
// can name the human word ("auditors") and still read the right term-stats key
// ("auditor").
func stemOf(t *testing.T, word string) string {
	t.Helper()
	toks := Tokenize(word)
	if len(toks) != 1 {
		t.Fatalf("Tokenize(%q) = %v, want exactly one term", word, toks)
	}
	return toks[0]
}

// dfOf reads the committed document frequency for a word's index term.
// missing reports whether the term-stats key exists at all — the distinction
// matters, because getIDF treats a missing key as "the corpus has never seen
// this term" and charges idfMax.
func dfOf(t *testing.T, idx *Index, ws [8]byte, word string) (df uint32, missing bool) {
	t.Helper()
	val, closer, err := idx.db.Get(keys.TermStatsKey(ws, stemOf(t, word)))
	if err != nil {
		return 0, true
	}
	defer closer.Close()
	if len(val) < 4 {
		return 0, true
	}
	return binary.BigEndian.Uint32(val[0:4]), false
}

// TestReindexEngram_IsStatsNeutral is the pin for the defect that made a
// delete-then-index retag corrode its own engram's score.
//
// fts.IndexEngram increments the per-term document frequency df_t for EVERY term
// of the document and bumps TotalEngrams/AvgDocLen; fts.DeleteEngram decrements
// neither. So the pair moved (N, df) → (N+1, df+1) on every retag. getIDF is
// log((N−df+0.5)/(df+0.5)+1) ≡ ln((N+1)/(df+0.5)): the numerator barely moves,
// the denominator moves by a whole document. The N drift is second-order (idfMax
// grows like ln N); the df_t drift is FIRST-ORDER and per-call, which is what
// the earlier "second-order, directionally safe" reading of this got wrong.
//
// This test asserts the arithmetic directly, term by term:
//
//   - TotalEngrams and AvgDocLen do not move at all.
//   - a term present in BOTH documents keeps its df (the whole point — a retag
//     must not make the engram's own content rarer-looking to itself).
//   - a term that genuinely ENTERED moves +1; one that genuinely LEFT moves −1.
//   - a tag token that coincides with a content token is NOT a membership change,
//     which is why membership is computed per term over the whole document rather
//     than per field.
func TestReindexEngram_IsStatsNeutral(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("reindex-neutral")

	// A small corpus so df values are small and readable.
	for i := range 5 {
		id := [16]byte{byte(i + 1)}
		if err := idx.IndexEngram(ws, id, "migration plan", "", "the plan covers the staged rollout", nil); err != nil {
			t.Fatalf("IndexEngram(background %d): %v", i, err)
		}
	}

	target := [16]byte{99}
	const concept = "migration plan"
	const content = "the plan covers the staged rollout and the rollback drill"
	// "rollout" is deliberately BOTH a tag and a content word.
	prev := Document{Concept: concept, Content: content, Tags: []string{"zebrafish", "rollout"}}
	if err := idx.IndexEngram(ws, target, prev.Concept, prev.CreatedBy, prev.Content, prev.Tags); err != nil {
		t.Fatalf("IndexEngram(target): %v", err)
	}

	statsBefore := idx.readStats(ws)
	type dfSnap struct {
		df      uint32
		missing bool
	}
	before := map[string]dfSnap{}
	for _, w := range []string{"plan", "rollback", "rollout", "zebrafish", "antelope"} {
		df, missing := dfOf(t, idx, ws, w)
		before[w] = dfSnap{df, missing}
	}
	if before["zebrafish"].missing || before["zebrafish"].df != 1 {
		t.Fatalf("precondition: df(zebrafish) = %v (missing=%v), want 1", before["zebrafish"].df, before["zebrafish"].missing)
	}
	if !before["antelope"].missing {
		t.Fatalf("precondition: df(antelope) exists (%d) before it was ever indexed", before["antelope"].df)
	}

	// The retag under test: zebrafish leaves, antelope enters, rollout stays a
	// tag-word only in the sense that it is also still in the content.
	next := Document{Concept: concept, Content: content, Tags: []string{"antelope", "rollout"}}
	if err := idx.ReindexEngram(ws, target, prev, next); err != nil {
		t.Fatalf("ReindexEngram: %v", err)
	}

	statsAfter := idx.readStats(ws)
	if statsAfter.TotalEngrams != statsBefore.TotalEngrams {
		t.Errorf("TotalEngrams %d → %d across a retag, want unchanged (a retag adds no document to the corpus)",
			statsBefore.TotalEngrams, statsAfter.TotalEngrams)
	}
	if statsAfter.AvgDocLen != statsBefore.AvgDocLen {
		t.Errorf("AvgDocLen %v → %v across a retag, want unchanged", statsBefore.AvgDocLen, statsAfter.AvgDocLen)
	}

	// Terms present in both documents: df must not budge. These are the ones
	// whose inflation made the engram's own score decay.
	for _, w := range []string{"plan", "rollback", "rollout"} {
		df, missing := dfOf(t, idx, ws, w)
		if missing || df != before[w].df {
			t.Errorf("df(%s) %d → %d (missing=%v) across a retag, want unchanged — the term is in both the old and the new document",
				w, before[w].df, df, missing)
		}
	}

	// Genuine membership changes move by exactly one, in the right direction.
	if df, missing := dfOf(t, idx, ws, "zebrafish"); missing || df != before["zebrafish"].df-1 {
		t.Errorf("df(zebrafish) %d → %d (missing=%v), want %d — the tag left the document",
			before["zebrafish"].df, df, missing, before["zebrafish"].df-1)
	}
	if df, missing := dfOf(t, idx, ws, "antelope"); missing || df != 1 {
		t.Errorf("df(antelope) = %d (missing=%v), want 1 — the tag entered the document", df, missing)
	}
}

// TestReindexEngram_ScoreSurvivesRepeatedRetags is the same defect from the other
// end: the symptom, not the arithmetic. A memory queried on its own UNCHANGED
// content must not get harder to find because its owner keeps curating its tags —
// and this tool's own description advertises exactly that pattern
// ("mutable tag conventions such as due:<ISO-date>"), so an agent maintaining a
// daily due: tag crosses ten retags in under two weeks.
//
// Measured with the delete-then-index pair on a 60-engram corpus:
//
//	retags=  0 score=0.4357
//	retags=  1 score=0.4002      -8.1% from ONE retag
//	retags= 10 score=0.2696      below a 0.3 default threshold
//	retags= 40 score=0.1446
func TestReindexEngram_ScoreSurvivesRepeatedRetags(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 200})
	ws := store.VaultPrefix("reindex-score")
	ctx := context.Background()

	for i := range 59 {
		id := [16]byte{byte(i/250 + 1), byte(i%250 + 1)}
		if err := idx.IndexEngram(ws, id, "release notes", "", "the release notes describe the deployment steps", nil); err != nil {
			t.Fatalf("IndexEngram(background %d): %v", i, err)
		}
	}

	target := [16]byte{200, 200}
	const concept = "capacitor recalibration"
	const content = "the zorbfluke procedure needs a torque wrench and a steady hand"
	doc := Document{Concept: concept, Content: content, Tags: []string{"due:2026-01-01"}}
	if err := idx.IndexEngram(ws, target, doc.Concept, doc.CreatedBy, doc.Content, doc.Tags); err != nil {
		t.Fatalf("IndexEngram(target): %v", err)
	}

	// One rare term the target really has plus one the corpus has never seen —
	// the shape a real recall takes, and the shape COG-24 calibrates.
	const query = "zorbfluke plarfnog"
	scoreOf := func() float64 {
		hits, err := idx.Search(ctx, ws, query, 50)
		if err != nil {
			t.Fatalf("Search(%q): %v", query, err)
		}
		for _, h := range hits {
			if h.ID == target {
				return h.Score
			}
		}
		t.Fatalf("target absent from results for %q", query)
		return 0
	}

	// Baseline is taken AFTER one retag so the comparison isn't confounded by the
	// tag token count changing between "no tags" and "one tags-field token set".
	prev := doc
	next := doc
	next.Tags = []string{"due:2026-02-02"}
	if err := idx.ReindexEngram(ws, target, prev, next); err != nil {
		t.Fatalf("ReindexEngram(baseline): %v", err)
	}
	prev = next
	baseline := scoreOf()

	// Same shape of tag, 40 more times — the due:<ISO-date> treadmill.
	for i := range 40 {
		next = prev
		next.Tags = []string{"due:2026-03-" + twoDigit(i+1)}
		if err := idx.ReindexEngram(ws, target, prev, next); err != nil {
			t.Fatalf("ReindexEngram(retag %d): %v", i, err)
		}
		prev = next
	}

	after := scoreOf()
	t.Logf("score after 1 retag = %.4f; after 41 retags = %.4f", baseline, after)
	if after < baseline*0.99 {
		t.Errorf("score decayed from %.4f to %.4f over 40 retags (%.1f%% loss) — a retag must not make the engram harder to find by its own unchanged content",
			baseline, after, 100*(baseline-after)/baseline)
	}
}

// TestAdjustDF_DecrementClampsAtZero pins the clamp in adjustDF. ReindexEngram is
// the first decrementing DF path in the codebase, so it is the first way df_t can
// be driven BELOW its true value, and uint32 underflow there is not a rounding
// error — it is a sign flip in the score.
//
// Reaching the clamp needs a spurious decrement, which is reachable: the
// write-path initial index is always an ftsWorker.Submit, and Submit returns
// false when the queue is full (internal/index/fts/worker.go:145) or after Stop
// (:133). An engram whose initial index job was dropped has postings for nothing,
// so its tag tokens were never counted — yet a later retag hands ReindexEngram a
// prev containing them, and the −1 lands on a df another engram legitimately
// owns.
//
// Why the test needs a SECOND decrement rather than one: the `existed` guard makes
// the first decrement on a MISSING term-stats key a no-op, so a test that only
// decrements an uncounted term never reaches the arithmetic. Hence the shape
// below — engram A really holds the term (df=1, key exists), then two
// never-indexed engrams each hand ReindexEngram a prev containing it. The first
// takes df 1 → 0 legitimately-looking; the second is the one that would wrap.
//
// Measured, with and without the clamp (getIDF ≡ log((N−df+0.5)/(df+0.5)+1)):
//
//	WITH clamp:     #1 df=0 | #2 df=0            getIDF(zarquon, N=3) =   2.0794
//	WITHOUT clamp:  #1 df=0 | #2 df=4294967295   getIDF(zarquon, N=3) = -20.7944
//
// A NEGATIVE idf is worse than a worthless one: COG-24's numerator is
// Σ idf_t'·cov_t, so a document that CONTAINS the term contributes a negative
// term and ranks BELOW one that does not. The term stops being weak evidence and
// becomes an active penalty.
func TestAdjustDF_DecrementClampsAtZero(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("adjustdf-clamp")

	// A really holds the term, so the term-stats key exists at df=1 and the
	// `existed` guard cannot short-circuit the decrements below.
	a := [16]byte{1}
	if err := idx.IndexEngram(ws, a, "alpha record", "", "the alpha body", []string{"zarquon"}); err != nil {
		t.Fatalf("IndexEngram(A): %v", err)
	}
	if df, missing := dfOf(t, idx, ws, "zarquon"); missing || df != 1 {
		t.Fatalf("precondition: df(zarquon) = %d (missing=%v), want 1", df, missing)
	}

	// B and C were NEVER indexed — the dropped-Submit case. Each hands
	// ReindexEngram a prev whose tag tokens were never counted.
	prev := Document{Concept: "beta record", Content: "the beta body", Tags: []string{"zarquon"}}
	next := Document{Concept: "beta record", Content: "the beta body"}

	if err := idx.ReindexEngram(ws, [16]byte{2}, prev, next); err != nil {
		t.Fatalf("ReindexEngram(B): %v", err)
	}
	df1, missing1 := dfOf(t, idx, ws, "zarquon")
	if missing1 || df1 != 0 {
		t.Fatalf("after the first spurious decrement df(zarquon) = %d (missing=%v), want 0", df1, missing1)
	}

	if err := idx.ReindexEngram(ws, [16]byte{3}, prev, next); err != nil {
		t.Fatalf("ReindexEngram(C): %v", err)
	}
	df2, missing2 := dfOf(t, idx, ws, "zarquon")

	// The assertion under test: a decrement at df=0 stays at 0 rather than
	// wrapping to 2^32−1.
	if missing2 || df2 != 0 {
		t.Errorf("df(zarquon) = %d (missing=%v) after decrementing at df=0, want 0 — an unclamped uint32 decrement wraps to 4294967295", df2, missing2)
	}

	// And the consequence, stated in the units that matter: IDF must stay
	// positive. At the wrapped value this is −20.7944, which makes a document
	// holding the term rank BELOW one that does not.
	idx.InvalidateIDFCache()
	idf := idx.getIDF(ws, stemOf(t, "zarquon"), 3)
	t.Logf("getIDF(zarquon, N=3) with df=%d → %.4f", df2, idf)
	if idf <= 0 {
		t.Errorf("getIDF(zarquon, N=3) = %.4f with df=%d, want > 0 — a negative IDF penalizes documents that CONTAIN the term (COG-24's numerator is Σ idf_t'·cov_t)", idf, df2)
	}
}

// twoDigit renders 1..99 zero-padded, so every generated due: tag tokenizes to
// the same number of terms and only the day component's membership changes.
func twoDigit(n int) string {
	return string([]byte{byte('0' + n/10), byte('0' + n%10)})
}
