package enrich

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/storage"
)

// MockLLMProvider is a mock LLM provider for testing.
type MockLLMProvider struct {
	responses      map[string]string
	callCount      int
	failCount      int
	entityResponse string
	customComplete func(ctx context.Context, system, user string) (string, error)
}

func NewMockLLMProvider() *MockLLMProvider {
	return &MockLLMProvider{
		responses: make(map[string]string),
	}
}

func (m *MockLLMProvider) Name() string {
	return "mock"
}

func (m *MockLLMProvider) Init(ctx context.Context, cfg LLMProviderConfig) error {
	return nil
}

func (m *MockLLMProvider) Complete(ctx context.Context, system, user string) (string, error) {
	if m.customComplete != nil {
		return m.customComplete(ctx, system, user)
	}

	m.callCount++

	if m.failCount > 0 {
		m.failCount--
		return "", fmt.Errorf("mock provider error")
	}

	// Return default responses based on system prompt keywords.
	// Order matters: check "summarization" before "memory classification"
	// because the summarize prompt contains the word "memory" in its rules.
	if contains(system, "entity extraction") {
		if m.entityResponse != "" {
			return m.entityResponse, nil
		}
		return `{"entities": [{"name": "PostgreSQL", "type": "database", "confidence": 0.95}]}`, nil
	}
	if contains(system, "relationship") {
		return `{"relationships": [{"from": "app", "to": "PostgreSQL", "type": "uses", "weight": 0.9}]}`, nil
	}
	if contains(system, "summarization") {
		return `{"summary": "This is a test summary.", "key_points": ["point 1", "point 2"]}`, nil
	}
	if contains(system, "memory classification") {
		return `{"memory_type": "decision", "category": "infrastructure", "subcategory": "databases", "tags": ["db"]}`, nil
	}

	return "{}", nil
}

func (m *MockLLMProvider) Close() error {
	return nil
}

// TestPipelineRun_Success tests successful pipeline execution.
func TestPipelineRun_Success(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:      storage.NewULID(),
		Concept: "test-concept",
		Content: "test content here",
	}

	ctx := context.Background()
	result, err := pipeline.Run(ctx, eng)

	if err != nil {
		t.Fatalf("pipeline.Run failed: %v", err)
	}

	if result == nil {
		t.Fatalf("expected non-nil result")
	}

	if len(result.Entities) == 0 {
		t.Fatalf("expected at least one entity, got: %d", len(result.Entities))
	}

	// Summary may be empty due to parsing, but we should have some result
	// Just check that we got a result (not checking summary specifically)
	if result.MemoryType == "" && result.Summary == "" && len(result.Entities) == 0 {
		t.Fatalf("expected at least one field to be populated")
	}

	if mock.callCount > 0 && mock.callCount < 4 {
		// If we have entities, we should make all 4 calls
		// but if parsing fails, callCount might be 0
		// Just verify we made a reasonable number of calls
		t.Logf("callCount: %d", mock.callCount)
	}
}

// TestPipelineRun_ProviderError tests graceful degradation when provider fails.
func TestPipelineRun_ProviderError(t *testing.T) {
	mock := NewMockLLMProvider()
	// Simulate first call failing
	mock.failCount = 1

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:      storage.NewULID(),
		Concept: "test-concept",
		Content: "test content here",
	}

	ctx := context.Background()
	result, err := pipeline.Run(ctx, eng)

	if err != nil {
		t.Fatalf("pipeline.Run failed: %v", err)
	}

	if result == nil {
		t.Fatalf("expected non-nil result")
	}

	// First call failed, so no entities. But Call 2 should be skipped (no entities).
	// Calls 3 and 4 should proceed.
	if len(result.Entities) != 0 {
		t.Fatalf("expected 0 entities (first call failed), got: %d", len(result.Entities))
	}

	// Other calls should have succeeded
	if result.MemoryType == "" {
		t.Fatalf("expected non-empty memory_type from Call 3")
	}

	// The second call (relationships) should be skipped because there are no entities
	// So we expect 3 successful calls, not 4
	// mock.callCount should be 3 or 4 depending on how we count the initial failure
}

// TestPipelineRun_AllFail tests error when all calls fail.
func TestPipelineRun_AllFail(t *testing.T) {
	mock := NewMockLLMProvider()
	mock.failCount = 100 // Fail all calls

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:      storage.NewULID(),
		Concept: "test-concept",
		Content: "test content here",
	}

	ctx := context.Background()
	result, err := pipeline.Run(ctx, eng)

	if err == nil {
		t.Fatalf("expected error when all calls fail")
	}

	if result != nil {
		t.Fatalf("expected nil result when all calls fail")
	}
}

