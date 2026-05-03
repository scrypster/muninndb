package mcp

import (
	"strings"
	"testing"
)

func TestAllToolDefinitionsCount(t *testing.T) {
	tools := allToolDefinitions()
	if len(tools) != 39 {
		t.Errorf("expected 39 tools, got %d", len(tools))
	}
}

func TestAllToolDefinitions_ContainsTreeTools(t *testing.T) {
	tools := allToolDefinitions()
	names := make(map[string]bool, len(tools))
	for _, td := range tools {
		names[td.Name] = true
	}
	for _, want := range []string{"muninn_remember_tree", "muninn_recall_tree", "muninn_add_child"} {
		if !names[want] {
			t.Errorf("missing tool: %s", want)
		}
	}
}

func TestAllToolNamesUnique(t *testing.T) {
	tools := allToolDefinitions()
	seen := make(map[string]bool)
	for _, tool := range tools {
		if seen[tool.Name] {
			t.Errorf("duplicate tool name: %s", tool.Name)
		}
		seen[tool.Name] = true
	}
}

func TestAllToolsHaveVaultParam(t *testing.T) {
	tools := allToolDefinitions()
	for _, tool := range tools {
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Errorf("tool %s: inputSchema is not a map", tool.Name)
			continue
		}
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["vault"]; !ok {
			t.Errorf("tool %s: missing 'vault' parameter", tool.Name)
		}
		// vault is optional (not required) since sessions can pin a vault automatically
		required, _ := schema["required"].([]string)
		for _, r := range required {
			if r == "vault" {
				t.Errorf("tool %s: 'vault' must not be in required list (it is optional via session pin)", tool.Name)
				break
			}
		}
	}
}

func TestExpectedToolNames(t *testing.T) {
	tools := allToolDefinitions()
	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
	}
	expected := []string{
		"muninn_remember", "muninn_remember_batch", "muninn_recall", "muninn_read", "muninn_forget",
		"muninn_link", "muninn_contradictions", "muninn_status",
		"muninn_evolve", "muninn_update", "muninn_consolidate", "muninn_session", "muninn_decide",
		// Epic 18
		"muninn_restore", "muninn_traverse", "muninn_explain",
		"muninn_state", "muninn_list_deleted", "muninn_retry_enrich",
		"muninn_get_enrichment_candidates", "muninn_apply_enrichment",
		// Guide
		"muninn_guide",
		// Hierarchical memory
		"muninn_remember_tree", "muninn_recall_tree", "muninn_add_child",
		// Session context
		"muninn_where_left_off",
		// Entity reverse index
		"muninn_find_by_entity",
		// Entity lifecycle state
		"muninn_entity_state",
		"muninn_entity_state_batch",
		// Entity cluster detection
		"muninn_entity_clusters",
		// Knowledge graph export
		"muninn_export_graph",
		// Entity similarity detection and merge
		"muninn_similar_entities",
		"muninn_merge_entity",
		// Entity timeline
		"muninn_entity_timeline",
		// Enrichment replay
		"muninn_replay_enrichment",
		// Provenance audit trail
		"muninn_provenance",
		// SGD learning loop feedback
		"muninn_feedback",
		// Entity aggregate view
		"muninn_entity",
		"muninn_entities",
	}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("missing expected tool: %s", name)
		}
	}
}

// TestNewToolRequiredFields verifies that each Epic 18 tool's JSON schema has
// the correct required fields declared.
func TestNewToolRequiredFields(t *testing.T) {
	tools := allToolDefinitions()
	byName := make(map[string]ToolDefinition, len(tools))
	for _, td := range tools {
		byName[td.Name] = td
	}

	cases := []struct {
		tool     string
		required []string
	}{
		{"muninn_restore", []string{"id"}},
		{"muninn_traverse", []string{"start_id"}},
		{"muninn_explain", []string{"engram_id", "query"}},
		{"muninn_state", []string{"id", "state"}},
		{"muninn_list_deleted", []string{}},
		{"muninn_retry_enrich", []string{"id"}},
		{"muninn_get_enrichment_candidates", []string{}},
		{"muninn_apply_enrichment", []string{"id", "expected_updated_at"}},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			td, ok := byName[tc.tool]
			if !ok {
				t.Fatalf("tool %q not found in allToolDefinitions()", tc.tool)
			}
			schema, ok := td.InputSchema.(map[string]any)
			if !ok {
				t.Fatalf("inputSchema is not a map for %q", tc.tool)
			}
			required, _ := schema["required"].([]string)
			reqSet := make(map[string]bool, len(required))
			for _, r := range required {
				reqSet[r] = true
			}
			for _, field := range tc.required {
				if !reqSet[field] {
					t.Errorf("field %q not in required list for %s", field, tc.tool)
				}
			}
		})
	}
}

