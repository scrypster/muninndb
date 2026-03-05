package activation

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// Tests for package-internal helpers: extractTimeBounds, passesMetaFilter,
// resolveWeights, computeGatedActivation, computeComponents, buildWhy
// ---------------------------------------------------------------------------

func TestExtractTimeBounds_NoFilters(t *testing.T) {
	since, before, has := extractTimeBounds(nil)
	if has {
		t.Error("expected no bounds for nil filters")
	}
	if !since.IsZero() || !before.IsZero() {
		t.Error("expected zero times")
	}
}

func TestExtractTimeBounds_Since(t *testing.T) {
	now := time.Now()
	filters := []Filter{{Field: "created_after", Op: "gt", Value: now}}
	since, before, has := extractTimeBounds(filters)
	if !has {
		t.Error("expected has=true")
	}
	if !since.Equal(now) {
		t.Errorf("since = %v, want %v", since, now)
	}
	if !before.IsZero() {
		t.Error("expected zero before")
	}
}

func TestExtractTimeBounds_Before(t *testing.T) {
	now := time.Now()
	filters := []Filter{{Field: "created_before", Op: "lt", Value: now}}
	_, before, has := extractTimeBounds(filters)
	if !has || !before.Equal(now) {
		t.Errorf("before = %v (has=%v), want %v", before, has, now)
	}
}

func TestExtractTimeBounds_Both(t *testing.T) {
	t1 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	filters := []Filter{
		{Field: "created_after", Op: "gt", Value: t1},
		{Field: "created_before", Op: "lt", Value: t2},
	}
	since, before, has := extractTimeBounds(filters)
	if !has || !since.Equal(t1) || !before.Equal(t2) {
		t.Errorf("got since=%v before=%v has=%v", since, before, has)
	}
}

func TestExtractTimeBounds_WrongType(t *testing.T) {
	filters := []Filter{{Field: "created_after", Op: "gt", Value: "not-a-time"}}
	_, _, has := extractTimeBounds(filters)
	if has {
		t.Error("expected has=false for non-time value")
	}
}

// ---------------------------------------------------------------------------
// passesMetaFilter
// ---------------------------------------------------------------------------

func TestPassesMetaFilter_Empty(t *testing.T) {
	eng := &storage.Engram{State: storage.StateActive}
	if !passesMetaFilter(eng, nil) {
		t.Error("nil filters should pass")
	}
}

func TestPassesMetaFilter_StateEq(t *testing.T) {
	eng := &storage.Engram{State: storage.StateActive}
	pass := passesMetaFilter(eng, []Filter{
		{Field: "state", Op: "eq", Value: storage.StateActive},
	})
	if !pass {
		t.Error("should pass for matching state")
	}

	fail := passesMetaFilter(eng, []Filter{
		{Field: "state", Op: "eq", Value: storage.StateSoftDeleted},
	})
	if fail {
		t.Error("should fail for mismatched state")
	}
}

func TestPassesMetaFilter_StateNeq(t *testing.T) {
	eng := &storage.Engram{State: storage.StateActive}
	pass := passesMetaFilter(eng, []Filter{
		{Field: "state", Op: "neq", Value: storage.StateSoftDeleted},
	})
	if !pass {
		t.Error("should pass for neq different state")
	}

	fail := passesMetaFilter(eng, []Filter{
		{Field: "state", Op: "neq", Value: storage.StateActive},
	})
	if fail {
		t.Error("should fail for neq same state")
	}
}

func TestPassesMetaFilter_CreatedAfter(t *testing.T) {
	threshold := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	eng := &storage.Engram{CreatedAt: time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)}
	if !passesMetaFilter(eng, []Filter{{Field: "created_after", Value: threshold}}) {
		t.Error("should pass — engram created after threshold")
	}

	old := &storage.Engram{CreatedAt: time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)}
	if passesMetaFilter(old, []Filter{{Field: "created_after", Value: threshold}}) {
		t.Error("should fail — engram created before threshold")
	}
}

func TestPassesMetaFilter_CreatedBefore(t *testing.T) {
	threshold := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	eng := &storage.Engram{CreatedAt: time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)}
	if !passesMetaFilter(eng, []Filter{{Field: "created_before", Value: threshold}}) {
		t.Error("should pass — engram created before threshold")
	}

	newer := &storage.Engram{CreatedAt: time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)}
	if passesMetaFilter(newer, []Filter{{Field: "created_before", Value: threshold}}) {
		t.Error("should fail — engram created after threshold")
	}
}

