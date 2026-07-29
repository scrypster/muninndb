package activation_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// Issue #711: calibrated FTS relevance -> real abstention.
//
// Root cause (see .claude/deep-review/2026-07-29-fts-abstention-design.md):
// full_text_relevance was tanh(raw unbounded BM25). Raw BM25 saturates tanh by
// x~=3, so a SINGLE common word (e.g. "how") reaching bm25~=3-6 reports
// full_text_relevance ~=0.9999 -- indistinguishable from a genuine multi-term
// match. A threshold gate cannot filter a dishonest ~1.0 score, so nonsense
// queries with no real corpus overlap never abstain.
//
// This test builds a small, realistic ~30-engram corpus on a single topic
// (software engineering notes) via the REAL fts.Index (not a stub), wires it
// into a real activation.ActivationEngine with no HNSW/embedder (forcing the
// FTS-only path so the effect is isolated), and checks:
//
//   (a) a nonsense query with no real corpus content-word overlap abstains
//       (0 results at the default threshold) -- and this must FAIL if the
//       old tanh(raw BM25) mapping is restored (RED-sanity requirement).
//   (b) a genuine on-topic query still returns its correct match above
//       threshold (the fix must not gut recall).
// ---------------------------------------------------------------------------

func openTestActivationFTS(t *testing.T) *fts.Index {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-fts-abstention-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		os.RemoveAll(dir)
	})
	return fts.New(db)
}

// devNoteCorpus is ~30 realistic dev-note memories sharing common software
// vocabulary. Exactly one ("how we validate schema migrations") contains the
// common word "how" prominently in its concept field (field weight 3.0) --
// this is the exact shape of the sourdough/Vuetify bug from the design doc:
// a single common token in a high-weight field saturates raw BM25.
//
// One engram ("dbPerfID") is the sole genuine match for "database performance
// tuning", covering all three query terms.
func devNoteCorpus(t *testing.T, idx *fts.Index, store *stubStore) (ws [8]byte, dbPerfID storage.ULID) {
	t.Helper()
	ws = store.VaultPrefix("")

	type note struct {
		concept string
		content string
		tags    []string
	}
	notes := []note{
		{"How we validate schema migrations", "This documents how the team validates schema migrations before every deploy to production.", nil},
		{"Auth token rotation policy", "Access tokens rotate every 24 hours; refresh tokens are revoked on password change.", []string{"auth"}},
		{"CI pipeline flakiness", "Intermittent failures in the CI pipeline traced to a race condition in the test runner setup.", []string{"ci"}},
		{"Deploy rollback procedure", "Rolling back a bad deploy requires reverting the migration and redeploying the previous image tag.", []string{"deploy"}},
		{"Git branch naming convention", "Feature branches use the pattern feature/short-description; hotfixes use hotfix/ticket-id.", nil},
		{"Code review checklist", "Reviewers check for test coverage, naming clarity, and absence of dead code before approving.", []string{"review"}},
		{"Monitoring alert thresholds", "Alert thresholds were tuned to reduce noisy paging while still catching real incidents.", []string{"monitoring"}},
		{"Logging format standard", "All services emit structured JSON logs with a request ID for cross-service tracing.", nil},
		{"Feature flag rollout plan", "New features roll out to 5% of traffic first, then ramp to 100% over a week if metrics hold.", []string{"flags"}},
		{"API rate limiting design", "Rate limits are enforced per API key using a token bucket algorithm at the gateway.", nil},
		{"Onboarding checklist for new engineers", "New engineers get repo access, a laptop, and a pairing session in their first week.", nil},
		{"Incident postmortem process", "Postmortems are blameless and focus on process gaps rather than individual mistakes.", []string{"incident"}},
		{"Database performance tuning guide", "Notes on database performance tuning: query indexes, connection pooling, and cache tuning all improved throughput significantly.", []string{"database"}},
		{"Test suite parallelization", "Splitting the test suite across four workers cut CI runtime from twelve minutes to four.", []string{"testing"}},
		{"Secrets management approach", "Secrets live in a vault service and are injected as environment variables at deploy time.", nil},
		{"Frontend bundle size budget", "The frontend build fails CI if the main bundle exceeds the configured size budget.", nil},
		{"Backup and restore drill", "Quarterly restore drills confirm backups are actually usable, not just present.", []string{"backup"}},
		{"Service mesh retry policy", "Retries use exponential backoff with jitter to avoid thundering-herd overload downstream.", nil},
		{"Dependency upgrade cadence", "Non-major dependency upgrades run automatically each week via a scheduled job.", nil},
		{"On-call rotation schedule", "On-call rotates weekly across the team, with a documented handoff checklist.", []string{"oncall"}},
		{"Cache invalidation strategy", "Cache entries carry a version tag so a schema change invalidates stale entries automatically.", nil},
		{"Load testing methodology", "Load tests ramp traffic gradually and record latency percentiles at each step.", []string{"testing"}},
		{"Error budget policy", "When the error budget is exhausted, feature work pauses in favor of reliability work.", nil},
		{"Config management approach", "Configuration is versioned alongside code and validated by a schema check in CI.", nil},
		{"Data retention policy", "Raw event data is retained for 90 days; aggregates are retained indefinitely.", nil},
		{"Pairing session norms", "Pairing sessions rotate driver/navigator every 25 minutes to keep both engineers engaged.", nil},
		{"Release notes template", "Release notes group changes into added, changed, fixed, and removed sections.", nil},
		{"Access review cadence", "Quarterly access reviews confirm each engineer's permissions still match their current role.", []string{"auth"}},
		{"Staging environment parity", "Staging mirrors production configuration closely enough to catch most environment-specific bugs.", nil},
		{"Runbook maintenance policy", "Runbooks are reviewed every quarter and retired if the underlying system no longer exists.", nil},
	}

	for _, n := range notes {
		eng := &storage.Engram{
			Concept:    n.concept,
			Content:    n.content,
			Tags:       n.tags,
			Confidence: 1.0,
			Stability:  30.0,
			CreatedAt:  time.Now(),
		}
		store.writeEngram(eng)
		if err := idx.IndexEngram(ws, [16]byte(eng.ID), eng.Concept, "", eng.Content, eng.Tags); err != nil {
			t.Fatalf("IndexEngram(%q): %v", n.concept, err)
		}
		if n.concept == "Database performance tuning guide" {
			dbPerfID = eng.ID
		}
	}
	if dbPerfID == (storage.ULID{}) {
		t.Fatal("test setup: db-perf engram not found")
	}
	return ws, dbPerfID
}