// TestMuninnStateSchemaHasEnum verifies that the muninn_state tool declares the
// 7 valid lifecycle states as an enum on the "state" property.
func TestMuninnStateSchemaHasEnum(t *testing.T) {
	tools := allToolDefinitions()
	var stateTool *ToolDefinition
	for i := range tools {
		if tools[i].Name == "muninn_state" {
			stateTool = &tools[i]
			break
		}
	}
	if stateTool == nil {
		t.Fatal("muninn_state not found in allToolDefinitions()")
	}
	schema, ok := stateTool.InputSchema.(map[string]any)
	if !ok {
		t.Fatal("inputSchema is not a map")
	}
	props, _ := schema["properties"].(map[string]any)
	stateProp, ok := props["state"].(map[string]any)
	if !ok {
		t.Fatal("state property not found or not a map")
	}
	enum, ok := stateProp["enum"].([]string)
	if !ok {
		t.Fatal("state property missing 'enum' array")
	}
	wantStates := map[string]bool{
		"planning": true, "active": true, "paused": true, "blocked": true,
		"completed": true, "cancelled": true, "archived": true,
	}
	if len(enum) != len(wantStates) {
		t.Errorf("expected %d enum values, got %d: %v", len(wantStates), len(enum), enum)
	}
	for _, s := range enum {
		if !wantStates[s] {
			t.Errorf("unexpected enum value: %q", s)
		}
	}
}

// TestNewToolsHaveDescriptions verifies each Epic 18 tool has a non-empty description.
func TestNewToolsHaveDescriptions(t *testing.T) {
	tools := allToolDefinitions()
	newTools := []string{
		"muninn_restore", "muninn_traverse", "muninn_explain",
		"muninn_state", "muninn_list_deleted", "muninn_retry_enrich",
		"muninn_get_enrichment_candidates", "muninn_apply_enrichment",
	}
	byName := make(map[string]ToolDefinition, len(tools))
	for _, td := range tools {
		byName[td.Name] = td
	}
	for _, name := range newTools {
		td, ok := byName[name]
		if !ok {
			t.Errorf("tool %q not found", name)
			continue
		}
		if td.Description == "" {
			t.Errorf("tool %q has empty description", name)
		}
	}
}

func TestMuninnLinkTool_HasAllRelationTypes(t *testing.T) {
	tools := allToolDefinitions()
	var linkTool *ToolDefinition
	for i := range tools {
		if tools[i].Name == "muninn_link" {
			linkTool = &tools[i]
			break
		}
	}
	if linkTool == nil {
		t.Fatal("muninn_link tool not found")
	}

	schema := linkTool.InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)
	relProp := props["relation"].(map[string]any)
	desc := relProp["description"].(string)

	required := []string{
		"supports", "contradicts", "depends_on", "supersedes", "relates_to",
		"is_part_of", "causes", "preceded_by", "followed_by",
		"created_by_person", "belongs_to_project", "references",
		"implements", "blocks", "resolves", "refines",
	}
	for _, r := range required {
		if !strings.Contains(desc, r) {
			t.Errorf("muninn_link relation description missing %q", r)
		}
	}
}

func TestMuninnRecallTool_HasProfileParam(t *testing.T) {
	tools := allToolDefinitions()
	var recallTool *ToolDefinition
	for i := range tools {
		if tools[i].Name == "muninn_recall" {
			recallTool = &tools[i]
			break
		}
	}
	if recallTool == nil {
		t.Fatal("muninn_recall tool not found")
	}

	schema := recallTool.InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)
	if _, ok := props["profile"]; !ok {
		t.Error("muninn_recall tool must have a 'profile' parameter")
	}
}

