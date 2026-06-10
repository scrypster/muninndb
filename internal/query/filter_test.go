package query

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestFilter_ZeroValue tests that an empty filter matches everything
func TestFilter_ZeroValue(t *testing.T) {
	f := &Filter{}

	now := time.Now()
	engrams := []*storage.Engram{
		{
			ID:         storage.NewULID(),
			Concept:    "test1",
			Content:    "content1",
			CreatedAt:  now,
			State:      storage.StateActive,
			Confidence: 0.8,
			Relevance:  0.6,
			Stability:  7,
			CreatedBy:  "user1",
			Tags:       []string{"tag1", "tag2"},
		},
		{
			ID:         storage.NewULID(),
			Concept:    "test2",
			Content:    "content2",
			CreatedAt:  now.Add(-24 * time.Hour),
			State:      storage.StatePaused,
			Confidence: 0.5,
			Relevance:  0.3,
			Stability:  14,
			CreatedBy:  "user2",
			Tags:       []string{"tag3"},
		},
	}

	result := f.Apply(engrams)
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
}

// TestFilter_CreatedAfter filters by creation time
func TestFilter_CreatedAfter(t *testing.T) {
	now := time.Now()
	before := now.Add(-48 * time.Hour)
	after := now.Add(-24 * time.Hour)

	f := &Filter{
		CreatedAfter: &after,
	}

	engrams := []*storage.Engram{
		{
			ID:        storage.NewULID(),
			Concept:   "old",
			Content:   "old content",
			CreatedAt: before, // should not match
			State:     storage.StateActive,
		},
		{
			ID:        storage.NewULID(),
			Concept:   "new",
			Content:   "new content",
			CreatedAt: now, // should match
			State:     storage.StateActive,
		},
	}

	result := f.Apply(engrams)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if result[0].Concept != "new" {
		t.Fatalf("expected 'new', got %q", result[0].Concept)
	}
}

// TestFilter_States_OR tests OR semantics for state filtering
func TestFilter_States_OR(t *testing.T) {
	f := &Filter{
		States: []storage.LifecycleState{storage.StateActive, storage.StatePaused},
	}

	engrams := []*storage.Engram{
		{ID: storage.NewULID(), Concept: "active", State: storage.StateActive},
		{ID: storage.NewULID(), Concept: "paused", State: storage.StatePaused},
		{ID: storage.NewULID(), Concept: "archived", State: storage.StateArchived},
	}

	result := f.Apply(engrams)
	if len(result) != 2 {
		t.Fatalf("expected 2 results (OR semantics), got %d", len(result))
	}

	concepts := map[string]bool{}
	for _, e := range result {
		concepts[e.Concept] = true
	}

	if !concepts["active"] || !concepts["paused"] {
		t.Fatal("expected 'active' and 'paused' results")
	}
	if concepts["archived"] {
		t.Fatal("should not have 'archived' result")
	}
}

// TestFilter_Tags_AND tests AND semantics for tag filtering
func TestFilter_Tags_AND(t *testing.T) {
	f := &Filter{
		Tags: []string{"important", "urgent"},
	}

	engrams := []*storage.Engram{
		{
			ID:      storage.NewULID(),
			Concept: "both",
			Tags:    []string{"important", "urgent"},
			State:   storage.StateActive,
		},
		{
			ID:      storage.NewULID(),
			Concept: "one",
			Tags:    []string{"important", "review"},
			State:   storage.StateActive,
		},
		{
			ID:      storage.NewULID(),
			Concept: "neither",
			Tags:    []string{"review"},
			State:   storage.StateActive,
		},
	}

	result := f.Apply(engrams)
	if len(result) != 1 {
		t.Fatalf("expected 1 result (AND semantics), got %d", len(result))
	}
	if result[0].Concept != "both" {
		t.Fatalf("expected 'both', got %q", result[0].Concept)
	}
}

