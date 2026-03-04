package mcp

import (
	"encoding/json"
	"testing"
	"time"

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
