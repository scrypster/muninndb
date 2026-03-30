package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestOrderedMap_MarshalJSON_PreservesInsertionOrder(t *testing.T) {
	om := NewOrderedMap()
	om.Set("zebra", 1)
	om.Set("alpha", 2)
	om.Set("middle", 3)

	b, err := json.Marshal(om)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	want := `{"zebra":1,"alpha":2,"middle":3}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestOrderedMap_MarshalJSON_Empty(t *testing.T) {
	om := NewOrderedMap()
	b, err := json.Marshal(om)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{}" {
		t.Errorf("got %s, want {}", string(b))
	}
}

func TestOrderedMap_Set_UpdateKeepsPosition(t *testing.T) {
	om := NewOrderedMap()
	om.Set("a", 1)
	om.Set("b", 2)
	om.Set("a", 99) // update, should keep position 0

	b, err := json.Marshal(om)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	want := `{"a":99,"b":2}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestOrderSchemaProperties_RequiredFirst(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"alpha":   map[string]any{"type": "string"},
			"content": map[string]any{"type": "string"},
			"beta":    map[string]any{"type": "number"},
		},
		"required": []string{"content"},
	}

	ordered := orderSchemaProperties(schema)
	m := ordered.(map[string]any)
	b, err := json.Marshal(m["properties"])
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	// "content" (required) must come before "alpha" and "beta" (optional, alphabetical).
	contentIdx := strings.Index(got, `"content"`)
	alphaIdx := strings.Index(got, `"alpha"`)
	betaIdx := strings.Index(got, `"beta"`)

	if contentIdx > alphaIdx {
		t.Errorf("required 'content' (%d) should appear before optional 'alpha' (%d) in: %s", contentIdx, alphaIdx, got)
	}
	if contentIdx > betaIdx {
		t.Errorf("required 'content' (%d) should appear before optional 'beta' (%d) in: %s", contentIdx, betaIdx, got)
	}
	if alphaIdx > betaIdx {
		t.Errorf("optional 'alpha' (%d) should appear before optional 'beta' (%d) alphabetically in: %s", alphaIdx, betaIdx, got)
	}
}

func TestOrderSchemaProperties_MultipleRequired_PreservesDeclaredOrder(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"zebra":  map[string]any{"type": "string"},
			"id":     map[string]any{"type": "string"},
			"reason": map[string]any{"type": "string"},
			"alpha":  map[string]any{"type": "number"},
		},
		"required": []string{"id", "reason"}, // this order must be preserved
	}

	ordered := orderSchemaProperties(schema)
	b, err := json.Marshal(ordered.(map[string]any)["properties"])
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	idIdx := strings.Index(got, `"id"`)
	reasonIdx := strings.Index(got, `"reason"`)
	alphaIdx := strings.Index(got, `"alpha"`)
	zebraIdx := strings.Index(got, `"zebra"`)

	if idIdx > reasonIdx {
		t.Errorf("required 'id' should come before 'reason' (declared order): %s", got)
	}
	if reasonIdx > alphaIdx {
		t.Errorf("required 'reason' should come before optional 'alpha': %s", got)
	}
	if alphaIdx > zebraIdx {
		t.Errorf("optional 'alpha' should come before 'zebra' (alphabetical): %s", got)
	}
}

func TestOrderSchemaProperties_RecursesIntoItems(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"memories": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"concept": map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
						"tags":    map[string]any{"type": "array"},
					},
					"required": []string{"content"},
				},
			},
		},
		"required": []string{"memories"},
	}

	ordered := orderSchemaProperties(schema)

	// Marshal the full result to check nested ordering.
	b, err := json.Marshal(ordered)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	// In the nested items schema, "content" (required) should come before
	// "concept" and "tags" (optional).
	contentIdx := strings.Index(got, `"content"`)
	conceptIdx := strings.Index(got, `"concept"`)
	tagsIdx := strings.Index(got, `"tags"`)

	if contentIdx > conceptIdx {
		t.Errorf("nested required 'content' should come before optional 'concept': %s", got)
	}
	if contentIdx > tagsIdx {
		t.Errorf("nested required 'content' should come before optional 'tags': %s", got)
	}
}