// TestFTSAbstention_NonsenseQueryReturnsNoConfidentMatch is the RED/GREEN case
// for issue #711. It MUST fail (nonsense returns a confident match) while the
// tanh(raw BM25) mapping is in place, and MUST pass after the calibrated
// IDF-weighted coverage fix lands.
func TestFTSAbstention_NonsenseQueryReturnsNoConfidentMatch(t *testing.T) {
	store := newStubStore()
	idx := openTestActivationFTS(t)
	_, _ = devNoteCorpus(t, idx, store)

	// No HNSW, no embedder: pure FTS-only path so the effect under test is
	// isolated from the (separately deferred, per the design) semantic-floor hole.
	eng := activation.New(store, activation.NewFTSAdapter(idx), nil, activation.NewNoopEmbedder())
	defer eng.Close()

	result, err := eng.Run(context.Background(), &activation.ActivateRequest{
		Context:    []string{"how to bake sourdough bread"},
		Threshold:  0.1,
		MaxResults: 10,
		IncludeWhy: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Activations) > 0 {
		top := result.Activations[0]
		t.Errorf("nonsense query %q returned %d result(s) at threshold 0.1; want 0 (abstention). "+
			"Top match: concept=%q score=%.4f full_text_relevance=%.4f semantic_similarity=%.4f why=%q",
			"how to bake sourdough bread", len(result.Activations),
			top.Engram.Concept, top.Score, top.Components.FullTextRelevance, top.Components.SemanticSimilarity, top.Why)
	} else {
		t.Logf("nonsense query correctly abstained: 0 results at threshold 0.1")
	}
}

// TestFTSAbstention_PositiveControl_GenuineQueryStillMatches is the positive
// control: a real on-topic query, run through the identical pipeline, must
// still surface its genuine match above threshold. This proves the fix does
// not gut recall along with the nonsense-abstention behavior.
func TestFTSAbstention_PositiveControl_GenuineQueryStillMatches(t *testing.T) {
	store := newStubStore()
	idx := openTestActivationFTS(t)
	_, dbPerfID := devNoteCorpus(t, idx, store)

	eng := activation.New(store, activation.NewFTSAdapter(idx), nil, activation.NewNoopEmbedder())
	defer eng.Close()

	result, err := eng.Run(context.Background(), &activation.ActivateRequest{
		Context:    []string{"database performance tuning"},
		Threshold:  0.1,
		MaxResults: 10,
		IncludeWhy: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Activations) == 0 {
		t.Fatal("genuine query 'database performance tuning' returned 0 results, want >= 1 (positive control)")
	}
	top := result.Activations[0]
	if top.Engram.ID != dbPerfID {
		t.Errorf("top result = %q, want the db-perf-tuning engram; full result set:", top.Engram.Concept)
		for _, a := range result.Activations {
			t.Logf("  concept=%q score=%.4f full_text_relevance=%.4f", a.Engram.Concept, a.Score, a.Components.FullTextRelevance)
		}
	}
	t.Logf("genuine query matched: concept=%q score=%.4f full_text_relevance=%.4f semantic_similarity=%.4f why=%q",
		top.Engram.Concept, top.Score, top.Components.FullTextRelevance, top.Components.SemanticSimilarity, top.Why)
}
