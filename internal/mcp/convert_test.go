package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func TestTextContentEnvelope(t *testing.T) {
	payload := `{"id":"abc123","status":"ok"}`
	result := textContent(payload)

	contentRaw, exists := result["content"]
	if !exists {
		t.Fatal("result missing 'content' key")
	}

	content, ok := contentRaw.([]map[string]any)
	if !ok {
		t.Fatalf("content should be []map[string]any, got %T", contentRaw)
	}
	if len(content) != 1 {
		t.Fatalf("content should have exactly 1 element, got %d", len(content))
	}

	elem := content[0]
	if elem["type"] != "text" {
		t.Errorf("content[0].type = %v, want \"text\"", elem["type"])
	}
	if elem["text"] != payload {
		t.Errorf("content[0].text = %v, want %q", elem["text"], payload)
	}

	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	var roundtrip map[string]any
	if err := json.Unmarshal(b, &roundtrip); err != nil {
		t.Fatalf("json.Unmarshal roundtrip failed: %v", err)
	}
	ct, ok := roundtrip["content"].([]any)
	if !ok || len(ct) != 1 {
		t.Fatalf("roundtrip content not []any with 1 element: %T", roundtrip["content"])
	}
	item, ok := ct[0].(map[string]any)
	if !ok {
		t.Fatalf("roundtrip content[0] not map: %T", ct[0])
	}
	if item["type"] != "text" || item["text"] != payload {
		t.Errorf("roundtrip mismatch: type=%v text=%v", item["type"], item["text"])
	}
}

func TestConvertActivationToMemory(t *testing.T) {
	item := &mbp.ActivationItem{
		ID:         "abc123",
		Concept:    "test concept",
		Content:    "short content",
		Score:      0.9,
		Confidence: 0.85,
		Why:        "found in context",
	}
	m := activationToMemory(item)
	if m.Concept != "test concept" {
		t.Errorf("concept = %q, want %q", m.Concept, "test concept")
	}
	if m.Content != "short content" {
		t.Errorf("content = %q, want %q", m.Content, "short content")
	}
	if m.ID != "abc123" {
		t.Errorf("id = %q, want %q", m.ID, "abc123")
	}
}

// TestConvertActivationToMemory_SupersessionAlwaysOn verifies Increment 2: when
// the ranking marked an item superseded, activationToMemory attaches the
// superseded_by/current_version annotation WITHOUT any annotate flag — an agent
// is never handed a stale fact silently.
func TestConvertActivationToMemory_SupersessionAlwaysOn(t *testing.T) {
	item := &mbp.ActivationItem{
		ID: "staleID", Concept: "runway 8mo", Content: "old",
		SupersededBy: "newID", CurrentVersion: "headID",
	}
	m := activationToMemory(item)
	if m.Annotations == nil {
		t.Fatal("superseded item must carry annotations without annotate=true")
	}
	if m.Annotations.SupersededBy != "newID" {
		t.Errorf("superseded_by = %q, want newID", m.Annotations.SupersededBy)
	}
	if m.Annotations.CurrentVersion != "headID" {
		t.Errorf("current_version = %q, want headID", m.Annotations.CurrentVersion)
	}

	// A non-superseded item gets no annotations block (omitempty stays clean).
	plain := activationToMemory(&mbp.ActivationItem{ID: "x", Concept: "c", Content: "y"})
	if plain.Annotations != nil {
		t.Errorf("non-superseded item must not carry annotations, got %+v", plain.Annotations)
	}
}

func TestConvertTruncatesLongContent(t *testing.T) {
	long := make([]byte, 600)
	for i := range long {
		long[i] = 'x'
	}

	item := &mbp.ActivationItem{
		ID:      "test-id",
		Content: string(long),
	}
	m := activationToMemory(item)
	if len(m.Content) > 503 { // 500 + "..."
		t.Errorf("content not truncated: len=%d", len(m.Content))
	}
	if m.Content[len(m.Content)-3:] != "..." {
		t.Error("truncated content must end with '...'")
	}
}

func TestConvertUsesContentWhenNoSummary(t *testing.T) {
	item := &mbp.ActivationItem{
		ID:      "test-id",
		Content: "the content",
	}
	m := activationToMemory(item)
	if m.Content != "the content" {
		t.Errorf("content = %q, want %q", m.Content, "the content")
	}
}