func TestOrderSchemaProperties_DoesNotMutateOriginal(t *testing.T) {
	original := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"alpha":   map[string]any{"type": "string"},
			"content": map[string]any{"type": "string"},
		},
		"required": []string{"content"},
	}

	_ = orderSchemaProperties(original)

	// The original properties must still be a plain map[string]any.
	if _, ok := original["properties"].(map[string]any); !ok {
		t.Fatal("orderSchemaProperties mutated the original schema — properties is no longer map[string]any")
	}
}

func TestOrderSchemaProperties_NoRequired(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"zebra": map[string]any{"type": "string"},
			"alpha": map[string]any{"type": "string"},
		},
		"required": []string{},
	}

	ordered := orderSchemaProperties(schema)
	b, err := json.Marshal(ordered.(map[string]any)["properties"])
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	alphaIdx := strings.Index(got, `"alpha"`)
	zebraIdx := strings.Index(got, `"zebra"`)
	if alphaIdx > zebraIdx {
		t.Errorf("optional fields should be alphabetical — 'alpha' before 'zebra': %s", got)
	}
}

func TestOrderSchemaProperties_NonObjectPassthrough(t *testing.T) {
	// Non-map inputs should be returned as-is.
	result := orderSchemaProperties("just a string")
	if result != "just a string" {
		t.Errorf("non-map input should pass through unchanged, got %v", result)
	}
}