// TestFilter_MinRelevance filters by relevance score
func TestFilter_MinRelevance(t *testing.T) {
	f := &Filter{
		MinRelevance: 0.5,
	}

	engrams := []*storage.Engram{
		{ID: storage.NewULID(), Concept: "high", Relevance: 0.8, State: storage.StateActive},
		{ID: storage.NewULID(), Concept: "medium", Relevance: 0.5, State: storage.StateActive},
		{ID: storage.NewULID(), Concept: "low", Relevance: 0.3, State: storage.StateActive},
	}

	result := f.Apply(engrams)
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}

	concepts := map[string]bool{}
	for _, e := range result {
		concepts[e.Concept] = true
	}

	if !concepts["high"] || !concepts["medium"] {
		t.Fatal("expected 'high' and 'medium' results")
	}
}

// TestFilter_MinConfidence filters by confidence score
func TestFilter_MinConfidence(t *testing.T) {
	f := &Filter{
		MinConfidence: 0.7,
	}

	engrams := []*storage.Engram{
		{ID: storage.NewULID(), Concept: "high", Confidence: 0.9, State: storage.StateActive},
		{ID: storage.NewULID(), Concept: "medium", Confidence: 0.6, State: storage.StateActive},
	}

	result := f.Apply(engrams)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if result[0].Concept != "high" {
		t.Fatalf("expected 'high', got %q", result[0].Concept)
	}
}

// TestFilter_Combined tests multiple constraints together
func TestFilter_Combined(t *testing.T) {
	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)

	f := &Filter{
		CreatedAfter:  &yesterday,
		States:        []storage.LifecycleState{storage.StateActive},
		Tags:          []string{"important"},
		MinConfidence: 0.7,
	}

	engrams := []*storage.Engram{
		{
			ID:         storage.NewULID(),
			Concept:    "pass",
			CreatedAt:  now,
			State:      storage.StateActive,
			Confidence: 0.8,
			Tags:       []string{"important", "review"},
		},
		{
			ID:         storage.NewULID(),
			Concept:    "old",
			CreatedAt:  now.Add(-48 * time.Hour),
			State:      storage.StateActive,
			Confidence: 0.8,
			Tags:       []string{"important"},
		},
		{
			ID:         storage.NewULID(),
			Concept:    "paused",
			CreatedAt:  now,
			State:      storage.StatePaused,
			Confidence: 0.8,
			Tags:       []string{"important"},
		},
		{
			ID:         storage.NewULID(),
			Concept:    "low_conf",
			CreatedAt:  now,
			State:      storage.StateActive,
			Confidence: 0.5,
			Tags:       []string{"important"},
		},
		{
			ID:         storage.NewULID(),
			Concept:    "no_tag",
			CreatedAt:  now,
			State:      storage.StateActive,
			Confidence: 0.8,
			Tags:       []string{"review"},
		},
	}

	result := f.Apply(engrams)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if result[0].Concept != "pass" {
		t.Fatalf("expected 'pass', got %q", result[0].Concept)
	}
}

// TestFilter_Apply_Pagination tests offset and limit
func TestFilter_Apply_Pagination(t *testing.T) {
	engrams := make([]*storage.Engram, 0, 10)
	for i := 0; i < 10; i++ {
		engrams = append(engrams, &storage.Engram{
			ID:    storage.NewULID(),
			State: storage.StateActive,
		})
	}

	tests := []struct {
		name      string
		offset    int
		limit     int
		want      int
		wantEmpty bool
	}{
		{"first 5", 0, 5, 5, false},
		{"next 3", 5, 3, 3, false},
		{"past end", 12, 5, 0, true},
		{"at end", 10, 5, 0, true},
		{"default limit (0)", 0, 0, 10, false}, // 0 treated as 20, so all 10
		{"limit > max", 0, 300, 10, false},     // capped at 200, all 10 fit
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &Filter{Offset: tt.offset, Limit: tt.limit}
			result := f.Apply(engrams)

			if tt.wantEmpty {
				if len(result) != 0 {
					t.Fatalf("expected empty result, got %d items", len(result))
				}
			} else if len(result) != tt.want {
				t.Fatalf("expected %d results, got %d", tt.want, len(result))
			}
		})
	}
}