// TestActivationToMemoryFreshnessFull verifies that all four freshness fields
// from ActivationItem are mapped correctly onto the resulting Memory.
func TestActivationToMemoryFreshnessFull(t *testing.T) {
	const lastAccessNs = int64(1700000000_000000000) // a fixed nanosecond timestamp
	item := &mbp.ActivationItem{
		ID:          "fresh-id",
		Concept:     "freshness concept",
		Content:     "freshness content",
		Score:       0.75,
		LastAccess:  lastAccessNs,
		AccessCount: 42,
		Relevance:   0.88,
		SourceType:  "human",
	}
	m := activationToMemory(item)

	if m.AccessCount != 42 {
		t.Errorf("AccessCount = %d, want 42", m.AccessCount)
	}
	if m.Relevance != 0.88 {
		t.Errorf("Relevance = %v, want 0.88", m.Relevance)
	}
	if m.SourceType != "human" {
		t.Errorf("SourceType = %q, want %q", m.SourceType, "human")
	}
	wantTime := time.Unix(0, lastAccessNs).UTC()
	if !m.LastAccess.Equal(wantTime) {
		t.Errorf("LastAccess = %v, want %v", m.LastAccess, wantTime)
	}
}

// TestActivationToMemoryLastAccessConversion verifies the int64 nanosecond →
// UTC time.Time conversion is correct for a known timestamp.
func TestActivationToMemoryLastAccessConversion(t *testing.T) {
	// 2024-01-15 12:00:00 UTC expressed in nanoseconds
	wantTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	ns := wantTime.UnixNano()

	item := &mbp.ActivationItem{
		ID:         "ts-test",
		LastAccess: ns,
	}
	m := activationToMemory(item)

	if !m.LastAccess.Equal(wantTime) {
		t.Errorf("LastAccess = %v, want %v", m.LastAccess, wantTime)
	}
	if m.LastAccess.Location() != time.UTC {
		t.Errorf("LastAccess location = %v, want UTC", m.LastAccess.Location())
	}
}

// TestActivationToMemoryLastAccessZero verifies that a zero LastAccess value
// produces time.Unix(0,0).UTC() (the zero Unix epoch in UTC).
func TestActivationToMemoryLastAccessZero(t *testing.T) {
	item := &mbp.ActivationItem{
		ID:         "zero-ts",
		LastAccess: 0,
	}
	m := activationToMemory(item)

	want := time.Unix(0, 0).UTC()
	if !m.LastAccess.Equal(want) {
		t.Errorf("LastAccess with 0 input = %v, want %v", m.LastAccess, want)
	}
}

// TestActivationToMemoryEmptySourceType verifies that an empty SourceType on
// the ActivationItem results in an empty SourceType on the Memory.
func TestActivationToMemoryEmptySourceType(t *testing.T) {
	item := &mbp.ActivationItem{
		ID:         "no-source",
		SourceType: "",
	}
	m := activationToMemory(item)

	if m.SourceType != "" {
		t.Errorf("SourceType = %q, want empty string", m.SourceType)
	}
}

// TestActivationToMemoryCreatedAt is a regression test for GitHub issue #97:
// muninn_recall returned created_at: 0001-01-01T00:00:00Z (Go zero-value) for
// all engrams because CreatedAt was not mapped through the ActivationItem pipeline.
func TestActivationToMemoryCreatedAt(t *testing.T) {
	// Use a well-known timestamp to avoid test fragility.
	want := time.Date(2026, 3, 6, 20, 15, 29, 0, time.UTC)
	item := &mbp.ActivationItem{
		ID:        "engram-abc",
		Concept:   "test",
		Content:   "content",
		CreatedAt: want.UnixNano(),
	}
	m := activationToMemory(item)

	if m.CreatedAt.IsZero() {
		t.Fatal("CreatedAt is zero — regression: issue #97 not fixed")
	}
	if !m.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", m.CreatedAt, want)
	}
	if m.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", m.CreatedAt.Location())
	}
}

// TestActivationToMemoryCreatedAtZero verifies that a zero CreatedAt (not yet
// persisted, or old data) maps to the Unix epoch, not a Go zero time.
func TestActivationToMemoryCreatedAtZero(t *testing.T) {
	item := &mbp.ActivationItem{
		ID:        "engram-zero",
		CreatedAt: 0,
	}
	m := activationToMemory(item)

	want := time.Unix(0, 0).UTC()
	if !m.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt with 0 input = %v, want %v", m.CreatedAt, want)
	}
}