func TestOrderedMap_MarshalJSON_NestedOrderedMap(t *testing.T) {
	inner := NewOrderedMap()
	inner.Set("z", 1)
	inner.Set("a", 2)

	outer := NewOrderedMap()
	outer.Set("nested", inner)
	outer.Set("flat", "ok")

	b, err := json.Marshal(outer)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"nested":{"z":1,"a":2},"flat":"ok"}`
	if string(b) != want {
		t.Errorf("got %s, want %s", string(b), want)
	}
}

func TestOrderSchemaProperties_RequiredKeyAbsent(t *testing.T) {
	// No "required" key at all (different from empty slice).
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"zebra": map[string]any{"type": "string"},
			"alpha": map[string]any{"type": "string"},
		},
	}

	ordered := orderSchemaProperties(schema)
	b, err := json.Marshal(ordered.(map[string]any)["properties"])
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	alphaIdx := strings.Index(got, `"alpha"`)
	zebraIdx := strings.Index(got, `"zebra"`)
	if alphaIdx > zebraIdx {
		t.Errorf("all-optional fields should be alphabetical: %s", got)
	}
}

func TestOrderSchemaProperties_NoPropertiesKey(t *testing.T) {
	// Schema without "properties" (e.g. a primitive type) — should pass through.
	schema := map[string]any{
		"type":        "string",
		"description": "just a string field",
	}
	result := orderSchemaProperties(schema)
	m := result.(map[string]any)
	if m["type"] != "string" || m["description"] != "just a string field" {
		t.Errorf("schema without properties should pass through unchanged: %v", m)
	}
}

func TestOrderSchemaProperties_RecursesIntoNestedObjectProperties(t *testing.T) {
	// Properties within properties (not via items) — like muninn_remember_tree's "root".
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"root": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tags":    map[string]any{"type": "array"},
					"content": map[string]any{"type": "string"},
					"concept": map[string]any{"type": "string"},
				},
				"required": []string{"concept", "content"},
			},
		},
		"required": []string{"root"},
	}

	ordered := orderSchemaProperties(schema)
	b, err := json.Marshal(ordered)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	// In the nested "root" properties, "concept" and "content" (required)
	// must appear before "tags" (optional).
	conceptIdx := strings.Index(got, `"concept"`)
	contentIdx := strings.Index(got, `"content"`)
	tagsIdx := strings.Index(got, `"tags"`)

	if conceptIdx > tagsIdx {
		t.Errorf("nested required 'concept' should come before optional 'tags': %s", got)
	}
	if contentIdx > tagsIdx {
		t.Errorf("nested required 'content' should come before optional 'tags': %s", got)
	}
}

func TestOrderSchemaProperties_RequiredFieldMissingFromProperties(t *testing.T) {
	// "required" lists a field not present in "properties" — should skip gracefully.
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"alpha": map[string]any{"type": "string"},
			"beta":  map[string]any{"type": "string"},
		},
		"required": []string{"ghost", "alpha"},
	}

	ordered := orderSchemaProperties(schema)
	b, err := json.Marshal(ordered.(map[string]any)["properties"])
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	// "alpha" (required) should come before "beta" (optional).
	// "ghost" is silently skipped (not in properties).
	alphaIdx := strings.Index(got, `"alpha"`)
	betaIdx := strings.Index(got, `"beta"`)
	ghostIdx := strings.Index(got, `"ghost"`)

	if alphaIdx > betaIdx {
		t.Errorf("required 'alpha' should come before optional 'beta': %s", got)
	}
	if ghostIdx != -1 {
		t.Errorf("missing required field 'ghost' should not appear in output: %s", got)
	}
}

func TestOrderSchemaProperties_AllFieldsRequired(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"zebra": map[string]any{"type": "string"},
			"alpha": map[string]any{"type": "string"},
		},
		"required": []string{"zebra", "alpha"},
	}

	ordered := orderSchemaProperties(schema)
	b, err := json.Marshal(ordered.(map[string]any)["properties"])
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	// Both required — declared order preserved: "zebra" before "alpha".
	zebraIdx := strings.Index(got, `"zebra"`)
	alphaIdx := strings.Index(got, `"alpha"`)
	if zebraIdx > alphaIdx {
		t.Errorf("required fields should follow declared order — 'zebra' before 'alpha': %s", got)
	}
}

func TestToolDefinition_MarshalJSON_OrdersProperties(t *testing.T) {
	td := ToolDefinition{
		Name:        "test_tool",
		Description: "A test tool.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"alpha":   map[string]any{"type": "string", "description": "optional"},
				"content": map[string]any{"type": "string", "description": "required"},
				"beta":    map[string]any{"type": "number", "description": "optional"},
			},
			"required": []string{"content"},
		},
	}

	b, err := json.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	// In the JSON output, "content" must appear before "alpha" within properties.
	contentIdx := strings.Index(got, `"content"`)
	alphaIdx := strings.Index(got, `"alpha"`)
	if contentIdx > alphaIdx {
		t.Errorf("MarshalJSON should order required 'content' before optional 'alpha': %s", got)
	}
}

func TestToolDefinition_MarshalJSON_PreservesOriginalSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"alpha":   map[string]any{"type": "string"},
			"content": map[string]any{"type": "string"},
		},
		"required": []string{"content"},
	}
	td := ToolDefinition{
		Name:        "test_tool",
		Description: "A test tool.",
		InputSchema: schema,
	}

	// Marshal (triggers ordering).
	if _, err := json.Marshal(td); err != nil {
		t.Fatal(err)
	}

	// The in-memory schema must still be a plain map.
	if _, ok := schema["properties"].(map[string]any); !ok {
		t.Fatal("MarshalJSON mutated the original InputSchema")
	}
}

func TestToolDefinition_MarshalJSON_Idempotent(t *testing.T) {
	td := ToolDefinition{
		Name:        "test_tool",
		Description: "A test tool.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"alpha":   map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"content"},
		},
	}

	b1, err := json.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := json.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Errorf("MarshalJSON is not idempotent:\n  first:  %s\n  second: %s", b1, b2)
	}
}

func TestToolDefinition_MarshalJSON_StructureIntegrity(t *testing.T) {
	td := ToolDefinition{
		Name:        "my_tool",
		Description: "Does things.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
			"required":   []string{"x"},
		},
	}

	b, err := json.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		InputSchema struct {
			Type       string                       `json:"type"`
			Properties map[string]map[string]string  `json:"properties"`
			Required   []string                      `json:"required"`
		} `json:"inputSchema"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if parsed.Name != "my_tool" {
		t.Errorf("name: got %q, want %q", parsed.Name, "my_tool")
	}
	if parsed.Description != "Does things." {
		t.Errorf("description: got %q, want %q", parsed.Description, "Does things.")
	}
	if parsed.InputSchema.Type != "object" {
		t.Errorf("inputSchema.type: got %q, want %q", parsed.InputSchema.Type, "object")
	}
	if _, ok := parsed.InputSchema.Properties["x"]; !ok {
		t.Error("inputSchema.properties.x missing")
	}
	if len(parsed.InputSchema.Required) != 1 || parsed.InputSchema.Required[0] != "x" {
		t.Errorf("inputSchema.required: got %v, want [x]", parsed.InputSchema.Required)
	}
}