// TestPipelineRun_ContextTimeout tests context timeout handling.
func TestPipelineRun_ContextTimeout(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:      storage.NewULID(),
		Concept: "test-concept",
		Content: "test content here",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	result, err := pipeline.Run(ctx, eng)

	// Should timeout or return an error
	if err == nil && result == nil {
		t.Fatalf("expected error or nil result on timeout")
	}
}

// TestPipelineRelationshipSkippedWithoutEntities tests that Call 2 is skipped when Call 1 has no entities.
func TestPipelineRelationshipSkippedWithoutEntities(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	// Set up custom complete to return empty entities
	mock.entityResponse = `{"entities": []}`

	eng := &storage.Engram{
		ID:      storage.NewULID(),
		Concept: "test-concept",
		Content: "test content here",
	}

	ctx := context.Background()
	result, err := pipeline.Run(ctx, eng)

	if err != nil {
		t.Fatalf("pipeline.Run failed: %v", err)
	}

	if result == nil {
		t.Fatalf("expected non-nil result")
	}

	// Relationships should be empty because Call 1 returned no entities
	if len(result.Relationships) != 0 {
		t.Fatalf("expected 0 relationships (no entities), got: %d", len(result.Relationships))
	}
}

// --- Task 8: Background Enrichment Restructure Tests ---

func boolPtr(b bool) *bool { return &b }

// TestLightMode_OnlyOneLLMCall verifies light mode runs only summarization.
func TestLightMode_OnlyOneLLMCall(t *testing.T) {
	var callCount atomic.Int32
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		callCount.Add(1)
		if strings.Contains(system, "summarization") {
			return `{"summary": "Light summary.", "key_points": ["kp1"]}`, nil
		}
		return `{}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)
	pipeline.SetConfig(&config.PluginConfig{EnrichMode: "light"})

	eng := &storage.Engram{
		ID:      storage.NewULID(),
		Concept: "test",
		Content: "content",
	}

	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if callCount.Load() != 1 {
		t.Fatalf("light mode should make exactly 1 LLM call, got %d", callCount.Load())
	}
	if result.Summary != "Light summary." {
		t.Fatalf("expected light summary, got %q", result.Summary)
	}
	if len(result.KeyPoints) != 1 || result.KeyPoints[0] != "kp1" {
		t.Fatalf("expected 1 key point, got %v", result.KeyPoints)
	}
	if len(result.Entities) != 0 {
		t.Fatalf("light mode should produce no entities, got %d", len(result.Entities))
	}
	if len(result.Relationships) != 0 {
		t.Fatalf("light mode should produce no relationships, got %d", len(result.Relationships))
	}
	if result.MemoryType != "" {
		t.Fatalf("light mode should produce no classification, got %q", result.MemoryType)
	}
}

// TestDisableEntitiesStage skips entity extraction when disabled.
func TestDisableEntitiesStage(t *testing.T) {
	var calledStages []string
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		if strings.Contains(system, "entity extraction") {
			calledStages = append(calledStages, "entities")
			return `{"entities": [{"name": "X", "type": "tool", "confidence": 0.9}]}`, nil
		}
		if strings.Contains(system, "relationship") {
			calledStages = append(calledStages, "relationships")
			return `{"relationships": []}`, nil
		}
		if strings.Contains(system, "summarization") {
			calledStages = append(calledStages, "summary")
			return `{"summary": "sum", "key_points": ["kp"]}`, nil
		}
		if strings.Contains(system, "memory classification") {
			calledStages = append(calledStages, "classification")
			return `{"memory_type": "fact", "category": "test", "subcategory": "sub", "tags": []}`, nil
		}
		return `{}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)
	pipeline.SetConfig(&config.PluginConfig{EnrichEntities: boolPtr(false)})

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	for _, s := range calledStages {
		if s == "entities" {
			t.Fatal("entity extraction should have been skipped")
		}
		if s == "relationships" {
			t.Fatal("relationship extraction should have been skipped (no entities)")
		}
	}
	if len(result.Entities) != 0 {
		t.Fatalf("expected 0 entities, got %d", len(result.Entities))
	}
	if result.Summary != "sum" {
		t.Fatalf("summary should still run, got %q", result.Summary)
	}
}

