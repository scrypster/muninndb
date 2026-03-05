package rest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAssociationItem_RestoredAt_OmitEmpty(t *testing.T) {
	item := AssociationItem{
		TargetID: "test-id",
		Weight:   0.5,
	}
	data, _ := json.Marshal(item)
	if strings.Contains(string(data), "restored_at") {
		t.Error("restored_at should be omitted when zero")
	}

	item.RestoredAt = 1709568000
	data, _ = json.Marshal(item)
	if !strings.Contains(string(data), "restored_at") {
		t.Error("restored_at should be present when non-zero")
	}
}