func TestPassesMetaFilter_Combined(t *testing.T) {
	eng := &storage.Engram{
		State:     storage.StateActive,
		CreatedAt: time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC),
	}
	filters := []Filter{
		{Field: "state", Op: "eq", Value: storage.StateActive},
		{Field: "created_after", Value: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
		{Field: "created_before", Value: time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
	if !passesMetaFilter(eng, filters) {
		t.Error("should pass all combined filters")
	}
}

// ---------------------------------------------------------------------------
// resolveWeights
// ---------------------------------------------------------------------------

func TestResolveWeights_NilRequest(t *testing.T) {
	def := DefaultWeights{
		SemanticSimilarity: 0.4,
		FullTextRelevance:  0.3,
		DecayFactor:        0.1,
		HebbianBoost:       0.1,
		AccessFrequency:    0.05,
		Recency:            0.05,
	}
	rw := resolveWeights(nil, def)
	if !rw.UseACTR {
		t.Error("nil weights should default to UseACTR=true")
	}
	if rw.ACTRDecay != 0.5 {
		t.Errorf("ACTRDecay = %v, want 0.5", rw.ACTRDecay)
	}
	if rw.ACTRHebScale != 4.0 {
		t.Errorf("ACTRHebScale = %v, want 4.0", rw.ACTRHebScale)
	}
	if rw.SemanticSimilarity < 0.39 || rw.SemanticSimilarity > 0.41 {
		t.Errorf("SemanticSimilarity = %v, want ~0.4", rw.SemanticSimilarity)
	}
}

func TestResolveWeights_WithRequest(t *testing.T) {
	req := &Weights{
		SemanticSimilarity: 0.5,
		FullTextRelevance:  0.3,
		ACTRDecay:          0.7,
		ACTRHebScale:       6.0,
	}
	rw := resolveWeights(req, DefaultWeights{})
	if rw.ACTRDecay < 0.69 || rw.ACTRDecay > 0.71 {
		t.Errorf("ACTRDecay = %v, want ~0.7", rw.ACTRDecay)
	}
	if rw.ACTRHebScale < 5.99 || rw.ACTRHebScale > 6.01 {
		t.Errorf("ACTRHebScale = %v, want ~6.0", rw.ACTRHebScale)
	}
}

func TestResolveWeights_CGDN(t *testing.T) {
	req := &Weights{UseCGDN: true}
	rw := resolveWeights(req, DefaultWeights{})
	if !rw.UseCGDN {
		t.Error("expected UseCGDN=true")
	}
	if rw.CGDNAlpha != 1.5 {
		t.Errorf("CGDNAlpha = %v, want 1.5", rw.CGDNAlpha)
	}
	if rw.CGDNBeta != 0.5 {
		t.Errorf("CGDNBeta = %v, want 0.5", rw.CGDNBeta)
	}
	if rw.CGDNPower != 2.0 {
		t.Errorf("CGDNPower = %v, want 2.0", rw.CGDNPower)
	}
}

func TestResolveWeights_CGDNCustom(t *testing.T) {
	req := &Weights{
		UseCGDN:   true,
		CGDNAlpha: 2.0,
		CGDNBeta:  0.8,
		CGDNPower: 3.0,
	}
	rw := resolveWeights(req, DefaultWeights{})
	// float32→float64 conversion tolerance
	if rw.CGDNAlpha < 1.99 || rw.CGDNAlpha > 2.01 ||
		rw.CGDNBeta < 0.79 || rw.CGDNBeta > 0.81 ||
		rw.CGDNPower < 2.99 || rw.CGDNPower > 3.01 {
		t.Errorf("custom CGDN params not applied: alpha=%v beta=%v power=%v",
			rw.CGDNAlpha, rw.CGDNBeta, rw.CGDNPower)
	}
}

// ---------------------------------------------------------------------------
// computeGatedActivation
// ---------------------------------------------------------------------------

func TestComputeGatedActivation_FreshEngram(t *testing.T) {
	w := resolvedWeights{SemanticSimilarity: 0.5, FullTextRelevance: 0.3, CGDNAlpha: 1.5}
	score := computeGatedActivation(0.9, 0.5, 1.0, 0.0, w)
	if score <= 0 {
		t.Errorf("fresh engram should have positive score, got %v", score)
	}
}

func TestComputeGatedActivation_HebbianRescue(t *testing.T) {
	w := resolvedWeights{SemanticSimilarity: 0.5, FullTextRelevance: 0.3, CGDNAlpha: 1.5}
	withoutHebbian := computeGatedActivation(0.9, 0.5, 0.05, 0.0, w)
	withHebbian := computeGatedActivation(0.9, 0.5, 0.05, 0.8, w)
	if withHebbian <= withoutHebbian {
		t.Errorf("Hebbian rescue should increase score: without=%v with=%v", withoutHebbian, withHebbian)
	}
}

func TestComputeGatedActivation_GateCappedAt1(t *testing.T) {
	w := resolvedWeights{SemanticSimilarity: 1.0, FullTextRelevance: 0.0, CGDNAlpha: 0.1}
	score := computeGatedActivation(1.0, 0.0, 1.0, 1.0, w)
	if score > 1.0 {
		t.Errorf("score should not exceed 1.0 (gate capped), got %v", score)
	}
}

// ---------------------------------------------------------------------------
// computeComponents
// ---------------------------------------------------------------------------

func TestComputeComponents_BasicScoring(t *testing.T) {
	eng := &storage.Engram{
		Confidence:  0.9,
		Relevance:   0.8,
		Stability:   30.0,
		AccessCount: 5,
		CreatedAt:   time.Now().Add(-24 * time.Hour),
		LastAccess:  time.Now().Add(-1 * time.Hour),
	}
	w := resolvedWeights{
		SemanticSimilarity: 0.4,
		FullTextRelevance:  0.3,
		DecayFactor:        0.1,
		HebbianBoost:       0.1,
		AccessFrequency:    0.05,
		Recency:            0.05,
	}
	c := computeComponents(0.8, 1.0, 0.5, eng, 0, time.Now(), w)

	if c.SemanticSimilarity < 0.79 || c.SemanticSimilarity > 0.81 {
		t.Errorf("SemanticSimilarity = %v, want ~0.8", c.SemanticSimilarity)
	}
	if c.Confidence < 0.89 || c.Confidence > 0.91 {
		t.Errorf("Confidence = %v, want ~0.9", c.Confidence)
	}
	if c.Raw <= 0 || c.Raw > 1.0 {
		t.Errorf("Raw score out of range: %v", c.Raw)
	}
	if c.Final <= 0 || c.Final > 1.0 {
		t.Errorf("Final score out of range: %v", c.Final)
	}
	if c.Final > c.Raw {
		t.Errorf("Final (%v) should not exceed Raw (%v) when Confidence < 1", c.Final, c.Raw)
	}
}

func TestComputeComponents_CachedLastAccess(t *testing.T) {
	eng := &storage.Engram{
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 1,
		CreatedAt:   time.Now().Add(-48 * time.Hour),
		LastAccess:  time.Now().Add(-48 * time.Hour),
	}
	w := resolvedWeights{Recency: 1.0}
	now := time.Now()

	withoutCache := computeComponents(0, 0, 0, eng, 0, now, w)
	withCache := computeComponents(0, 0, 0, eng, now.Add(-1*time.Minute).UnixNano(), now, w)

	if withCache.Recency <= withoutCache.Recency {
		t.Errorf("cached last access should produce higher recency: cached=%v uncached=%v",
			withCache.Recency, withoutCache.Recency)
	}
}

// ---------------------------------------------------------------------------
// buildWhy
// ---------------------------------------------------------------------------

func TestBuildWhy_SemanticDominant(t *testing.T) {
	eng := &storage.Engram{Relevance: 0.5}
	c := ScoreComponents{SemanticSimilarity: 0.9, FullTextRelevance: 0.1, DecayFactor: 0.1, Confidence: 0.8}
	why := buildWhy(eng, c, nil, nil, "test query")
	if why == "" {
		t.Error("expected non-empty why string")
	}
}

func TestBuildWhy_FTSDominant(t *testing.T) {
	eng := &storage.Engram{Relevance: 0.5}
	c := ScoreComponents{SemanticSimilarity: 0.1, FullTextRelevance: 0.9, DecayFactor: 0.1, Confidence: 0.8}
	why := buildWhy(eng, c, nil, nil, "test query that is very long and exceeds forty characters for truncation testing")
	if why == "" {
		t.Error("expected non-empty why string")
	}
}

func TestBuildWhy_WithHopPath(t *testing.T) {
	eng := &storage.Engram{Relevance: 0.5}
	c := ScoreComponents{HebbianBoost: 0.9, Confidence: 0.8}
	path := []storage.ULID{{1}, {2}, {3}}
	concepts := []string{"alpha", "beta", "gamma"}
	why := buildWhy(eng, c, path, concepts, "")
	if why == "" {
		t.Error("expected non-empty why string with hops")
	}
}

func TestBuildWhy_LowConfidence(t *testing.T) {
	eng := &storage.Engram{Relevance: 0.5}
	c := ScoreComponents{SemanticSimilarity: 0.8, Confidence: 0.3}
	why := buildWhy(eng, c, nil, nil, "test")
	if why == "" {
		t.Error("expected non-empty why string")
	}
}

func TestBuildWhy_DormantEngram(t *testing.T) {
	eng := &storage.Engram{Relevance: minFloor * 1.05}
	c := ScoreComponents{DecayFactor: 0.9, Confidence: 0.8}
	why := buildWhy(eng, c, nil, nil, "")
	if why == "" {
		t.Error("expected non-empty why string for dormant engram")
	}
}

func TestBuildWhy_HopPathWithoutConcepts(t *testing.T) {
	eng := &storage.Engram{Relevance: 0.5}
	c := ScoreComponents{HebbianBoost: 0.9, Confidence: 0.8}
	path := []storage.ULID{{1}, {2}}
	why := buildWhy(eng, c, path, nil, "")
	if why == "" {
		t.Error("expected non-empty why string")
	}
}

// ---------------------------------------------------------------------------
// Internal stub store for package-internal tests.
// These stubs mirror activation_test.go stubs but live in package activation
// so they can access unexported types (fusedCandidate, traversedCandidate, etc.).
// ---------------------------------------------------------------------------

type internalStubStore struct {
	engrams      map[storage.ULID]*storage.Engram
	assocs       map[storage.ULID][]storage.Association
	recent       []storage.ULID
	lastAccessNs map[storage.ULID]int64
}

func newInternalStubStore() *internalStubStore {
	return &internalStubStore{
		engrams:      make(map[storage.ULID]*storage.Engram),
		assocs:       make(map[storage.ULID][]storage.Association),
		lastAccessNs: make(map[storage.ULID]int64),
	}
}

func (s *internalStubStore) addEngram(eng *storage.Engram) {
	if eng.ID == (storage.ULID{}) {
		eng.ID = storage.NewULID()
	}
	if eng.Confidence == 0 {
		eng.Confidence = 1.0
	}
	if eng.Stability == 0 {
		eng.Stability = 30.0
	}
	if eng.CreatedAt.IsZero() {
		eng.CreatedAt = time.Now()
	}
	if eng.LastAccess.IsZero() {
		eng.LastAccess = eng.CreatedAt
	}
	if eng.State == 0 {
		eng.State = storage.StateActive
	}
	s.engrams[eng.ID] = eng
	s.recent = append([]storage.ULID{eng.ID}, s.recent...)
}

func (s *internalStubStore) GetMetadata(_ context.Context, _ [8]byte, ids []storage.ULID) ([]*storage.EngramMeta, error) {
	out := make([]*storage.EngramMeta, 0, len(ids))
	for _, id := range ids {
		if e, ok := s.engrams[id]; ok {
			out = append(out, &storage.EngramMeta{
				ID: e.ID, CreatedAt: e.CreatedAt, LastAccess: e.LastAccess,
				Confidence: e.Confidence, Relevance: e.Relevance,
				Stability: e.Stability, AccessCount: e.AccessCount, State: e.State,
			})
		}
	}
	return out, nil
}

func (s *internalStubStore) GetEngrams(_ context.Context, _ [8]byte, ids []storage.ULID) ([]*storage.Engram, error) {
	out := make([]*storage.Engram, 0, len(ids))
	for _, id := range ids {
		if e, ok := s.engrams[id]; ok {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *internalStubStore) GetAssociations(_ context.Context, _ [8]byte, ids []storage.ULID, maxPerNode int) (map[storage.ULID][]storage.Association, error) {
	result := make(map[storage.ULID][]storage.Association)
	for _, id := range ids {
		assocs := s.assocs[id]
		if len(assocs) > maxPerNode {
			assocs = assocs[:maxPerNode]
		}
		result[id] = assocs
	}
	return result, nil
}

func (s *internalStubStore) RecentActive(_ context.Context, _ [8]byte, topK int) ([]storage.ULID, error) {
	if topK > len(s.recent) {
		topK = len(s.recent)
	}
	return s.recent[:topK], nil
}

func (s *internalStubStore) VaultPrefix(_ string) [8]byte { return [8]byte{} }

func (s *internalStubStore) EngramLastAccessNs(_ [8]byte, id storage.ULID) int64 {
	return s.lastAccessNs[id]
}

func (s *internalStubStore) EngramIDsByCreatedRange(_ context.Context, _ [8]byte, since, until time.Time, limit int) ([]storage.ULID, error) {
	return nil, nil
}

func (s *internalStubStore) RestoreArchivedEdgesTransitive(_ context.Context, _ [8]byte, _ storage.ULID, _, _ int) ([]storage.ULID, error) {
	return nil, nil
}

func (s *internalStubStore) ArchiveBloomMayContain(_ [16]byte) bool {
	return false
}

// stubTransitionStore implements PASTransitionStore for testing.
type stubTransitionStore struct {
	transitions map[storage.ULID][]storage.TransitionTarget
}

func (s *stubTransitionStore) GetTopTransitions(_ context.Context, _ [8]byte, srcID [16]byte, topK int) ([]storage.TransitionTarget, error) {
	uid := storage.ULID(srcID)
	targets := s.transitions[uid]
	if len(targets) > topK {
		targets = targets[:topK]
	}
	return targets, nil
}

func newTestActivationEngine(store ActivationStore) *ActivationEngine {
	e := &ActivationEngine{
		store:    store,
		assocLog: &ActivationLog{},
		weights: DefaultWeights{
			SemanticSimilarity: 0.35,
			FullTextRelevance:  0.25,
			DecayFactor:        0.20,
			HebbianBoost:       0.10,
			AccessFrequency:    0.05,
			Recency:            0.05,
		},
		logCh:   make(chan logItem, 64),
		logDone: make(chan struct{}),
	}
	go e.drainLog()
	return e
}

// ---------------------------------------------------------------------------
// SetTransitionStore / AssocLog — trivial getters (100% coverage)
// ---------------------------------------------------------------------------

func TestSetTransitionStore(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	if e.transStore != nil {
		t.Error("transStore should be nil initially")
	}
	ts := &stubTransitionStore{}
	e.SetTransitionStore(ts)
	if e.transStore == nil {
		t.Error("transStore should be set after SetTransitionStore")
	}

	e.SetTransitionStore(nil)
	if e.transStore != nil {
		t.Error("transStore should be nil after SetTransitionStore(nil)")
	}
}

func TestAssocLog(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	log := e.AssocLog()
	if log == nil {
		t.Fatal("AssocLog() must not return nil")
	}
	if log != e.assocLog {
		t.Error("AssocLog() should return the engine's assocLog")
	}
}

// ---------------------------------------------------------------------------
// phase4HebbianBoost — Hebbian association boosting
// ---------------------------------------------------------------------------

func TestPhase4HebbianBoost_NoRecentActivations(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	id1 := storage.NewULID()
	candidates := []fusedCandidate{{id: id1, rrfScore: 0.5}}

	e.phase4HebbianBoost(context.Background(), [8]byte{}, 1, candidates)

	if candidates[0].hebbianBoost != 0 {
		t.Errorf("hebbianBoost = %v, want 0 (no recent activations)", candidates[0].hebbianBoost)
	}
}

func TestPhase4HebbianBoost_WithAssociations(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	id1 := storage.NewULID()
	id2 := storage.NewULID()
	id3 := storage.NewULID()

	// id1 is associated with id2 (weight=0.8)
	store.assocs[id1] = []storage.Association{
		{TargetID: id2, Weight: 0.8, RelType: storage.RelSupports},
	}
	// id3 is associated with id2 (weight=0.6)
	store.assocs[id3] = []storage.Association{
		{TargetID: id2, Weight: 0.6, RelType: storage.RelRelatesTo},
	}

	// Record a recent activation that included id2
	e.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: []storage.ULID{id2},
		Scores:    []float64{0.9},
	})

	candidates := []fusedCandidate{
		{id: id1, rrfScore: 0.5},
		{id: id3, rrfScore: 0.4},
	}

	e.phase4HebbianBoost(context.Background(), [8]byte{}, 1, candidates)

	if candidates[0].hebbianBoost <= 0 {
		t.Errorf("candidate id1 should have positive hebbianBoost, got %v", candidates[0].hebbianBoost)
	}
	if candidates[1].hebbianBoost <= 0 {
		t.Errorf("candidate id3 should have positive hebbianBoost, got %v", candidates[1].hebbianBoost)
	}
	if candidates[0].hebbianBoost <= candidates[1].hebbianBoost {
		t.Errorf("id1 boost (%v) should be > id3 boost (%v) because weight 0.8 > 0.6",
			candidates[0].hebbianBoost, candidates[1].hebbianBoost)
	}
}

func TestPhase4HebbianBoost_BoostCappedAt1(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	id1 := storage.NewULID()
	targetIDs := make([]storage.ULID, 10)
	var assocs []storage.Association
	for i := range targetIDs {
		targetIDs[i] = storage.NewULID()
		assocs = append(assocs, storage.Association{
			TargetID: targetIDs[i], Weight: 1.0, RelType: storage.RelSupports,
		})
	}
	store.assocs[id1] = assocs

	e.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: targetIDs,
		Scores:    make([]float64, len(targetIDs)),
	})

	candidates := []fusedCandidate{{id: id1, rrfScore: 0.5}}
	e.phase4HebbianBoost(context.Background(), [8]byte{}, 1, candidates)

	if candidates[0].hebbianBoost > 1.0 {
		t.Errorf("hebbianBoost = %v, should be capped at 1.0", candidates[0].hebbianBoost)
	}
}