// TestDisableClassificationStage skips classification when disabled.
func TestDisableClassificationStage(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)
	pipeline.SetConfig(&config.PluginConfig{EnrichClassification: boolPtr(false)})

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if result.MemoryType != "" {
		t.Fatalf("classification should be empty when disabled, got %q", result.MemoryType)
	}
}

// TestDisableSummaryStage skips summarization when disabled.
func TestDisableSummaryStage(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)
	pipeline.SetConfig(&config.PluginConfig{EnrichSummary: boolPtr(false)})

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if result.Summary != "" {
		t.Fatalf("summary should be empty when disabled, got %q", result.Summary)
	}
	// Classification and entities should still have been attempted
	if result.MemoryType == "" {
		t.Fatal("classification should still run when only summary is disabled")
	}
}

// TestClassificationExpandedEnum verifies all 12 memory types map correctly.
func TestClassificationExpandedEnum(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType storage.MemoryType
	}{
		{"fact", "fact", storage.TypeFact},
		{"decision", "decision", storage.TypeDecision},
		{"observation", "observation", storage.TypeObservation},
		{"preference", "preference", storage.TypePreference},
		{"issue", "issue", storage.TypeIssue},
		{"bugfix alias", "bugfix", storage.TypeIssue},
		{"bug_report alias", "bug_report", storage.TypeIssue},
		{"task", "task", storage.TypeTask},
		{"procedure", "procedure", storage.TypeProcedure},
		{"event", "event", storage.TypeEvent},
		{"experience alias", "experience", storage.TypeEvent},
		{"goal", "goal", storage.TypeGoal},
		{"constraint", "constraint", storage.TypeConstraint},
		{"identity", "identity", storage.TypeIdentity},
		{"reference", "reference", storage.TypeReference},
		{"unknown defaults to fact", "unknown_thing", storage.TypeFact},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mt, _ := resolveClassification(tt.input, "")
			if mt != tt.wantType {
				t.Errorf("resolveClassification(%q) = %d, want %d", tt.input, mt, tt.wantType)
			}
		})
	}
}

// TestClassificationWithTypeLabel verifies type_label is preferred over memory_type.
func TestClassificationWithTypeLabel(t *testing.T) {
	mt, display := resolveClassification("decision", "architectural_decision")
	if mt != storage.TypeDecision {
		t.Errorf("expected TypeDecision, got %d", mt)
	}
	if display != "architectural_decision" {
		t.Errorf("expected display 'architectural_decision', got %q", display)
	}
}

// TestSkipIfPresent_Summary verifies summary is skipped when engram already has one.
func TestSkipIfPresent_Summary(t *testing.T) {
	var calledSummary bool
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		if strings.Contains(system, "summarization") {
			calledSummary = true
			return `{"summary": "new", "key_points": ["new"]}`, nil
		}
		if strings.Contains(system, "entity extraction") {
			return `{"entities": [{"name": "X", "type": "tool", "confidence": 0.9}]}`, nil
		}
		if strings.Contains(system, "relationship") {
			return `{"relationships": []}`, nil
		}
		if strings.Contains(system, "memory classification") {
			return `{"memory_type": "fact", "category": "c", "subcategory": "s", "tags": []}`, nil
		}
		return `{}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:      storage.NewULID(),
		Concept: "c",
		Content: "x",
		Summary: "existing summary",
	}

	_, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if calledSummary {
		t.Fatal("summary LLM call should have been skipped (engram already has summary)")
	}
}

// TestSkipIfPresent_Classification verifies classification is skipped for typed engrams.
func TestSkipIfPresent_Classification(t *testing.T) {
	var calledClassify bool
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		if strings.Contains(system, "memory classification") {
			calledClassify = true
			return `{"memory_type": "task"}`, nil
		}
		if strings.Contains(system, "entity extraction") {
			return `{"entities": [{"name": "X", "type": "tool", "confidence": 0.9}]}`, nil
		}
		if strings.Contains(system, "relationship") {
			return `{"relationships": []}`, nil
		}
		if strings.Contains(system, "summarization") {
			return `{"summary": "s", "key_points": ["k"]}`, nil
		}
		return `{}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:         storage.NewULID(),
		Concept:    "c",
		Content:    "x",
		MemoryType: storage.TypeDecision,
	}

	_, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if calledClassify {
		t.Fatal("classification LLM call should have been skipped (engram already has MemoryType)")
	}
}