func TestMuninnRecallTool_ProfileNotInRequired(t *testing.T) {
	tools := allToolDefinitions()
	for _, tool := range tools {
		if tool.Name != "muninn_recall" {
			continue
		}
		schema := tool.InputSchema.(map[string]any)
		required, _ := schema["required"].([]string)
		for _, r := range required {
			if r == "profile" {
				t.Error("'profile' must not be in muninn_recall required list — it is optional")
			}
		}
	}
}

func TestMuninnRememberTool_HasEnrichmentFields(t *testing.T) {
	tools := allToolDefinitions()
	var rememberTool *ToolDefinition
	for i := range tools {
		if tools[i].Name == "muninn_remember" {
			rememberTool = &tools[i]
			break
		}
	}
	if rememberTool == nil {
		t.Fatal("muninn_remember not found")
	}
	schema := rememberTool.InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)

	for _, field := range []string{"summary", "entities", "relationships"} {
		if _, ok := props[field]; !ok {
			t.Errorf("muninn_remember missing enrichment field %q", field)
		}
	}
}

func TestMuninnRememberBatchTool_HasEnrichmentFields(t *testing.T) {
	tools := allToolDefinitions()
	var batchTool *ToolDefinition
	for i := range tools {
		if tools[i].Name == "muninn_remember_batch" {
			batchTool = &tools[i]
			break
		}
	}
	if batchTool == nil {
		t.Fatal("muninn_remember_batch not found")
	}
	schema := batchTool.InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)
	memories := props["memories"].(map[string]any)
	items := memories["items"].(map[string]any)
	itemProps := items["properties"].(map[string]any)

	for _, field := range []string{"summary", "entities", "relationships"} {
		if _, ok := itemProps[field]; !ok {
			t.Errorf("muninn_remember_batch item schema missing enrichment field %q", field)
		}
	}
}

// TestMuninnRememberTree_ChildrenHasItemsWithProperties verifies that the
// children array in muninn_remember_tree has an items key with a properties map,
// ensuring the JSON schema is valid and doesn't trigger "array schema missing items" errors.
func TestMuninnRememberTree_ChildrenHasItemsWithProperties(t *testing.T) {
	tools := allToolDefinitions()
	var treeTool *ToolDefinition
	for i := range tools {
		if tools[i].Name == "muninn_remember_tree" {
			treeTool = &tools[i]
			break
		}
	}
	if treeTool == nil {
		t.Fatal("muninn_remember_tree not found")
	}

	schema := treeTool.InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)
	root := props["root"].(map[string]any)
	rootProps := root["properties"].(map[string]any)
	children := rootProps["children"].(map[string]any)

	// Verify children is an array type
	if children["type"] != "array" {
		t.Fatal("children property must be type array")
	}

	// Verify children has items key
	items, ok := children["items"].(map[string]any)
	if !ok {
		t.Fatal("children array missing 'items' key or items is not a map")
	}

	// Verify items has a properties map
	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatal("children.items missing 'properties' map")
	}

	// Verify the expected properties exist on items
	for _, field := range []string{"concept", "content", "children"} {
		if _, ok := itemProps[field]; !ok {
			t.Errorf("children.items.properties missing field %q", field)
		}
	}

	// Verify nested children also has items with properties (recursive schema)
	nestedChildren, ok := itemProps["children"].(map[string]any)
	if !ok {
		t.Fatal("children.items.properties.children is not a map")
	}
	nestedItems, ok := nestedChildren["items"].(map[string]any)
	if !ok {
		t.Fatal("nested children array missing 'items' key")
	}
	if _, ok := nestedItems["properties"].(map[string]any); !ok {
		t.Fatal("nested children.items missing 'properties' map")
	}
}

func TestMuninnRecallTool_ProfileHasAllFiveOptions(t *testing.T) {
	tools := allToolDefinitions()
	for _, tool := range tools {
		if tool.Name != "muninn_recall" {
			continue
		}
		schema := tool.InputSchema.(map[string]any)
		props := schema["properties"].(map[string]any)
		profileProp, ok := props["profile"].(map[string]any)
		if !ok {
			t.Fatal("profile property is not a map")
		}
		desc, _ := profileProp["description"].(string)
		for _, name := range []string{"default", "causal", "confirmatory", "adversarial", "structural"} {
			if !strings.Contains(desc, name) {
				t.Errorf("profile description missing %q", name)
			}
		}
	}
}