func TestConvertReadResponseToMemory(t *testing.T) {
	resp := &mbp.ReadResponse{
		ID:         "read-123",
		Concept:    "stored concept",
		Content:    "stored content",
		Confidence: 0.95,
		State:      1,
		Tags:       []string{"tag1", "tag2"},
	}
	m := readResponseToMemory(resp)
	if m.ID != "read-123" {
		t.Errorf("id = %q, want %q", m.ID, "read-123")
	}
	if m.Concept != "stored concept" {
		t.Errorf("concept = %q, want %q", m.Concept, "stored concept")
	}
	if len(m.Tags) != 2 {
		t.Errorf("tags len = %d, want 2", len(m.Tags))
	}
}

// TestReadResponseToMemory_FullContent verifies muninn_read returns full content
// without truncation — regression guard for issue #112.
func TestReadResponseToMemory_FullContent(t *testing.T) {
	long := make([]byte, 2000)
	for i := range long {
		long[i] = 'y'
	}
	resp := &mbp.ReadResponse{
		ID:      "full-read",
		Content: string(long),
	}
	m := readResponseToMemory(resp)
	if len(m.Content) != 2000 {
		t.Errorf("readResponseToMemory truncated content: got len %d, want 2000", len(m.Content))
	}
}

// TestReadResponseToMemory_MapsummaryField verifies that Summary from the read
// response is propagated to the Memory.
func TestReadResponseToMemory_MapsSummaryField(t *testing.T) {
	resp := &mbp.ReadResponse{
		ID:      "sum-read",
		Content: "full content here",
		Summary: "short summary",
	}
	m := readResponseToMemory(resp)
	if m.Summary != "short summary" {
		t.Errorf("Summary = %q, want %q", m.Summary, "short summary")
	}
	if m.Content != "full content here" {
		t.Errorf("Content = %q, want full content", m.Content)
	}
}

// TestActivationToMemory_PrefersSummary verifies that muninn_recall keeps the
// enrichment summary in Summary while Content carries the real engram content
// (not a duplicate of the summary). Regression test for #502 defect (a).
func TestActivationToMemory_PrefersSummary(t *testing.T) {
	const realContent = "this is the full long content that goes well beyond any preview limit"
	item := &mbp.ActivationItem{
		ID:      "recall-with-summary",
		Concept: "concept",
		Content: realContent,
		Summary: "short enriched summary",
	}
	m := activationToMemory(item)
	if m.Summary != "short enriched summary" {
		t.Errorf("Summary = %q, want %q", m.Summary, "short enriched summary")
	}
	// Content must be the real engram content, never a duplicate of the summary.
	if m.Content == m.Summary {
		t.Errorf("Content duplicates Summary (#502 (a)): both = %q", m.Content)
	}
	if m.Content != realContent {
		t.Errorf("Content = %q, want real content %q", m.Content, realContent)
	}
}

// TestActivationToMemory_StatePopulated verifies that the lifecycle state is
// carried through recall and labelled like the read path. Regression for #502 (b).
func TestActivationToMemory_StatePopulated(t *testing.T) {
	item := &mbp.ActivationItem{
		ID:      "recall-state",
		Content: "content",
		State:   uint8(storage.StateActive),
	}
	m := activationToMemory(item)
	if m.State != "active" {
		t.Errorf("State = %q, want %q", m.State, "active")
	}
}

// TestActivationToMemory_StateMapsNonDefault verifies that a non-default lifecycle
// state is carried through recall and labelled. Regression for #502 (b): recall
// used to always emit an empty state because mbp.ActivationItem had no State field.
func TestActivationToMemory_StateMapsNonDefault(t *testing.T) {
	item := &mbp.ActivationItem{
		ID:      "recall-completed",
		Content: "content",
		State:   uint8(storage.StateCompleted),
	}
	m := activationToMemory(item)
	if m.State != "completed" {
		t.Errorf("State = %q, want %q", m.State, "completed")
	}
}

// TestActivationToMemory_StateOmittedWhenEmpty verifies that Memory.State carries
// the omitempty tag so a genuinely empty state label is not serialized as
// "state":"". Regression for #502 (b).
func TestActivationToMemory_StateOmittedWhenEmpty(t *testing.T) {
	// A Memory with an empty State (e.g. a non-recall construction) must omit it.
	b, err := json.Marshal(Memory{ID: "x"})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if v, present := got["state"]; present {
		t.Errorf("empty state should be omitted, got state=%v", v)
	}
}

