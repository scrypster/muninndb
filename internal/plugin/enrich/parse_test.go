package enrich

import (
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
)

// TestParseSummary tests parsing of summarization responses.
func TestParseSummary_ValidJSON(t *testing.T) {
	raw := `{"summary": "This is a summary.", "key_points": ["point 1", "point 2"]}`
	summary, keyPoints, err := ParseSummarizeResponse(raw)

	if err != nil {
		t.Fatalf("ParseSummarizeResponse failed: %v", err)
	}

	if summary != "This is a summary." {
		t.Fatalf("Expected summary 'This is a summary.', got: %q", summary)
	}

	if len(keyPoints) != 2 || keyPoints[0] != "point 1" || keyPoints[1] != "point 2" {
		t.Fatalf("Unexpected key points: %v", keyPoints)
	}
}

// TestParseKeyPoints_Fallback tests graceful degradation when JSON parsing fails.
func TestParseSummary_Fallback(t *testing.T) {
	raw := `Here is the result: {"summary": "Test", "key_points": []}`
	summary, _, err := ParseSummarizeResponse(raw)

	// Should still work with preamble text
	if err != nil {
		t.Fatalf("ParseSummarizeResponse failed: %v", err)
	}

	if summary != "Test" {
		t.Fatalf("Expected summary 'Test', got: %q", summary)
	}
}

// TestParseEntities_ValidJSON tests parsing of entity responses.
func TestParseEntities_ValidJSON(t *testing.T) {
	raw := `{"entities": [{"name": "PostgreSQL", "type": "database", "confidence": 0.95}]}`
	entities, err := ParseEntityResponse(raw)

	if err != nil {
		t.Fatalf("ParseEntityResponse failed: %v", err)
	}

	if len(entities) != 1 {
		t.Fatalf("Expected 1 entity, got: %d", len(entities))
	}

	if entities[0].Name != "PostgreSQL" || entities[0].Type != "database" {
		t.Fatalf("Unexpected entity: %+v", entities[0])
	}
}

// TestParseEntities_Empty tests parsing when no entities are found.
func TestParseEntities_Empty(t *testing.T) {
	raw := `{"entities": []}`
	entities, err := ParseEntityResponse(raw)

	if err != nil {
		t.Fatalf("ParseEntityResponse failed: %v", err)
	}

	if len(entities) != 0 {
		t.Fatalf("Expected 0 entities, got: %d", len(entities))
	}
}

// TestParseEntities_BadJSON tests graceful fallback for invalid JSON.
func TestParseEntities_BadJSON(t *testing.T) {
	raw := `This is not valid JSON`
	entities, err := ParseEntityResponse(raw)

	if err == nil {
		t.Fatal("expected parse error for invalid entity JSON")
	}

	if len(entities) != 0 {
		t.Fatalf("Expected 0 entities, got: %d", len(entities))
	}
}

// TestParseClassification tests parsing of classification responses.
func TestParseClassification_ValidJSON(t *testing.T) {
	raw := `{"memory_type": "decision", "type_label": "architectural_decision", "category": "infrastructure", "subcategory": "databases", "tags": ["db", "postgres"]}`
	memType, typeLabel, category, subcategory, tags, err := ParseClassificationResponse(raw)

	if err != nil {
		t.Fatalf("ParseClassificationResponse failed: %v", err)
	}

	if memType != "decision" || typeLabel != "architectural_decision" || category != "infrastructure" || subcategory != "databases" {
		t.Fatalf("Unexpected classification: type=%q label=%q cat=%q subcat=%q", memType, typeLabel, category, subcategory)
	}

	if len(tags) != 2 || tags[0] != "db" {
		t.Fatalf("Unexpected tags: %v", tags)
	}
}

// TestExtractJSON_WithPreamble tests JSON extraction from text with preamble.
func TestExtractJSON_WithPreamble(t *testing.T) {
	raw := `Here is the JSON response:
{"test": "value"}`
	extracted := extractJSON(raw)

	if !contains(extracted, `"test"`) || !contains(extracted, `"value"`) {
		t.Fatalf("Failed to extract JSON from preamble text: %q", extracted)
	}
}