func TestPhase4HebbianBoost_OldActivationDecays(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	id1 := storage.NewULID()
	id2 := storage.NewULID()
	store.assocs[id1] = []storage.Association{
		{TargetID: id2, Weight: 0.8, RelType: storage.RelSupports},
	}

	// Record an old activation (2 hours ago → recencyW ≈ exp(-7200/3600) ≈ 0.135)
	e.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now().Add(-2 * time.Hour),
		EngramIDs: []storage.ULID{id2},
		Scores:    []float64{0.9},
	})

	candidates := []fusedCandidate{{id: id1, rrfScore: 0.5}}
	e.phase4HebbianBoost(context.Background(), [8]byte{}, 1, candidates)
	oldBoost := candidates[0].hebbianBoost

	// Record a fresh activation.
	e2 := newTestActivationEngine(store)
	defer e2.Close()
	e2.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: []storage.ULID{id2},
		Scores:    []float64{0.9},
	})

	candidates2 := []fusedCandidate{{id: id1, rrfScore: 0.5}}
	e2.phase4HebbianBoost(context.Background(), [8]byte{}, 1, candidates2)
	freshBoost := candidates2[0].hebbianBoost

	if freshBoost <= oldBoost {
		t.Errorf("fresh boost (%v) should exceed old boost (%v)", freshBoost, oldBoost)
	}
}