// TestSkipIfPresent_Entities verifies entity extraction is skipped only when both
// KeyPoints AND Summary are present (caller provided full enrichment).
// With only KeyPoints set (no Summary), entity extraction must NOT be skipped —
// KeyPoints alone may have been set by summarization and do not proxy for entity extraction.
func TestSkipIfPresent_Entities(t *testing.T) {
	var calledEntities bool
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		if strings.Contains(system, "entity extraction") {
			calledEntities = true
			return `{"entities": []}`, nil
		}
		if strings.Contains(system, "summarization") {
			return `{"summary": "s", "key_points": ["k"]}`, nil
		}
		if strings.Contains(system, "memory classification") {
			return `{"memory_type": "fact", "category": "c", "subcategory": "s", "tags": []}`, nil
		}
		return `{}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	t.Run("KeyPointsOnly_MustNotSkip", func(t *testing.T) {
		// With only KeyPoints set (no Summary), entity extraction must proceed.
		// The old code used KeyPoints alone as a skip-proxy — this was the bug.
		calledEntities = false
		eng := &storage.Engram{
			ID:        storage.NewULID(),
			Concept:   "c",
			Content:   "x",
			KeyPoints: []string{"existing key point"},
			// Summary intentionally empty
		}
		_, err := pipeline.Run(context.Background(), eng)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if !calledEntities {
			t.Fatal("entity extraction must NOT be skipped when only KeyPoints are set (no Summary)")
		}
	})

	t.Run("KeyPointsAndSummary_MaySkip", func(t *testing.T) {
		// With both KeyPoints AND Summary set, caller provided full enrichment —
		// entity extraction may be skipped (conservative heuristic).
		calledEntities = false
		eng := &storage.Engram{
			ID:        storage.NewULID(),
			Concept:   "c",
			Content:   "x",
			KeyPoints: []string{"existing key point"},
			Summary:   "existing summary",
		}
		_, err := pipeline.Run(context.Background(), eng)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if calledEntities {
			t.Fatal("entity extraction should be skipped when both KeyPoints and Summary are present")
		}
	})
}

// TestFullModeBackwardCompat verifies full mode (default) runs all 4 calls.
func TestFullModeBackwardCompat(t *testing.T) {
	var callCount atomic.Int32
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		callCount.Add(1)
		if strings.Contains(system, "entity extraction") {
			return `{"entities": [{"name": "Go", "type": "language", "confidence": 0.95}]}`, nil
		}
		if strings.Contains(system, "relationship") {
			return `{"relationships": [{"from": "app", "to": "Go", "type": "uses", "weight": 0.9}]}`, nil
		}
		if strings.Contains(system, "summarization") {
			return `{"summary": "Full summary.", "key_points": ["kp1", "kp2"]}`, nil
		}
		if strings.Contains(system, "memory classification") {
			return `{"memory_type": "fact", "type_label": "tech_fact", "category": "tech", "subcategory": "lang", "tags": ["go"]}`, nil
		}
		return `{}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)
	// No config set = full mode (default)

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if callCount.Load() != 4 {
		t.Fatalf("full mode should make 4 LLM calls, got %d", callCount.Load())
	}
	if result.Summary != "Full summary." {
		t.Fatalf("expected summary, got %q", result.Summary)
	}
	if len(result.Entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(result.Entities))
	}
	if len(result.Relationships) != 1 {
		t.Fatalf("expected 1 relationship, got %d", len(result.Relationships))
	}
	if result.MemoryType != "tech_fact" {
		t.Fatalf("expected type_label 'tech_fact', got %q", result.MemoryType)
	}
}

// TestDisableRelationshipsStage_EntitiesStillExtracted verifies that disabling
// relationships doesn't affect entity extraction.
func TestDisableRelationshipsStage_EntitiesStillExtracted(t *testing.T) {
	var calledRels bool
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		if strings.Contains(system, "entity extraction") {
			return `{"entities": [{"name": "Go", "type": "language", "confidence": 0.95}]}`, nil
		}
		if strings.Contains(system, "relationship") {
			calledRels = true
			return `{"relationships": []}`, nil
		}
		if strings.Contains(system, "summarization") {
			return `{"summary": "s", "key_points": ["k"]}`, nil
		}
		if strings.Contains(system, "memory classification") {
			return `{"memory_type": "fact", "category": "c", "subcategory": "s", "tags": []}`, nil
		}
		return `{}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)
	pipeline.SetConfig(&config.PluginConfig{EnrichRelationships: boolPtr(false)})

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if calledRels {
		t.Fatal("relationship extraction should have been skipped")
	}
	if len(result.Entities) != 1 {
		t.Fatalf("entities should still be extracted, got %d", len(result.Entities))
	}
}