// TestExtractJSON_WithMarkdownFences tests JSON extraction from markdown fences.
func TestExtractJSON_WithMarkdownFences(t *testing.T) {
	raw := "```json\n{\"test\": \"value\"}\n```"
	extracted := extractJSON(raw)

	if !contains(extracted, `"test"`) || !contains(extracted, `"value"`) {
		t.Fatalf("Failed to extract JSON from markdown fences: %q", extracted)
	}
}

// TestParseRelationships_ValidJSON tests parsing of relationship responses.
func TestParseRelationships_ValidJSON(t *testing.T) {
	// Note: RelType is used in struct, but JSON has "type" field
	raw := `{"relationships": [{"from": "PostgreSQL", "to": "backend", "type": "uses", "weight": 0.9}]}`
	rels, err := ParseRelationshipResponse(raw)

	if err != nil {
		t.Fatalf("ParseRelationshipResponse failed: %v", err)
	}

	if len(rels) != 1 {
		t.Fatalf("Expected 1 relationship, got: %d", len(rels))
	}

	if rels[0].FromEntity != "PostgreSQL" || rels[0].ToEntity != "backend" {
		t.Fatalf("Unexpected relationship: %+v", rels[0])
	}
}

// TestNormalizeEntityType tests entity type normalization and validation.
func TestNormalizeEntityType_Valid(t *testing.T) {
	tests := map[string]string{
		// Known types — returned as-is after normalisation.
		"person":       "person",
		"PERSON":       "person",
		"database":     "database",
		"tool":         "tool",
		"ORGANIZATION": "organization",
		// UI-colour-map types that were previously missing from the allowlist
		// and were silently coerced to "service".
		"technology": "technology",
		"location":   "location",
		"concept":    "concept",
		"product":    "product",
		"event":      "event",
		// Unknown types are passed through (not coerced to "service").
		"unknown":  "unknown",
		"library":  "library",
		"LIBRARY":  "library", // still normalised to lowercase
	}

	for input, expected := range tests {
		result := normalizeEntityType(input)
		if result != expected {
			t.Errorf("normalizeEntityType(%q): got %q, want %q", input, result, expected)
		}
	}
}

// TestNormalizeEntityType_UnknownPassThrough verifies that unknown entity types
// are returned as their normalised string rather than silently coerced to
// "service". This prevents data corruption when an LLM returns a valid semantic
// type (e.g. "library", "concept", "event") that is not yet in the allowlist.
func TestNormalizeEntityType_UnknownPassThrough(t *testing.T) {
	unknownTypes := []string{"library", "algorithm", "protocol", "api", "config", "file"}
	for _, typ := range unknownTypes {
		result := normalizeEntityType(typ)
		if result == "service" {
			t.Errorf("normalizeEntityType(%q) = %q, must not coerce unknown types to \"service\"", typ, result)
		}
		if result != typ {
			t.Errorf("normalizeEntityType(%q) = %q, want pass-through %q", typ, result, typ)
		}
	}
}

// TestValidateAndDedupeEntities tests deduplication, empty names, and confidence clamping.
func TestValidateAndDedupeEntities(t *testing.T) {
	input := []plugin.ExtractedEntity{
		{Name: "PostgreSQL", Type: "database", Confidence: 0.8},
		{Name: "PostgreSQL", Type: "database", Confidence: 0.95}, // higher confidence wins
		{Name: "Redis", Type: "tool", Confidence: 0.7},
		{Name: "", Type: "tool", Confidence: 0.5},       // empty name => skipped
		{Name: "Neg", Type: "person", Confidence: -0.5}, // clamped to 0.0
		{Name: "Over", Type: "person", Confidence: 1.5}, // clamped to 1.0
	}

	result := validateAndDedupeEntities(input)

	byName := map[string]plugin.ExtractedEntity{}
	for _, e := range result {
		byName[e.Name] = e
	}

	if _, ok := byName[""]; ok {
		t.Fatal("empty-name entity should have been removed")
	}

	pg, ok := byName["PostgreSQL"]
	if !ok {
		t.Fatal("PostgreSQL missing")
	}
	if pg.Confidence != 0.95 {
		t.Fatalf("expected PostgreSQL confidence 0.95, got %v", pg.Confidence)
	}

	neg := byName["Neg"]
	if neg.Confidence != 0.0 {
		t.Fatalf("expected clamped confidence 0.0, got %v", neg.Confidence)
	}
	over := byName["Over"]
	if over.Confidence != 1.0 {
		t.Fatalf("expected clamped confidence 1.0, got %v", over.Confidence)
	}
}