func TestPhase4HebbianBoost_MoreThan50CandidatesCapped(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	targetID := storage.NewULID()
	e.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: []storage.ULID{targetID},
		Scores:    []float64{0.9},
	})

	candidates := make([]fusedCandidate, 60)
	for i := range candidates {
		candidates[i].id = storage.NewULID()
		candidates[i].rrfScore = 0.5
		store.assocs[candidates[i].id] = []storage.Association{
			{TargetID: targetID, Weight: 0.5, RelType: storage.RelSupports},
		}
	}

	e.phase4HebbianBoost(context.Background(), [8]byte{}, 1, candidates)

	// First 50 should have boost, rest should not (ids capped to 50)
	for i := 0; i < 50; i++ {
		if candidates[i].hebbianBoost <= 0 {
			t.Errorf("candidate[%d] should have boost, got %v", i, candidates[i].hebbianBoost)
		}
	}
	for i := 50; i < 60; i++ {
		if candidates[i].hebbianBoost != 0 {
			t.Errorf("candidate[%d] should have 0 boost (beyond cap), got %v", i, candidates[i].hebbianBoost)
		}
	}
}

func TestPhase4HebbianBoost_NoAssociations(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	id1 := storage.NewULID()
	id2 := storage.NewULID()

	// Recent activation references id2 but id1 has no associations at all
	e.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: []storage.ULID{id2},
		Scores:    []float64{0.9},
	})

	candidates := []fusedCandidate{{id: id1, rrfScore: 0.5}}
	e.phase4HebbianBoost(context.Background(), [8]byte{}, 1, candidates)

	if candidates[0].hebbianBoost != 0 {
		t.Errorf("hebbianBoost should be 0 with no associations, got %v", candidates[0].hebbianBoost)
	}
}

// ---------------------------------------------------------------------------
// phase5Traverse — BFS traversal
// ---------------------------------------------------------------------------

func TestPhase5Traverse_EmptyCandidates(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	profile := GetProfile("default")
	result := e.phase5Traverse(context.Background(), &ActivateRequest{HopDepth: 2}, [8]byte{}, profile, nil)
	if result != nil {
		t.Errorf("expected nil for empty candidates, got %d items", len(result))
	}
}

func TestPhase5Traverse_SingleHopDiscovery(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	seedID := storage.NewULID()
	hopID := storage.NewULID()

	store.assocs[seedID] = []storage.Association{
		{TargetID: hopID, Weight: 0.9, RelType: storage.RelSupports},
	}

	candidates := []fusedCandidate{{id: seedID, rrfScore: 0.8}}
	profile := GetProfile("default")
	result := e.phase5Traverse(context.Background(), &ActivateRequest{HopDepth: 1}, [8]byte{}, profile, candidates)

	if len(result) != 1 {
		t.Fatalf("expected 1 traversed candidate, got %d", len(result))
	}
	if result[0].id != hopID {
		t.Errorf("discovered id = %v, want %v", result[0].id, hopID)
	}
	if result[0].propagated <= 0 {
		t.Errorf("propagated score should be > 0, got %v", result[0].propagated)
	}
	if len(result[0].hopPath) != 2 {
		t.Errorf("hop path should have 2 entries (seed + hop), got %d", len(result[0].hopPath))
	}
}

func TestPhase5Traverse_MultiHopDiscovery(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	seedID := storage.NewULID()
	hop1ID := storage.NewULID()
	hop2ID := storage.NewULID()

	store.assocs[seedID] = []storage.Association{
		{TargetID: hop1ID, Weight: 0.9, RelType: storage.RelSupports},
	}
	store.assocs[hop1ID] = []storage.Association{
		{TargetID: hop2ID, Weight: 0.8, RelType: storage.RelSupports},
	}

	candidates := []fusedCandidate{{id: seedID, rrfScore: 0.8}}
	profile := GetProfile("default")
	result := e.phase5Traverse(context.Background(), &ActivateRequest{HopDepth: 2}, [8]byte{}, profile, candidates)

	if len(result) < 2 {
		t.Fatalf("expected at least 2 traversed candidates (hop1 + hop2), got %d", len(result))
	}

	foundHop1, foundHop2 := false, false
	for _, tc := range result {
		if tc.id == hop1ID {
			foundHop1 = true
		}
		if tc.id == hop2ID {
			foundHop2 = true
		}
	}
	if !foundHop1 {
		t.Error("expected hop1 in traversed results")
	}
	if !foundHop2 {
		t.Error("expected hop2 in traversed results")
	}
}

func TestPhase5Traverse_SkipsSeedCandidates(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	seedID := storage.NewULID()
	otherSeedID := storage.NewULID()

	// seed has an association pointing back to otherSeed — BFS should skip it
	store.assocs[seedID] = []storage.Association{
		{TargetID: otherSeedID, Weight: 0.9, RelType: storage.RelSupports},
	}

	candidates := []fusedCandidate{
		{id: seedID, rrfScore: 0.8},
		{id: otherSeedID, rrfScore: 0.7},
	}
	profile := GetProfile("default")
	result := e.phase5Traverse(context.Background(), &ActivateRequest{HopDepth: 1}, [8]byte{}, profile, candidates)

	for _, tc := range result {
		if tc.id == otherSeedID {
			t.Error("BFS should not re-discover existing seed candidates")
		}
	}
}

func TestPhase5Traverse_ProfileFiltering(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	seedID := storage.NewULID()
	causalID := storage.NewULID()
	supportID := storage.NewULID()

	store.assocs[seedID] = []storage.Association{
		{TargetID: causalID, Weight: 0.9, RelType: storage.RelCauses},
		{TargetID: supportID, Weight: 0.9, RelType: storage.RelSupports},
	}

	candidates := []fusedCandidate{{id: seedID, rrfScore: 0.8}}
	causalProfile := GetProfile("causal")
	result := e.phase5Traverse(context.Background(), &ActivateRequest{HopDepth: 1}, [8]byte{}, causalProfile, candidates)

	foundCausal, foundSupport := false, false
	for _, tc := range result {
		if tc.id == causalID {
			foundCausal = true
		}
		if tc.id == supportID {
			foundSupport = true
		}
	}
	if !foundCausal {
		t.Error("causal profile should traverse RelCauses edges")
	}
	if foundSupport {
		t.Error("causal profile should NOT traverse RelSupports edges")
	}
}