// TestActivationToMemory_ScoreNoFloat32Noise verifies that widening the float32
// score to float64 does not reproduce quantization noise. Regression for #502 (c).
func TestActivationToMemory_ScoreNoFloat32Noise(t *testing.T) {
	item := &mbp.ActivationItem{
		ID:    "noisy-score",
		Score: 1.15,
	}
	item.ScoreComponents.SemanticSimilarity = 0.85
	m := activationToMemory(item)
	if m.Score != 1.15 {
		t.Errorf("Score = %v, want clean 1.15 (no float32 noise)", m.Score)
	}
	if m.VectorScore != 0.85 {
		t.Errorf("VectorScore = %v, want clean 0.85 (no float32 noise)", m.VectorScore)
	}
}

// TestActivationToMemory_FallsBackToTruncated verifies that when no summary
// exists, muninn_recall falls back to a truncated content preview.
func TestActivationToMemory_FallsBackToTruncated(t *testing.T) {
	long := make([]byte, 800)
	for i := range long {
		long[i] = 'z'
	}
	item := &mbp.ActivationItem{
		ID:      "recall-no-summary",
		Content: string(long),
		Summary: "",
	}
	m := activationToMemory(item)
	if m.Summary != "" {
		t.Errorf("Summary should be empty, got %q", m.Summary)
	}
	if len(m.Content) > contentPreviewLen+3 {
		t.Errorf("Content not truncated: len=%d", len(m.Content))
	}
	if m.Content[len(m.Content)-3:] != "..." {
		t.Error("truncated content must end with '...'")
	}
}

// TestActivationToMemoryMapsType verifies muninn_recall exposes the stored
// memory type as its canonical label plus the writer's free-form type_label.
func TestActivationToMemoryMapsType(t *testing.T) {
	item := &mbp.ActivationItem{
		ID:         "typed-recall",
		MemoryType: uint8(storage.TypeDecision),
		TypeLabel:  "architectural_decision",
	}
	m := activationToMemory(item)
	if m.Type != "decision" {
		t.Errorf("Type = %q, want %q", m.Type, "decision")
	}
	if m.TypeLabel != "architectural_decision" {
		t.Errorf("TypeLabel = %q, want %q", m.TypeLabel, "architectural_decision")
	}
}

// TestActivationToMemoryTypeZeroValueIsFact pins the zero-value story: an
// engram stored without an explicit type (MemoryType 0) presents as "fact",
// so the field is always present and never reads as "type not exposed".
func TestActivationToMemoryTypeZeroValueIsFact(t *testing.T) {
	m := activationToMemory(&mbp.ActivationItem{ID: "untyped-recall"})
	if m.Type != "fact" {
		t.Errorf("Type = %q, want %q", m.Type, "fact")
	}
	if m.TypeLabel != "" {
		t.Errorf("TypeLabel = %q, want empty", m.TypeLabel)
	}
}

// TestActivationToMemory_MapsTags verifies muninn_recall exposes the stored
// tags on Memory.Tags — S4. Previously mbp.ActivationItem had no Tags field
// so recall always dropped them (muninn_read already returned them).
func TestActivationToMemory_MapsTags(t *testing.T) {
	item := &mbp.ActivationItem{
		ID:   "tagged-recall",
		Tags: []string{"one", "two"},
	}
	m := activationToMemory(item)
	if len(m.Tags) != 2 {
		t.Fatalf("Tags len = %d, want 2 (got %v)", len(m.Tags), m.Tags)
	}
	if m.Tags[0] != "one" || m.Tags[1] != "two" {
		t.Errorf("Tags = %v, want [one two]", m.Tags)
	}
}

// TestReadResponseToMemoryMapsType verifies muninn_read maps MemoryType and
// TypeLabel from the wire response (which has always carried them).
func TestReadResponseToMemoryMapsType(t *testing.T) {
	r := &mbp.ReadResponse{
		ID:         "typed-read",
		MemoryType: uint8(storage.TypeProcedure),
		TypeLabel:  "runbook",
	}
	m := readResponseToMemory(r)
	if m.Type != "procedure" {
		t.Errorf("Type = %q, want %q", m.Type, "procedure")
	}
	if m.TypeLabel != "runbook" {
		t.Errorf("TypeLabel = %q, want %q", m.TypeLabel, "runbook")
	}
}