func TestValidateRelationships(t *testing.T) {
	input := []plugin.ExtractedRelation{
		{FromEntity: "A", ToEntity: "B", RelType: "uses", Weight: 0.9},
		{FromEntity: "", ToEntity: "B", RelType: "uses", Weight: 0.5},   // empty from => skip
		{FromEntity: "A", ToEntity: "", RelType: "uses", Weight: 0.5},   // empty to => skip
		{FromEntity: "C", ToEntity: "D", RelType: "uses", Weight: -0.3}, // clamped to 0.0
		{FromEntity: "E", ToEntity: "F", RelType: "uses", Weight: 1.5},  // clamped to 1.0
	}

	result := validateRelationships(input)

	if len(result) != 3 {
		t.Fatalf("expected 3 valid relationships, got %d", len(result))
	}

	if result[1].Weight != 0.0 {
		t.Fatalf("expected clamped weight 0.0, got %v", result[1].Weight)
	}
	if result[2].Weight != 1.0 {
		t.Fatalf("expected clamped weight 1.0, got %v", result[2].Weight)
	}
}

func TestParseEntityResponse_DirectArray(t *testing.T) {
	raw := `[{"name": "Go", "type": "language", "confidence": 0.9}]`
	entities, err := ParseEntityResponse(raw)
	if err != nil {
		t.Fatalf("ParseEntityResponse failed: %v", err)
	}
	if len(entities) != 1 || entities[0].Name != "Go" {
		t.Fatalf("unexpected entities: %+v", entities)
	}
}

func TestParseRelationshipResponse_DirectArray(t *testing.T) {
	raw := `[{"from": "A", "to": "B", "type": "uses", "weight": 0.8}]`
	rels, err := ParseRelationshipResponse(raw)
	if err != nil {
		t.Fatalf("ParseRelationshipResponse failed: %v", err)
	}
	if len(rels) != 1 || rels[0].FromEntity != "A" {
		t.Fatalf("unexpected rels: %+v", rels)
	}
}

func TestParseRelationshipResponse_BadJSON(t *testing.T) {
	raw := `not valid json at all`
	rels, err := ParseRelationshipResponse(raw)
	if err == nil {
		t.Fatal("expected parse error for invalid relationship JSON")
	}
	if len(rels) != 0 {
		t.Fatalf("expected 0 relationships, got %d", len(rels))
	}
}

func TestParseEntityResponse_NestedWrapperKeyReturnsError(t *testing.T) {
	raw := `{"meta":{"entities":[]}}`
	entities, err := ParseEntityResponse(raw)
	if err == nil {
		t.Fatal("expected parse error for nested entities wrapper")
	}
	if len(entities) != 0 {
		t.Fatalf("expected 0 entities, got %d", len(entities))
	}
}

func TestParseRelationshipResponse_NestedWrapperKeyReturnsError(t *testing.T) {
	raw := `{"meta":{"relationships":[]}}`
	rels, err := ParseRelationshipResponse(raw)
	if err == nil {
		t.Fatal("expected parse error for nested relationships wrapper")
	}
	if len(rels) != 0 {
		t.Fatalf("expected 0 relationships, got %d", len(rels))
	}
}

func TestParseClassification_BadJSON(t *testing.T) {
	raw := `totally broken {{{`
	memType, typeLabel, cat, subcat, tags, err := ParseClassificationResponse(raw)
	if err == nil {
		t.Fatal("expected parse error for invalid classification JSON")
	}
	if memType != "" || typeLabel != "" || cat != "" || subcat != "" || tags != nil {
		t.Fatal("expected all empty on bad JSON")
	}
}

func TestParseSummarize_BadJSON(t *testing.T) {
	raw := `garbage in`
	summary, keyPoints, err := ParseSummarizeResponse(raw)
	if err == nil {
		t.Fatal("expected parse error for invalid summarize JSON")
	}
	if summary != "" || keyPoints != nil {
		t.Fatal("expected empty on bad JSON")
	}
}