// TestFilter_Validate tests validation logic
func TestFilter_Validate(t *testing.T) {
	tests := []struct {
		name    string
		filter  *Filter
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid zero value",
			filter:  &Filter{},
			wantErr: false,
		},
		{
			name:    "valid with all fields",
			filter:  &Filter{Limit: 50, Offset: 10, MinRelevance: 0.5, MinConfidence: 0.6},
			wantErr: false,
		},
		{
			name:    "limit too high",
			filter:  &Filter{Limit: 201},
			wantErr: true,
			errMsg:  "limit must be between 0 and 200",
		},
		{
			name:    "limit negative",
			filter:  &Filter{Limit: -1},
			wantErr: true,
			errMsg:  "limit must be between 0 and 200",
		},
		{
			name:    "offset negative",
			filter:  &Filter{Offset: -1},
			wantErr: true,
			errMsg:  "offset must be non-negative",
		},
		{
			name:    "min_relevance too high",
			filter:  &Filter{MinRelevance: 1.5},
			wantErr: true,
			errMsg:  "min_relevance must be between 0.0 and 1.0",
		},
		{
			name:    "min_relevance negative",
			filter:  &Filter{MinRelevance: -0.1},
			wantErr: true,
			errMsg:  "min_relevance must be between 0.0 and 1.0",
		},
		{
			name:    "min_confidence too high",
			filter:  &Filter{MinConfidence: 1.1},
			wantErr: true,
			errMsg:  "min_confidence must be between 0.0 and 1.0",
		},
		{
			name:    "min_stability negative",
			filter:  &Filter{MinStability: -1},
			wantErr: true,
			errMsg:  "min_stability must be non-negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.filter.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("wantErr=%v, got err=%v", tt.wantErr, err)
			}
			if tt.wantErr && err.Error() != tt.errMsg {
				t.Fatalf("expected error %q, got %q", tt.errMsg, err.Error())
			}
		})
	}
}

// TestFilter_Creator tests creator field filtering
func TestFilter_Creator(t *testing.T) {
	f := &Filter{
		Creator: "alice",
	}

	engrams := []*storage.Engram{
		{ID: storage.NewULID(), CreatedBy: "alice", State: storage.StateActive},
		{ID: storage.NewULID(), CreatedBy: "bob", State: storage.StateActive},
		{ID: storage.NewULID(), CreatedBy: "alice", State: storage.StateActive},
	}

	result := f.Apply(engrams)
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	for _, e := range result {
		if e.CreatedBy != "alice" {
			t.Fatalf("expected creator 'alice', got %q", e.CreatedBy)
		}
	}
}

// TestFilter_MinStability tests stability threshold
func TestFilter_MinStability(t *testing.T) {
	f := &Filter{
		MinStability: 10,
	}

	engrams := []*storage.Engram{
		{ID: storage.NewULID(), Stability: 15, State: storage.StateActive},
		{ID: storage.NewULID(), Stability: 10, State: storage.StateActive},
		{ID: storage.NewULID(), Stability: 5, State: storage.StateActive},
	}

	result := f.Apply(engrams)
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
}

// TestFilter_UpdatedAfter tests updated timestamp filtering
func TestFilter_UpdatedAfter(t *testing.T) {
	now := time.Now()
	threshold := now.Add(-12 * time.Hour)

	f := &Filter{
		UpdatedAfter: &threshold,
	}

	engrams := []*storage.Engram{
		{ID: storage.NewULID(), UpdatedAt: now, State: storage.StateActive},
		{ID: storage.NewULID(), UpdatedAt: now.Add(-24 * time.Hour), State: storage.StateActive},
	}

	result := f.Apply(engrams)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
}

// TestFilter_EmptyVault tests with nil engrams in slice
func TestFilter_EmptyVault(t *testing.T) {
	f := &Filter{}

	engrams := []*storage.Engram{
		nil,
		{ID: storage.NewULID(), State: storage.StateActive},
		nil,
		{ID: storage.NewULID(), State: storage.StateActive},
	}

	result := f.Apply(engrams)
	if len(result) != 2 {
		t.Fatalf("expected 2 results (nil filtered), got %d", len(result))
	}
}

