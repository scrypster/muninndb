package enrich

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/scrypster/muninndb/internal/plugin"
)

// knownEntityTypes mirrors the entity types recognised by the UI colour map
// in web/static/js/app.js:getEntityTypeColor. Extend both together.
var knownEntityTypes = map[string]bool{
	"person":       true,
	"organization": true,
	"project":      true,
	"tool":         true,
	"framework":    true,
	"language":     true,
	"database":     true,
	"service":      true,
	"technology":   true,
	"location":     true,
	"concept":      true,
	"product":      true,
	"event":        true,
	"other":        true,
}

// extractJSON finds and returns the first valid JSON structure in a string.
// Handles markdown code fences and trailing text.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)

	// Remove markdown code fences if present
	if strings.Contains(s, "```json") {
		start := strings.Index(s, "```json")
		end := strings.Index(s[start+7:], "```")
		if end != -1 {
			s = s[start+7 : start+7+end]
			s = strings.TrimSpace(s)
		}
	} else if strings.Contains(s, "```") {
		start := strings.Index(s, "```")
		end := strings.Index(s[start+3:], "```")
		if end != -1 {
			s = s[start+3 : start+3+end]
			s = strings.TrimSpace(s)
		}
	}

	// Find first [ or {
	start := strings.IndexAny(s, "[{")
	if start < 0 {
		return s
	}

	// Walk forward with a bracket-depth counter to find the end of the first
	// complete JSON object or array. A backwards scan would incorrectly grab
	// both objects when a model (e.g. llama3.2) repeats its output. The depth
	// walk also correctly skips brackets inside quoted strings.
	open := s[start]
	var close byte
	if open == '{' {
		close = '}'
	} else {
		close = ']'
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == open {
			depth++
		} else if c == close {
			depth--
			if depth == 0 {
				return strings.TrimSpace(s[start : i+1])
			}
		}
	}

	return s[start:]
}

// ParseEntityResponse parses the JSON response from the entity extraction call.
func ParseEntityResponse(raw string) ([]plugin.ExtractedEntity, error) {
	raw = strings.TrimSpace(raw)
	jsonStr := extractJSON(raw)

	if rawEntities, ok, err := extractTopLevelField(jsonStr, "entities"); err == nil && ok {
		if isJSONNull(rawEntities) {
			return nil, nil
		}
		var entities []plugin.ExtractedEntity
		if err := json.Unmarshal(rawEntities, &entities); err != nil {
			return nil, fmt.Errorf("invalid entity response JSON: %s", truncateForError(jsonStr))
		}
		return validateAndDedupeEntities(entities), nil
	}

	// Try to parse as direct array
	var entities []plugin.ExtractedEntity
	if err := json.Unmarshal([]byte(jsonStr), &entities); err == nil {
		return validateAndDedupeEntities(entities), nil
	}

	return nil, fmt.Errorf("invalid entity response JSON: %s", truncateForError(jsonStr))
}