func TestExtractJSON_PlainCodeFences(t *testing.T) {
	raw := "```\n{\"key\": \"val\"}\n```"
	extracted := extractJSON(raw)
	if !contains(extracted, `"key"`) {
		t.Fatalf("failed to extract from plain code fences: %q", extracted)
	}
}

// TestExtractJSON_DuplicateOutput covers models (e.g. llama3.2) that repeat
// their JSON output in a single completion. The parser must return only the
// first complete object and ignore everything after it.
func TestExtractJSON_DuplicateOutput(t *testing.T) {
	raw := `{"entities": [{"name": "foo", "type": "tool"}]} {"entities": [{"name": "bar", "type": "tool"}]}`
	extracted := extractJSON(raw)
	// Must stop at the end of the first object — second object must not appear.
	if contains(extracted, `"bar"`) {
		t.Fatalf("extractJSON grabbed both duplicate objects: %q", extracted)
	}
	if !contains(extracted, `"foo"`) {
		t.Fatalf("extractJSON dropped the first object: %q", extracted)
	}
}

// TestParseEntityResponse_DuplicateOutput is the end-to-end version of the
// above: ParseEntityResponse must succeed and return only the first object's
// entities when the LLM repeats itself.
func TestParseEntityResponse_DuplicateOutput(t *testing.T) {
	raw := `{"entities": [{"name": "fb-automate", "type": "tool", "confidence": 1.0}]} {"entities": [{"name": "reply-comment", "type": "project", "confidence": 0.7}]}`
	entities, err := ParseEntityResponse(raw)
	if err != nil {
		t.Fatalf("ParseEntityResponse failed on duplicate output: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("expected 1 entity from first object, got %d: %+v", len(entities), entities)
	}
	if entities[0].Name != "fb-automate" {
		t.Fatalf("expected 'fb-automate', got %q", entities[0].Name)
	}
}

// TestParseSummarizeResponse_DuplicateOutput ensures summarization parsing
// handles the llama3.2 duplicate-output pattern.
func TestParseSummarizeResponse_DuplicateOutput(t *testing.T) {
	raw := `{"summary": "first summary", "key_points": ["point A"]} {"summary": "second summary", "key_points": ["point B"]}`
	summary, keyPoints, err := ParseSummarizeResponse(raw)
	if err != nil {
		t.Fatalf("ParseSummarizeResponse failed on duplicate output: %v", err)
	}
	if summary != "first summary" {
		t.Fatalf("expected 'first summary', got %q", summary)
	}
	if len(keyPoints) != 1 || keyPoints[0] != "point A" {
		t.Fatalf("expected ['point A'], got %v", keyPoints)
	}
}

// TestExtractJSON_BracketInsideString ensures brackets inside quoted strings
// do not confuse the depth counter.
func TestExtractJSON_BracketInsideString(t *testing.T) {
	raw := `{"key": "value with } brace and { another"}`
	extracted := extractJSON(raw)
	if extracted != raw {
		t.Fatalf("extractJSON mangled JSON with brackets in string: %q", extracted)
	}
}

func TestExtractJSON_NoJSON(t *testing.T) {
	raw := "no json here"
	extracted := extractJSON(raw)
	if extracted != "no json here" {
		t.Fatalf("expected raw string back, got %q", extracted)
	}
}

func TestExtractJSON_ArrayBrackets(t *testing.T) {
	raw := `some text [{"a":1}]`
	extracted := extractJSON(raw)
	if !contains(extracted, `[{"a":1}]`) {
		t.Fatalf("failed to extract array: %q", extracted)
	}
}

func TestParseSummary_WithMarkdownFences(t *testing.T) {
	raw := "```json\n{\"summary\": \"fenced\", \"key_points\": [\"a\"]}\n```"
	summary, kp, err := ParseSummarizeResponse(raw)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if summary != "fenced" {
		t.Fatalf("expected 'fenced', got %q", summary)
	}
	if len(kp) != 1 || kp[0] != "a" {
		t.Fatalf("unexpected key_points: %v", kp)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