func TestPhase5Traverse_LowWeightPruned(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	seedID := storage.NewULID()
	weakID := storage.NewULID()

	store.assocs[seedID] = []storage.Association{
		{TargetID: weakID, Weight: 0.001, RelType: storage.RelSupports},
	}

	candidates := []fusedCandidate{{id: seedID, rrfScore: 0.1}}
	profile := GetProfile("default")
	result := e.phase5Traverse(context.Background(), &ActivateRequest{HopDepth: 1}, [8]byte{}, profile, candidates)

	// propagated = 0.1 * 0.001 * 1.0 * 0.7^1 = 0.00007 < minHopScore (0.05)
	for _, tc := range result {
		if tc.id == weakID {
			t.Error("weak edges below minHopScore should be pruned")
		}
	}
}

func TestPhase5Traverse_NoAssociations(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	seedID := storage.NewULID()

	candidates := []fusedCandidate{{id: seedID, rrfScore: 0.8}}
	profile := GetProfile("default")
	result := e.phase5Traverse(context.Background(), &ActivateRequest{HopDepth: 2}, [8]byte{}, profile, candidates)

	if len(result) != 0 {
		t.Errorf("expected 0 traversed candidates when no associations exist, got %d", len(result))
	}
}

func TestPhase5Traverse_HopDepthZero(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	seedID := storage.NewULID()
	hopID := storage.NewULID()
	store.assocs[seedID] = []storage.Association{
		{TargetID: hopID, Weight: 0.9, RelType: storage.RelSupports},
	}

	candidates := []fusedCandidate{{id: seedID, rrfScore: 0.8}}
	profile := GetProfile("default")
	result := e.phase5Traverse(context.Background(), &ActivateRequest{HopDepth: 0}, [8]byte{}, profile, candidates)

	if len(result) != 0 {
		t.Errorf("HopDepth=0 should produce no traversal, got %d items", len(result))
	}
}

func TestPhase5Traverse_PropagatedScoreDecays(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	seedID := storage.NewULID()
	hop1ID := storage.NewULID()
	hop2ID := storage.NewULID()

	store.assocs[seedID] = []storage.Association{
		{TargetID: hop1ID, Weight: 1.0, RelType: storage.RelSupports},
	}
	store.assocs[hop1ID] = []storage.Association{
		{TargetID: hop2ID, Weight: 1.0, RelType: storage.RelSupports},
	}

	candidates := []fusedCandidate{{id: seedID, rrfScore: 1.0}}
	profile := GetProfile("default")
	result := e.phase5Traverse(context.Background(), &ActivateRequest{HopDepth: 2}, [8]byte{}, profile, candidates)

	var hop1Score, hop2Score float64
	for _, tc := range result {
		if tc.id == hop1ID {
			hop1Score = tc.propagated
		}
		if tc.id == hop2ID {
			hop2Score = tc.propagated
		}
	}

	if hop1Score <= 0 {
		t.Fatal("hop1 should have a positive propagated score")
	}
	if hop2Score >= hop1Score {
		t.Errorf("hop2 (%v) should have lower propagated score than hop1 (%v) due to hop penalty",
			hop2Score, hop1Score)
	}
}

// ---------------------------------------------------------------------------
// getTransitionCandidates — PAS candidate retrieval
// ---------------------------------------------------------------------------

func TestGetTransitionCandidates_NoTransitionStore(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	// No transStore set, no recent activations — should return nil
	result := e.getTransitionCandidates(context.Background(), [8]byte{}, 1, 5)
	if result != nil {
		t.Errorf("expected nil when no recent activations, got %v", result)
	}
}

func TestGetTransitionCandidates_WithTransitions(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	srcID := storage.NewULID()
	targetID1 := storage.NewULID()
	targetID2 := storage.NewULID()

	ts := &stubTransitionStore{
		transitions: map[storage.ULID][]storage.TransitionTarget{
			srcID: {
				{ID: [16]byte(targetID1), Count: 5},
				{ID: [16]byte(targetID2), Count: 3},
			},
		},
	}
	e.transStore = ts

	e.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: []storage.ULID{srcID},
		Scores:    []float64{0.9},
	})

	result := e.getTransitionCandidates(context.Background(), [8]byte{}, 1, 5)

	if len(result) != 2 {
		t.Fatalf("expected 2 transition candidates, got %d", len(result))
	}
}

func TestGetTransitionCandidates_DefaultMaxInjections(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	srcID := storage.NewULID()
	var targets []storage.TransitionTarget
	for i := 0; i < 10; i++ {
		targets = append(targets, storage.TransitionTarget{
			ID: [16]byte(storage.NewULID()), Count: uint32(10 - i),
		})
	}

	ts := &stubTransitionStore{
		transitions: map[storage.ULID][]storage.TransitionTarget{srcID: targets},
	}
	e.transStore = ts

	e.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: []storage.ULID{srcID},
		Scores:    []float64{0.9},
	})

	// maxInjections=0 defaults to 5
	result := e.getTransitionCandidates(context.Background(), [8]byte{}, 1, 0)
	if len(result) > 5 {
		t.Errorf("expected at most 5 candidates with default maxInjections, got %d", len(result))
	}
}

func TestGetTransitionCandidates_Deduplication(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	srcID1 := storage.NewULID()
	srcID2 := storage.NewULID()
	sharedTarget := storage.NewULID()

	ts := &stubTransitionStore{
		transitions: map[storage.ULID][]storage.TransitionTarget{
			srcID1: {{ID: [16]byte(sharedTarget), Count: 5}},
			srcID2: {{ID: [16]byte(sharedTarget), Count: 3}},
		},
	}
	e.transStore = ts

	e.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: []storage.ULID{srcID1, srcID2},
		Scores:    []float64{0.9, 0.8},
	})

	result := e.getTransitionCandidates(context.Background(), [8]byte{}, 1, 10)

	count := 0
	for _, id := range result {
		if id == sharedTarget {
			count++
		}
	}
	if count > 1 {
		t.Errorf("shared target should appear at most once, appeared %d times", count)
	}
}

// ---------------------------------------------------------------------------
// phase4_5TransitionBoost — PAS transition scoring
// ---------------------------------------------------------------------------

func TestPhase4_5TransitionBoost_NilTransStore(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	candidates := []fusedCandidate{{id: storage.NewULID(), rrfScore: 0.5}}
	e.phase4_5TransitionBoost(context.Background(), [8]byte{}, 1, candidates)

	if candidates[0].transitionBoost != 0 {
		t.Errorf("transitionBoost should be 0 without transStore, got %v", candidates[0].transitionBoost)
	}
}

func TestPhase4_5TransitionBoost_NoRecentActivations(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	e.transStore = &stubTransitionStore{}

	candidates := []fusedCandidate{{id: storage.NewULID(), rrfScore: 0.5}}
	e.phase4_5TransitionBoost(context.Background(), [8]byte{}, 1, candidates)

	if candidates[0].transitionBoost != 0 {
		t.Errorf("transitionBoost should be 0 without recent activations, got %v", candidates[0].transitionBoost)
	}
}

func TestPhase4_5TransitionBoost_AppliesBoost(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	srcID := storage.NewULID()
	candidateID := storage.NewULID()
	otherID := storage.NewULID()

	ts := &stubTransitionStore{
		transitions: map[storage.ULID][]storage.TransitionTarget{
			srcID: {
				{ID: [16]byte(candidateID), Count: 10},
				{ID: [16]byte(otherID), Count: 5},
			},
		},
	}
	e.transStore = ts

	e.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: []storage.ULID{srcID},
		Scores:    []float64{0.9},
	})

	candidates := []fusedCandidate{
		{id: candidateID, rrfScore: 0.5},
		{id: otherID, rrfScore: 0.4},
		{id: storage.NewULID(), rrfScore: 0.3}, // not a transition target
	}

	e.phase4_5TransitionBoost(context.Background(), [8]byte{}, 1, candidates)

	// candidateID has count=10 (max), so boost = 10/10 = 1.0
	if candidates[0].transitionBoost < 0.99 || candidates[0].transitionBoost > 1.01 {
		t.Errorf("candidateID boost = %v, want ~1.0 (max count)", candidates[0].transitionBoost)
	}
	// otherID has count=5, so boost = 5/10 = 0.5
	if candidates[1].transitionBoost < 0.49 || candidates[1].transitionBoost > 0.51 {
		t.Errorf("otherID boost = %v, want ~0.5", candidates[1].transitionBoost)
	}
	// Non-transition candidate should have 0 boost
	if candidates[2].transitionBoost != 0 {
		t.Errorf("non-target boost = %v, want 0", candidates[2].transitionBoost)
	}
}

