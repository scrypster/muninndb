package mcp

import (
	"bytes"
	"encoding/json"
	"sort"
)

// OrderedMap is a JSON object that preserves key insertion order during
// marshaling.  Go's map[string]any serializes keys alphabetically;
// OrderedMap lets callers control the output order — which matters for
// JSON Schema "properties" where required fields should appear before
// optional ones.
type OrderedMap struct {
	keys   []string
	values map[string]any
}

// NewOrderedMap returns an empty OrderedMap ready for use.
func NewOrderedMap() *OrderedMap {
	return &OrderedMap{values: make(map[string]any)}
}

// Set adds or updates a key-value pair.  New keys are appended at the
// end; existing keys keep their original position.
func (o *OrderedMap) Set(key string, value any) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// MarshalJSON emits a JSON object with keys in insertion order.
func (o *OrderedMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		v, err := json.Marshal(o.values[key])
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// MarshalJSON on ToolDefinition applies property ordering at serialization
// time only: required fields first (in declared order), then optional
// fields alphabetically.  The in-memory InputSchema (map[string]any) is
// never mutated, so existing code and tests that type-assert on
// map[string]any continue to work.
//
// This fixes JSON Schema consumers that treat property order as
// significant (e.g. Python's inspect.Signature via the MCP SDK).
func (td ToolDefinition) MarshalJSON() ([]byte, error) {
	// Local struct without MarshalJSON avoids infinite recursion.
	type raw struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		InputSchema any    `json:"inputSchema"`
	}
	return json.Marshal(raw{
		Name:        td.Name,
		Description: td.Description,
		InputSchema: orderSchemaProperties(td.InputSchema),
	})
}

// orderSchemaProperties returns a shallow copy of a JSON Schema node
// where every "properties" map[string]any is replaced by an *OrderedMap:
// required fields first (in the order from the "required" array), then
// optional fields alphabetically.  Nested schemas (items, nested
// properties) are handled recursively.  The original schema is NOT
// mutated.
func orderSchemaProperties(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}

	// Shallow copy so we don't mutate the original.
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}

	// Recurse into "items" (array schemas).
	if items, ok := out["items"]; ok {
		out["items"] = orderSchemaProperties(items)
	}

	props, hasProps := out["properties"].(map[string]any)
	if !hasProps {
		return out
	}

	// Collect required names for O(1) lookup.
	var required []string
	if r, ok := out["required"].([]string); ok {
		required = r
	}
	reqSet := make(map[string]bool, len(required))
	for _, r := range required {
		reqSet[r] = true
	}

	om := NewOrderedMap()

	// 1. Required fields, in the order declared in "required".
	for _, r := range required {
		if v, ok := props[r]; ok {
			om.Set(r, orderSchemaProperties(v))
		}
	}

	// 2. Optional fields, sorted alphabetically.
	optional := make([]string, 0, len(props)-len(required))
	for k := range props {
		if !reqSet[k] {
			optional = append(optional, k)
		}
	}
	sort.Strings(optional)
	for _, k := range optional {
		om.Set(k, orderSchemaProperties(props[k]))
	}

	out["properties"] = om
	return out
}
