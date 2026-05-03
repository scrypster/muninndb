package mcp

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestMemoryAnnotations_JSONOmitEmpty(t *testing.T) {
	// MemoryAnnotations with only stale/stale_days set — omitempty fields absent
	ann := MemoryAnnotations{Stale: false, StaleDays: 5.2}
	b, err := json.Marshal(ann)
	if err != nil {
		t.Fatalf("marshal MemoryAnnotations: %v", err)
	}
	if bytes.Contains(b, []byte("conflicts_with")) {
		t.Errorf("conflicts_with should be omitted when nil: %s", b)
	}
	if bytes.Contains(b, []byte("superseded_by")) {
		t.Errorf("superseded_by should be omitted when empty: %s", b)
	}
	if bytes.Contains(b, []byte("last_verified")) {
		t.Errorf("last_verified should be omitted when empty: %s", b)
	}

	// Memory with non-nil Annotations — annotations key present
	m := Memory{Annotations: &ann}
	b2, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal Memory with annotations: %v", err)
	}
	if !bytes.Contains(b2, []byte(`"annotations"`)) {
		t.Errorf("annotations should be present when non-nil: %s", b2)
	}

	// Memory with nil Annotations — annotations key absent
	m2 := Memory{}
	b3, err := json.Marshal(m2)
	if err != nil {
		t.Fatalf("marshal Memory without annotations: %v", err)
	}
	if bytes.Contains(b3, []byte(`"annotations"`)) {
		t.Errorf("annotations should be absent when nil: %s", b3)
	}
}