func TestPhase4_5TransitionBoost_EmptyTransitions(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	srcID := storage.NewULID()
	ts := &stubTransitionStore{
		transitions: map[storage.ULID][]storage.TransitionTarget{},
	}
	e.transStore = ts

	e.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: []storage.ULID{srcID},
		Scores:    []float64{0.9},
	})

	candidates := []fusedCandidate{{id: storage.NewULID(), rrfScore: 0.5}}
	e.phase4_5TransitionBoost(context.Background(), [8]byte{}, 1, candidates)

	if candidates[0].transitionBoost != 0 {
		t.Errorf("boost should be 0 with empty transition results, got %v", candidates[0].transitionBoost)
	}
}

// ---------------------------------------------------------------------------
// phase6Score — final scoring (ACT-R path + standard path + structured filter)
// ---------------------------------------------------------------------------

func TestPhase6Score_ACTRPath(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	eng1 := &storage.Engram{
		Concept: "test engram", Content: "content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateActive,
	}
	store.addEngram(eng1)

	fused := []fusedCandidate{{id: eng1.ID, rrfScore: 0.5, ftsScore: 1.0, vectorScore: 0.8}}
	p1 := &phase1Result{queryStr: "test"}

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: 0.0,
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}
	if len(result.Activations) == 0 {
		t.Fatal("expected at least 1 activation")
	}
	if result.Activations[0].Score <= 0 {
		t.Errorf("score should be positive, got %v", result.Activations[0].Score)
	}
}

func TestPhase6Score_FiltersOutSoftDeleted(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	active := &storage.Engram{
		Concept: "active", Content: "active content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateActive,
	}
	deleted := &storage.Engram{
		Concept: "deleted", Content: "deleted content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateSoftDeleted,
	}
	store.addEngram(active)
	store.addEngram(deleted)

	fused := []fusedCandidate{
		{id: active.ID, rrfScore: 0.5, ftsScore: 1.0},
		{id: deleted.ID, rrfScore: 0.5, ftsScore: 1.0},
	}
	p1 := &phase1Result{queryStr: "test"}

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: 0.0,
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}
	for _, a := range result.Activations {
		if a.Engram.State == storage.StateSoftDeleted {
			t.Error("soft-deleted engram should be filtered out")
		}
	}
}

func TestPhase6Score_WithTraversedCandidates(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	eng1 := &storage.Engram{
		Concept: "seed", Content: "seed content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateActive,
	}
	eng2 := &storage.Engram{
		Concept: "discovered", Content: "discovered via BFS",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.7,
		State: storage.StateActive,
	}
	store.addEngram(eng1)
	store.addEngram(eng2)

	fused := []fusedCandidate{{id: eng1.ID, rrfScore: 0.5, ftsScore: 1.0}}
	traversed := []traversedCandidate{{
		id: eng2.ID, propagated: 0.3,
		hopPath: []storage.ULID{eng1.ID, eng2.ID},
		relType: uint16(storage.RelSupports),
	}}
	p1 := &phase1Result{queryStr: "test"}

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: 0.0,
	}, [8]byte{}, fused, traversed, p1)

	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}

	foundTraversed := false
	for _, a := range result.Activations {
		if a.Engram.ID == eng2.ID {
			foundTraversed = true
		}
	}
	if !foundTraversed {
		t.Error("traversed candidate should appear in final results")
	}
}

func TestPhase6Score_IncludeWhy(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	eng1 := &storage.Engram{
		Concept: "why test", Content: "content for why generation",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateActive,
	}
	store.addEngram(eng1)

	fused := []fusedCandidate{{id: eng1.ID, rrfScore: 0.5, ftsScore: 2.0, vectorScore: 0.8}}
	p1 := &phase1Result{queryStr: "why test query"}

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: 0.0, IncludeWhy: true,
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}
	if len(result.Activations) == 0 {
		t.Fatal("expected activations")
	}
	if result.Activations[0].Why == "" {
		t.Error("IncludeWhy=true should produce a non-empty Why string")
	}
}

func TestPhase6Score_MaxResultsTruncation(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	var fused []fusedCandidate
	for i := 0; i < 20; i++ {
		eng := &storage.Engram{
			Concept: "test", Content: "content",
			Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
			State: storage.StateActive,
		}
		store.addEngram(eng)
		fused = append(fused, fusedCandidate{id: eng.ID, rrfScore: float64(20-i) / 20.0, ftsScore: 1.0})
	}

	p1 := &phase1Result{queryStr: "test"}
	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 5, Threshold: 0.0,
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}
	if len(result.Activations) > 5 {
		t.Errorf("expected at most 5 results, got %d", len(result.Activations))
	}
	if result.TotalFound <= 5 {
		t.Errorf("TotalFound should reflect pre-truncation count, got %d", result.TotalFound)
	}
}

func TestPhase6Score_ThresholdFilters(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	eng1 := &storage.Engram{
		Concept: "low score", Content: "content",
		Confidence: 0.01, Stability: 30.0, Relevance: 0.1,
		State: storage.StateActive,
	}
	store.addEngram(eng1)

	fused := []fusedCandidate{{id: eng1.ID, rrfScore: 0.1, ftsScore: 0.01}}
	p1 := &phase1Result{queryStr: "test"}

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: 0.99,
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}
	for _, a := range result.Activations {
		if a.Score < 0.99 {
			t.Errorf("activation below threshold: score=%v", a.Score)
		}
	}
}

func TestPhase6Score_DormantFlag(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	eng1 := &storage.Engram{
		Concept: "dormant", Content: "dormant content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.01,
		State: storage.StateActive,
	}
	store.addEngram(eng1)

	fused := []fusedCandidate{{id: eng1.ID, rrfScore: 0.5, ftsScore: 1.0}}
	p1 := &phase1Result{queryStr: "test"}

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: 0.0,
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}
	for _, a := range result.Activations {
		if a.Engram.ID == eng1.ID && !a.Dormant {
			t.Error("engram with low Relevance should have Dormant=true")
		}
	}
}

func TestPhase6Score_CGDNPath(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	eng1 := &storage.Engram{
		Concept: "cgdn test", Content: "cgdn content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateActive,
	}
	store.addEngram(eng1)

	fused := []fusedCandidate{{id: eng1.ID, rrfScore: 0.5, ftsScore: 1.0, vectorScore: 0.8}}
	p1 := &phase1Result{queryStr: "test"}

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: 0.0,
		Weights: &Weights{
			UseCGDN:            true,
			SemanticSimilarity: 0.5,
			FullTextRelevance:  0.3,
		},
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score with CGDN: %v", err)
	}
	if len(result.Activations) == 0 {
		t.Fatal("expected at least 1 activation from CGDN path")
	}
	if result.Activations[0].Score <= 0 {
		t.Errorf("CGDN score should be positive, got %v", result.Activations[0].Score)
	}
}

