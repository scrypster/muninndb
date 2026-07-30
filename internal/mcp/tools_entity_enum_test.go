package mcp

import (
	"testing"
)

// muninn_remember's entities[].type carried a strict 14-value JSON-Schema `enum`
// (added 2026-06-11). Server-side it is DEAD WEIGHT: normalizeEntityType coerces
// any unrecognised value to "other" on every user-facing write path, so the enum
// changes nothing about what is stored. Client-side it is a hard constraint — a
// writer whose entity does not map cleanly onto one of the 14 buckets, or whose
// client validates arguments before sending, drops the field.
//
// A difference-in-differences control on a real 4,216-engram corpus (aggregate
// counts only) attributed an entity-coverage collapse to exactly this schema:
// muninn_remember_batch never received the enum, and across the change
// batch-shaped writes held at 87.9% -> 90.2% entity coverage while single-write
// coverage fell 76.2% -> 12.8% (DiD +65.7pp in the largest vault, +52.3pp
// overall). The rival "enrichment worker died" explanation is refuted by the
// same data: summarization and classification ran at 92-100% throughout while
// entity coverage sat at 0-25%.
//
// Entity coverage caps every graph-shaped capability in the product (29.9%
// today; jointly 2.49% with the type gate). So this schema line is load-bearing
// for far more than one field.
//
// The fix keeps the vocabulary DISCOVERABLE in the description — the writer
// still learns the 14 recognised types and that anything else becomes "other" —
// while removing the machine-enforced rejection. Guidance, not a gate: an entity
// the caller cannot classify is worth strictly more than no entity at all.
func TestRememberSchema_EntityTypeIsNotEnumConstrained(t *testing.T) {
	items := rememberEntityItemsSchema(t, "muninn_remember")
	props, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatal("muninn_remember entities.items has no properties")
	}
	typeProp, ok := props["type"].(map[string]any)
	if !ok {
		t.Fatal("muninn_remember entities.items.properties.type missing")
	}
	if _, hasEnum := typeProp["enum"]; hasEnum {
		t.Errorf("entities[].type must NOT carry a JSON-Schema enum: it is server-side dead weight "+
			"(normalizeEntityType coerces anyway) and client-side it suppresses the whole field. "+
			"Measured: it cost ~64pp of entity coverage on the single-write path. got %v", typeProp["enum"])
	}
	// The vocabulary must remain DISCOVERABLE even though it is not enforced —
	// dropping the enum must not degrade into telling the caller nothing.
	desc, _ := typeProp["description"].(string)
	if desc == "" {
		t.Fatal("entities[].type must keep a description naming the recognised types")
	}
	for _, name := range []string{"database", "service", "person", "project", "other"} {
		if !containsFold(desc, name) {
			t.Errorf("entities[].type description must still name the recognised type %q so the "+
				"vocabulary stays discoverable without being enforced; got %q", name, desc)
		}
	}
}

// The two write paths must not disagree about how entities are described. The
// enum was only ever added to the single-write path, which is precisely why the
// batch path served as the natural control — but that divergence is itself the
// drift this pins shut, in both directions.
func TestRememberSchema_BatchAndSingleAgreeOnEntityType(t *testing.T) {
	for _, tool := range []string{"muninn_remember", "muninn_remember_batch"} {
		items := rememberEntityItemsSchema(t, tool)
		props, _ := items["properties"].(map[string]any)
		typeProp, ok := props["type"].(map[string]any)
		if !ok {
			t.Fatalf("%s: entities.items.properties.type missing", tool)
		}
		if _, hasEnum := typeProp["enum"]; hasEnum {
			t.Errorf("%s: entities[].type must not be enum-constrained on ANY write path", tool)
		}
		if desc, _ := typeProp["description"].(string); desc == "" {
			t.Errorf("%s: entities[].type must carry a description on every write path "+
				"(the batch path historically carried none, which is its own capture gap)", tool)
		}
	}
}

// rememberEntityItemsSchema digs out entities.items for a named tool, failing
// loudly rather than silently returning an empty map if the shape drifts.
func rememberEntityItemsSchema(t *testing.T, toolName string) map[string]any {
	t.Helper()
	var schema map[string]any
	for _, td := range allToolDefinitions() {
		if td.Name == toolName {
			s, ok := td.InputSchema.(map[string]any)
			if !ok {
				t.Fatalf("%s: InputSchema is not a map[string]any (%T)", toolName, td.InputSchema)
			}
			schema = s
			break
		}
	}
	if schema == nil {
		t.Fatalf("tool %q not found in allToolDefinitions()", toolName)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s: schema has no properties", toolName)
	}
	// remember_batch nests per-memory properties under memories.items.
	if toolName == "muninn_remember_batch" {
		mem, ok := props["memories"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no memories property", toolName)
		}
		memItems, ok := mem["items"].(map[string]any)
		if !ok {
			t.Fatalf("%s: memories has no items", toolName)
		}
		props, ok = memItems["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s: memories.items has no properties", toolName)
		}
	}
	ents, ok := props["entities"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no entities property", toolName)
	}
	items, ok := ents["items"].(map[string]any)
	if !ok {
		t.Fatalf("%s: entities has no items", toolName)
	}
	return items
}

func containsFold(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexFold(haystack, needle) >= 0
}

func indexFold(s, substr string) int {
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 32
		}
		return b
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if lower(s[i+j]) != lower(substr[j]) {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