// ---------------------------------------------------------------------------
// Regression guard: all tools, all nesting levels
// ---------------------------------------------------------------------------

// extractJSONObjectKeys uses json.Decoder to extract top-level keys from a
// JSON object in their serialized order.
func extractJSONObjectKeys(data []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	t, err := dec.Token()
	if err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("expected JSON object, got %v", t)
	}
	var keys []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("expected string key, got %T", t)
		}
		keys = append(keys, key)
		// Consume the value (handles nested objects/arrays).
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// assertRequiredFirst checks that in jsonData (a JSON object with
// "properties" and "required"), every required property key appears
// before every optional key.  It recurses into nested schemas found
// in property values (objects with their own "properties") and into
// "items" (array schemas).
func assertRequiredFirst(t *testing.T, path string, jsonData []byte, schema map[string]any) {
	t.Helper()

	props, _ := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]string)

	// --- check this level ---------------------------------------------------
	if len(required) > 0 && len(props) > 0 {
		// Decode the JSON to reach the "properties" value.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(jsonData, &obj); err != nil {
			t.Fatalf("%s: unmarshal: %v", path, err)
		}
		propsJSON, ok := obj["properties"]
		if !ok {
			t.Fatalf("%s: no properties key in JSON", path)
		}

		keys, err := extractJSONObjectKeys(propsJSON)
		if err != nil {
			t.Fatalf("%s: extractKeys: %v", path, err)
		}

		reqSet := make(map[string]bool, len(required))
		for _, r := range required {
			reqSet[r] = true
		}

		seenOptional := false
		firstOpt := ""
		for _, k := range keys {
			if reqSet[k] {
				if seenOptional {
					t.Errorf("%sproperties: required %q appears after optional %q; key order: %v",
						path, k, firstOpt, keys)
					return // one failure per level is enough
				}
			} else {
				if !seenOptional {
					seenOptional = true
					firstOpt = k
				}
			}
		}

		// Recurse into each property value that is itself a schema.
		var propsObj map[string]json.RawMessage
		if err := json.Unmarshal(propsJSON, &propsObj); err == nil {
			for propName, propRaw := range propsObj {
				if sub, ok := props[propName].(map[string]any); ok {
					assertRequiredFirst(t, fmt.Sprintf("%sproperties.%s.", path, propName), propRaw, sub)
				}
			}
		}
	}

	// --- recurse into "items" (array types) ---------------------------------
	if items, ok := schema["items"].(map[string]any); ok {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(jsonData, &obj); err == nil {
			if itemsJSON, ok := obj["items"]; ok {
				assertRequiredFirst(t, path+"items.", itemsJSON, items)
			}
		}
	}
}

// TestAllTools_RequiredPropertiesFirstInJSON is a regression guard that
// verifies EVERY tool in allToolDefinitions() serializes required
// properties before optional ones in the JSON output — at all nesting
// levels.  If a new tool is added or an existing schema is modified,
// this test will catch ordering violations automatically.
func TestAllTools_RequiredPropertiesFirstInJSON(t *testing.T) {
	for _, td := range allToolDefinitions() {
		t.Run(td.Name, func(t *testing.T) {
			b, err := json.Marshal(td)
			if err != nil {
				t.Fatal(err)
			}

			schema, ok := td.InputSchema.(map[string]any)
			if !ok {
				return
			}

			// Unmarshal to get inputSchema JSON.
			var envelope struct {
				InputSchema json.RawMessage `json:"inputSchema"`
			}
			if err := json.Unmarshal(b, &envelope); err != nil {
				t.Fatal(err)
			}

			assertRequiredFirst(t, "", envelope.InputSchema, schema)
		})
	}
}