func TestPhase6Score_CachedLastAccess(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	eng1 := &storage.Engram{
		Concept: "cached", Content: "content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateActive,
		LastAccess: time.Now().Add(-48 * time.Hour),
	}
	store.addEngram(eng1)
	store.lastAccessNs[eng1.ID] = time.Now().UnixNano()

	fused := []fusedCandidate{{id: eng1.ID, rrfScore: 0.5, ftsScore: 1.0}}
	p1 := &phase1Result{queryStr: "test"}

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: 0.0,
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}
	if len(result.Activations) == 0 {
		t.Fatal("expected at least 1 activation")
	}
}

// ---------------------------------------------------------------------------
// computeComponents — additional edge cases
// ---------------------------------------------------------------------------

func TestComputeComponents_ZeroStability(t *testing.T) {
	eng := &storage.Engram{
		Confidence: 1.0, Stability: 0.0, AccessCount: 5,
		CreatedAt: time.Now().Add(-24 * time.Hour),
		LastAccess: time.Now().Add(-1 * time.Hour),
	}
	w := resolvedWeights{DecayFactor: 1.0}
	c := computeComponents(0, 0, 0, eng, 0, time.Now(), w)
	if c.DecayFactor < 0 || c.DecayFactor > 1 {
		t.Errorf("DecayFactor out of range with Stability=0: %v", c.DecayFactor)
	}
}

func TestComputeComponents_VeryHighAccessCount(t *testing.T) {
	eng := &storage.Engram{
		Confidence: 1.0, Stability: 30.0, AccessCount: 10000,
		CreatedAt: time.Now(), LastAccess: time.Now(),
	}
	w := resolvedWeights{AccessFrequency: 1.0}
	c := computeComponents(0, 0, 0, eng, 0, time.Now(), w)

	if c.AccessFrequency > 1.0 {
		t.Errorf("AccessFrequency should be capped at 1.0, got %v", c.AccessFrequency)
	}
	if c.AccessFrequency < 0.9 {
		t.Errorf("AccessFrequency should be near 1.0 for 10000 accesses, got %v", c.AccessFrequency)
	}
}

func TestComputeComponents_AllWeightsZero(t *testing.T) {
	eng := &storage.Engram{
		Confidence: 1.0, Stability: 30.0, AccessCount: 5,
		CreatedAt: time.Now().Add(-24 * time.Hour),
		LastAccess: time.Now().Add(-1 * time.Hour),
	}
	w := resolvedWeights{}
	c := computeComponents(0.8, 1.0, 0.5, eng, 0, time.Now(), w)

	if c.Raw != 0 {
		t.Errorf("Raw should be 0 when all weights are 0, got %v", c.Raw)
	}
}

func TestComputeComponents_RawClamping(t *testing.T) {
	eng := &storage.Engram{
		Confidence: 1.0, Stability: 30.0, AccessCount: 5,
		CreatedAt: time.Now(), LastAccess: time.Now(),
	}
	w := resolvedWeights{
		SemanticSimilarity: 1.0, FullTextRelevance: 1.0,
		DecayFactor: 1.0, HebbianBoost: 1.0,
		AccessFrequency: 1.0, Recency: 1.0,
	}
	c := computeComponents(1.0, 10.0, 1.0, eng, 0, time.Now(), w)

	if c.Raw > 1.0 {
		t.Errorf("Raw should be clamped to 1.0, got %v", c.Raw)
	}
}

func TestComputeComponents_FTSNormalization(t *testing.T) {
	eng := &storage.Engram{
		Confidence: 1.0, Stability: 30.0,
		CreatedAt: time.Now(), LastAccess: time.Now(),
	}
	w := resolvedWeights{FullTextRelevance: 1.0}

	c1 := computeComponents(0, 0.5, 0, eng, 0, time.Now(), w)
	c2 := computeComponents(0, 5.0, 0, eng, 0, time.Now(), w)
	c3 := computeComponents(0, 50.0, 0, eng, 0, time.Now(), w)

	if c1.FullTextRelevance >= c2.FullTextRelevance {
		t.Errorf("higher FTS score should yield higher normalized value: 0.5→%v, 5.0→%v",
			c1.FullTextRelevance, c2.FullTextRelevance)
	}
	// tanh saturates, so 5.0 and 50.0 should be very close
	if c3.FullTextRelevance < 0.99 {
		t.Errorf("very high FTS should saturate near 1.0, got %v", c3.FullTextRelevance)
	}
}

// ---------------------------------------------------------------------------
// phase2 — branch coverage (fast path vs parallel path)
// ---------------------------------------------------------------------------

type nilFTS struct{}

func (f *nilFTS) Search(_ context.Context, _ [8]byte, _ string, _ int) ([]ScoredID, error) {
	return nil, nil
}

type simpleHNSW struct {
	results []ScoredID
}

func (h *simpleHNSW) Search(_ context.Context, _ [8]byte, _ []float32, topK int) ([]ScoredID, error) {
	if topK > len(h.results) {
		topK = len(h.results)
	}
	return h.results[:topK], nil
}

func TestPhase2_FastPath_NilHNSW(t *testing.T) {
	store := newInternalStubStore()
	eng1 := &storage.Engram{Concept: "test", Content: "content"}
	store.addEngram(eng1)

	e := &ActivationEngine{
		store:    store,
		fts:      &nilFTS{},
		hnsw:     nil,
		assocLog: &ActivationLog{},
		logCh:    make(chan logItem, 64),
		logDone:  make(chan struct{}),
	}
	go e.drainLog()
	defer e.Close()

	req := &ActivateRequest{CandidatesPerIndex: 10}
	p1 := &phase1Result{queryStr: "test", embedding: []float32{0.1}}

	sets, err := e.phase2(context.Background(), req, p1, [8]byte{})
	if err != nil {
		t.Fatalf("phase2: %v", err)
	}
	if sets == nil {
		t.Fatal("expected non-nil sets")
	}
}

func TestPhase2_FastPath_EmptyEmbedding(t *testing.T) {
	store := newInternalStubStore()
	eng1 := &storage.Engram{Concept: "test", Content: "content"}
	store.addEngram(eng1)

	e := &ActivationEngine{
		store:    store,
		fts:      &nilFTS{},
		hnsw:     &simpleHNSW{},
		assocLog: &ActivationLog{},
		logCh:    make(chan logItem, 64),
		logDone:  make(chan struct{}),
	}
	go e.drainLog()
	defer e.Close()

	req := &ActivateRequest{CandidatesPerIndex: 10}
	p1 := &phase1Result{queryStr: "test", embedding: nil}

	sets, err := e.phase2(context.Background(), req, p1, [8]byte{})
	if err != nil {
		t.Fatalf("phase2: %v", err)
	}
	if sets == nil {
		t.Fatal("expected non-nil sets")
	}
}

func TestPhase2_FastPath_WithTimeBounds(t *testing.T) {
	store := newInternalStubStore()
	eng1 := &storage.Engram{Concept: "test", Content: "content"}
	store.addEngram(eng1)

	e := &ActivationEngine{
		store:    store,
		fts:      &nilFTS{},
		hnsw:     nil,
		assocLog: &ActivationLog{},
		logCh:    make(chan logItem, 64),
		logDone:  make(chan struct{}),
	}
	go e.drainLog()
	defer e.Close()

	req := &ActivateRequest{
		CandidatesPerIndex: 10,
		Filters: []Filter{
			{Field: "created_after", Op: "gt", Value: time.Now().Add(-24 * time.Hour)},
		},
	}
	p1 := &phase1Result{queryStr: "test"}

	sets, err := e.phase2(context.Background(), req, p1, [8]byte{})
	if err != nil {
		t.Fatalf("phase2: %v", err)
	}
	if sets == nil {
		t.Fatal("expected non-nil sets from time-bounded path")
	}
}