// TestFilter_TagsEmptySlice tests engram with empty tags
func TestFilter_TagsEmptySlice(t *testing.T) {
	f := &Filter{
		Tags: []string{}, // empty filter should match all
	}

	engrams := []*storage.Engram{
		{ID: storage.NewULID(), Tags: []string{}, State: storage.StateActive},
		{ID: storage.NewULID(), Tags: []string{"tag1"}, State: storage.StateActive},
	}

	result := f.Apply(engrams)
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
}

// TestFilterValidate_LimitExceedsMax verifies that a Limit of 201 is rejected.
func TestFilterValidate_LimitExceedsMax(t *testing.T) {
	f := &Filter{Limit: 201}
	if err := f.Validate(); err == nil {
		t.Fatal("expected error for Limit=201, got nil")
	}
}

// TestFilterValidate_NegativeOffset verifies that a negative Offset is rejected.
func TestFilterValidate_NegativeOffset(t *testing.T) {
	f := &Filter{Offset: -1}
	if err := f.Validate(); err == nil {
		t.Fatal("expected error for Offset=-1, got nil")
	}
}

// TestFilterValidate_MinRelevanceOutOfRange verifies that MinRelevance values
// outside [0.0, 1.0] are rejected.
func TestFilterValidate_MinRelevanceOutOfRange(t *testing.T) {
	cases := []struct {
		name         string
		minRelevance float32
	}{
		{"above 1.0", 1.5},
		{"below 0.0", -0.1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Filter{MinRelevance: tc.minRelevance}
			if err := f.Validate(); err == nil {
				t.Fatalf("expected error for MinRelevance=%v, got nil", tc.minRelevance)
			}
		})
	}
}

// TestFilterMatch_TemporalBounds verifies CreatedAfter and CreatedBefore filtering.
func TestFilterMatch_TemporalBounds(t *testing.T) {
	tenDaysAgo := time.Now().Add(-10 * 24 * time.Hour)
	fiveDaysAgo := time.Now().Add(-5 * 24 * time.Hour)

	engram := &storage.Engram{
		CreatedAt: tenDaysAgo,
	}

	t.Run("CreatedAfter 5 days ago does not match engram from 10 days ago", func(t *testing.T) {
		f := &Filter{CreatedAfter: &fiveDaysAgo}
		if f.Match(engram) {
			t.Fatal("expected no match: engram created 10 days ago should not pass CreatedAfter=5 days ago")
		}
	})

	t.Run("CreatedBefore 5 days ago matches engram from 10 days ago", func(t *testing.T) {
		f := &Filter{CreatedBefore: &fiveDaysAgo}
		if !f.Match(engram) {
			t.Fatal("expected match: engram created 10 days ago should pass CreatedBefore=5 days ago")
		}
	})
}

// TestFilterApply_OffsetBeyondSlice verifies that an Offset larger than the
// result set returns an empty slice without panicking.
func TestFilterApply_OffsetBeyondSlice(t *testing.T) {
	engrams := []*storage.Engram{
		{Concept: "a"},
		{Concept: "b"},
		{Concept: "c"},
	}

	f := &Filter{Offset: 10}
	result := f.Apply(engrams)

	if len(result) != 0 {
		t.Fatalf("expected empty slice, got %d items", len(result))
	}
}

// TestFilterApply_LimitDefault verifies that Limit=0 applies the default of 20.
func TestFilterApply_LimitDefault(t *testing.T) {
	// Build 25 engrams so we can observe whether default limit of 20 is applied.
	engrams := make([]*storage.Engram, 25)
	for i := range engrams {
		engrams[i] = &storage.Engram{Concept: "item"}
	}

	f := &Filter{Limit: 0}
	result := f.Apply(engrams)

	const defaultLimit = 20
	if len(result) != defaultLimit {
		t.Fatalf("expected default limit of %d, got %d", defaultLimit, len(result))
	}
}