// ParseRelationshipResponse parses the JSON response from the relationship extraction call.
func ParseRelationshipResponse(raw string) ([]plugin.ExtractedRelation, error) {
	raw = strings.TrimSpace(raw)
	jsonStr := extractJSON(raw)

	if rawRelationships, ok, err := extractTopLevelField(jsonStr, "relationships"); err == nil && ok {
		if isJSONNull(rawRelationships) {
			return nil, nil
		}
		var wrapper []struct {
			From   string  `json:"from"`
			To     string  `json:"to"`
			Type   string  `json:"type"`
			Weight float32 `json:"weight"`
		}
		if err := json.Unmarshal(rawRelationships, &wrapper); err != nil {
			return nil, fmt.Errorf("invalid relationship response JSON: %s", truncateForError(jsonStr))
		}
		var result []plugin.ExtractedRelation
		for _, rel := range wrapper {
			result = append(result, plugin.ExtractedRelation{
				FromEntity: rel.From,
				ToEntity:   rel.To,
				RelType:    rel.Type,
				Weight:     rel.Weight,
			})
		}
		return validateRelationships(result), nil
	}

	// Try to parse as direct array
	var rawRels []struct {
		From   string  `json:"from"`
		To     string  `json:"to"`
		Type   string  `json:"type"`
		Weight float32 `json:"weight"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &rawRels); err == nil {
		var result []plugin.ExtractedRelation
		for _, rel := range rawRels {
			result = append(result, plugin.ExtractedRelation{
				FromEntity: rel.From,
				ToEntity:   rel.To,
				RelType:    rel.Type,
				Weight:     rel.Weight,
			})
		}
		return validateRelationships(result), nil
	}

	return nil, fmt.Errorf("invalid relationship response JSON: %s", truncateForError(jsonStr))
}

func extractTopLevelField(jsonStr, field string) (json.RawMessage, bool, error) {
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonStr), &wrapper); err != nil {
		return nil, false, err
	}
	value, ok := wrapper[field]
	return value, ok, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// ParseClassificationResponse parses the JSON response from the classification call.
func ParseClassificationResponse(raw string) (memType, typeLabel, category, subcategory string, tags []string, err error) {
	raw = strings.TrimSpace(raw)
	jsonStr := extractJSON(raw)

	var result struct {
		MemoryType  string   `json:"memory_type"`
		TypeLabel   string   `json:"type_label"`
		Category    string   `json:"category"`
		Subcategory string   `json:"subcategory"`
		Tags        []string `json:"tags"`
	}

	err = json.Unmarshal([]byte(jsonStr), &result)
	if err != nil {
		return "", "", "", "", nil, fmt.Errorf("invalid classification response JSON: %s", truncateForError(jsonStr))
	}
	if result.MemoryType == "" && result.TypeLabel == "" && result.Category == "" && result.Subcategory == "" && len(result.Tags) == 0 {
		return "", "", "", "", nil, fmt.Errorf("classification response was empty")
	}

	return result.MemoryType, result.TypeLabel, result.Category, result.Subcategory, result.Tags, nil
}

// ParseSummarizeResponse parses the JSON response from the summarization call.
func ParseSummarizeResponse(raw string) (summary string, keyPoints []string, err error) {
	raw = strings.TrimSpace(raw)
	jsonStr := extractJSON(raw)

	var result struct {
		Summary   string   `json:"summary"`
		KeyPoints []string `json:"key_points"`
	}

	err = json.Unmarshal([]byte(jsonStr), &result)
	if err != nil {
		return "", nil, fmt.Errorf("invalid summarize response JSON: %s", truncateForError(jsonStr))
	}
	if result.Summary == "" && len(result.KeyPoints) == 0 {
		return "", nil, fmt.Errorf("summarize response was empty")
	}

	return result.Summary, result.KeyPoints, nil
}

func truncateForError(s string) string {
	const maxLen = 160
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// validateAndDedupeEntities validates entity fields and removes duplicates (keeping highest confidence).
func validateAndDedupeEntities(entities []plugin.ExtractedEntity) []plugin.ExtractedEntity {
	seen := make(map[string]plugin.ExtractedEntity)

	for _, e := range entities {
		e.Name = strings.TrimSpace(e.Name)
		e.Type = strings.TrimSpace(e.Type)

		// Skip empty names
		if e.Name == "" {
			continue
		}

		// Validate and normalize type
		e.Type = normalizeEntityType(e.Type)

		// Clamp confidence to [0.0, 1.0]
		if e.Confidence < 0.0 {
			e.Confidence = 0.0
		} else if e.Confidence > 1.0 {
			e.Confidence = 1.0
		}

		// Keep highest confidence for duplicates
		if existing, ok := seen[e.Name]; ok {
			if e.Confidence > existing.Confidence {
				seen[e.Name] = e
			}
		} else {
			seen[e.Name] = e
		}
	}

	result := make([]plugin.ExtractedEntity, 0, len(seen))
	for _, e := range seen {
		result = append(result, e)
	}

	return result
}

// normalizeEntityType normalizes entity type strings to lowercase and
// validates against the known types recognised by the UI colour map.
// Known types are returned as-is after normalisation. Unknown types are
// returned as their normalised string rather than being silently coerced
// to "service", which would corrupt semantic information and cause the
// graph UI to display incorrect colours for any type not in the original
// eight-item allowlist (e.g. "technology", "location", "concept", "event").
func normalizeEntityType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))

	if knownEntityTypes[t] || t == "" {
		return t
	}

	// Pass through unrecognised types rather than coercing to "service".
	// This preserves the LLM's semantic intent and avoids silent data
	// corruption when new types are added to the UI before the allowlist
	// is updated.
	return t
}

// validateRelationships validates relationship fields.
func validateRelationships(rels []plugin.ExtractedRelation) []plugin.ExtractedRelation {
	result := make([]plugin.ExtractedRelation, 0, len(rels))

	for _, r := range rels {
		r.FromEntity = strings.TrimSpace(r.FromEntity)
		r.ToEntity = strings.TrimSpace(r.ToEntity)
		r.RelType = strings.TrimSpace(r.RelType)

		// Skip if from or to is empty
		if r.FromEntity == "" || r.ToEntity == "" {
			continue
		}

		// Clamp weight to [0.0, 1.0]
		if r.Weight < 0.0 {
			r.Weight = 0.0
		} else if r.Weight > 1.0 {
			r.Weight = 1.0
		}

		result = append(result, r)
	}

	return result
}