func TestPhase2_FastPath_WithPAS(t *testing.T) {
	store := newInternalStubStore()
	eng1 := &storage.Engram{Concept: "test", Content: "content"}
	store.addEngram(eng1)

	srcID := storage.NewULID()
	targetID := storage.NewULID()
	ts := &stubTransitionStore{
		transitions: map[storage.ULID][]storage.TransitionTarget{
			srcID: {{ID: [16]byte(targetID), Count: 5}},
		},
	}

	e := &ActivationEngine{
		store:      store,
		fts:        &nilFTS{},
		hnsw:       nil,
		assocLog:   &ActivationLog{},
		transStore: ts,
		logCh:      make(chan logItem, 64),
		logDone:    make(chan struct{}),
	}
	go e.drainLog()
	defer e.Close()

	e.assocLog.Record(LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: []storage.ULID{srcID},
		Scores:    []float64{0.9},
	})

	req := &ActivateRequest{
		VaultID:            1,
		CandidatesPerIndex: 10,
		PASEnabled:         true,
		PASMaxInjections:   3,
	}
	p1 := &phase1Result{queryStr: "test"}

	sets, err := e.phase2(context.Background(), req, p1, [8]byte{})
	if err != nil {
		t.Fatalf("phase2 with PAS: %v", err)
	}
	if sets == nil {
		t.Fatal("expected non-nil sets")
	}
	if len(sets.transition) == 0 {
		t.Error("expected transition candidates from PAS")
	}
}

func TestPhase2_ParallelPath(t *testing.T) {
	store := newInternalStubStore()
	id1 := storage.NewULID()
	eng1 := &storage.Engram{ID: id1, Concept: "test", Content: "content"}
	store.addEngram(eng1)

	e := &ActivationEngine{
		store:    store,
		fts:      &nilFTS{},
		hnsw:     &simpleHNSW{results: []ScoredID{{ID: id1, Score: 0.8}}},
		assocLog: &ActivationLog{},
		logCh:    make(chan logItem, 64),
		logDone:  make(chan struct{}),
	}
	go e.drainLog()
	defer e.Close()

	req := &ActivateRequest{CandidatesPerIndex: 10}
	p1 := &phase1Result{queryStr: "test", embedding: []float32{0.1, 0.2}}

	sets, err := e.phase2(context.Background(), req, p1, [8]byte{})
	if err != nil {
		t.Fatalf("phase2 parallel path: %v", err)
	}
	if sets == nil {
		t.Fatal("expected non-nil sets")
	}
	if len(sets.vector) == 0 {
		t.Error("expected vector results from parallel path")
	}
}

func TestPhase2_ParallelPath_WithTimeBounds(t *testing.T) {
	store := newInternalStubStore()
	id1 := storage.NewULID()
	eng1 := &storage.Engram{ID: id1, Concept: "test", Content: "content"}
	store.addEngram(eng1)

	e := &ActivationEngine{
		store:    store,
		fts:      &nilFTS{},
		hnsw:     &simpleHNSW{results: []ScoredID{{ID: id1, Score: 0.8}}},
		assocLog: &ActivationLog{},
		logCh:    make(chan logItem, 64),
		logDone:  make(chan struct{}),
	}
	go e.drainLog()
	defer e.Close()

	req := &ActivateRequest{
		CandidatesPerIndex: 10,
		Filters: []Filter{
			{Field: "created_after", Op: "gt", Value: time.Now().Add(-24 * time.Hour)},
			{Field: "created_before", Op: "lt", Value: time.Now().Add(24 * time.Hour)},
		},
	}
	p1 := &phase1Result{queryStr: "test", embedding: []float32{0.1, 0.2}}

	sets, err := e.phase2(context.Background(), req, p1, [8]byte{})
	if err != nil {
		t.Fatalf("phase2 with time bounds: %v", err)
	}
	if sets == nil {
		t.Fatal("expected non-nil sets")
	}
}

func TestPhase2_DefaultCandidatesPerIndex(t *testing.T) {
	store := newInternalStubStore()
	e := &ActivationEngine{
		store:    store,
		fts:      &nilFTS{},
		hnsw:     nil,
		assocLog: &ActivationLog{},
		logCh:    make(chan logItem, 64),
		logDone:  make(chan struct{}),
	}
	go e.drainLog()
	defer e.Close()

	req := &ActivateRequest{CandidatesPerIndex: 0}
	p1 := &phase1Result{queryStr: "test"}

	sets, err := e.phase2(context.Background(), req, p1, [8]byte{})
	if err != nil {
		t.Fatalf("phase2: %v", err)
	}
	if sets == nil {
		t.Fatal("expected non-nil sets")
	}
}

// ---------------------------------------------------------------------------
// phase6Score — structured filter
// ---------------------------------------------------------------------------

type testStructuredFilter struct {
	allowIDs map[storage.ULID]bool
}

func (f *testStructuredFilter) Match(eng *storage.Engram) bool {
	return f.allowIDs[eng.ID]
}

func TestPhase6Score_StructuredFilter(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	eng1 := &storage.Engram{
		Concept: "allowed", Content: "content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateActive,
	}
	eng2 := &storage.Engram{
		Concept: "filtered", Content: "content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateActive,
	}
	store.addEngram(eng1)
	store.addEngram(eng2)

	fused := []fusedCandidate{
		{id: eng1.ID, rrfScore: 0.5, ftsScore: 1.0},
		{id: eng2.ID, rrfScore: 0.5, ftsScore: 1.0},
	}
	p1 := &phase1Result{queryStr: "test"}

	sf := &testStructuredFilter{allowIDs: map[storage.ULID]bool{eng1.ID: true}}
	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults:       10,
		Threshold:        0.0,
		StructuredFilter: sf,
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}
	for _, a := range result.Activations {
		if a.Engram.ID == eng2.ID {
			t.Error("structured filter should have excluded eng2")
		}
	}
}

// ---------------------------------------------------------------------------
// phase6Score — standard (non-ACT-R, non-CGDN) path for completeness
// ---------------------------------------------------------------------------

func TestPhase6Score_StandardPath(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	eng1 := &storage.Engram{
		Concept: "standard", Content: "content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateActive,
	}
	store.addEngram(eng1)

	fused := []fusedCandidate{{id: eng1.ID, rrfScore: 0.5, ftsScore: 1.0, vectorScore: 0.8}}
	p1 := &phase1Result{queryStr: "test"}

	// The standard path is unreachable in production because UseACTR is always true.
	// But resolveWeights always sets UseACTR=true, so this will go through ACT-R.
	// We just verify it doesn't error.
	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: 0.0,
		Weights: &Weights{
			SemanticSimilarity: 0.4,
			FullTextRelevance:  0.3,
			DecayFactor:        0.1,
			HebbianBoost:       0.1,
			AccessFrequency:    0.05,
			Recency:            0.05,
		},
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score standard: %v", err)
	}
	if len(result.Activations) == 0 {
		t.Fatal("expected at least 1 activation")
	}
}

// ---------------------------------------------------------------------------
// phase6Score — meta filter coverage
// ---------------------------------------------------------------------------

func TestPhase6Score_MetaFilterExcludes(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	eng1 := &storage.Engram{
		Concept: "active", Content: "content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateActive,
	}
	eng2 := &storage.Engram{
		Concept: "planning", Content: "content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StatePlanning,
	}
	store.addEngram(eng1)
	store.addEngram(eng2)

	fused := []fusedCandidate{
		{id: eng1.ID, rrfScore: 0.5, ftsScore: 1.0},
		{id: eng2.ID, rrfScore: 0.5, ftsScore: 1.0},
	}
	p1 := &phase1Result{queryStr: "test"}

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: 0.0,
		Filters: []Filter{
			{Field: "state", Op: "eq", Value: storage.StateActive},
		},
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}
	for _, a := range result.Activations {
		if a.Engram.State != storage.StateActive {
			t.Errorf("meta filter should only allow StateActive, got %v", a.Engram.State)
		}
	}
}
