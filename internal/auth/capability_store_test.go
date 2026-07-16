package auth

import (
	"testing"
	"time"
)

func TestCapability_GenerateValidateRoundtrip(t *testing.T) {
	db := openAuthTestDB(t)
	store := NewStore(db)
	exp := time.Now().Add(time.Hour)
	token, cap, err := store.GenerateCapability("wf-x", "agent", ModeFull, "workflow_vault", &exp)
	if err != nil {
		t.Fatalf("GenerateCapability: %v", err)
	}
	if token[:4] != "cap_" {
		t.Errorf("token prefix want cap_, got %q", token[:4])
	}
	got, err := store.ValidateCapability(token)
	if err != nil {
		t.Fatalf("ValidateCapability: %v", err)
	}
	if got.Vault != "wf-x" || got.Mode != ModeFull || got.Origin != "workflow_vault" {
		t.Errorf("resolved cap = %+v", got)
	}
	if cap.ID != got.ID {
		t.Error("ID mismatch between generate and validate")
	}
}

func TestCapability_ExpiredRejected(t *testing.T) {
	db := openAuthTestDB(t)
	store := NewStore(db)
	past := time.Now().Add(-time.Minute)
	token, _, err := store.GenerateCapability("wf-x", "agent", ModeFull, "workflow_vault", &past)
	if err != nil {
		t.Fatalf("GenerateCapability: %v", err)
	}
	if _, err := store.ValidateCapability(token); err == nil {
		t.Error("expired capability should fail validation")
	}
}

func TestCapability_Revoke(t *testing.T) {
	db := openAuthTestDB(t)
	store := NewStore(db)
	exp := time.Now().Add(time.Hour)
	token, cap, _ := store.GenerateCapability("wf-x", "agent", ModeFull, "workflow_vault", &exp)
	if err := store.RevokeCapability("wf-x", cap.ID); err != nil {
		t.Fatalf("RevokeCapability: %v", err)
	}
	if _, err := store.ValidateCapability(token); err == nil {
		t.Error("revoked capability should fail validation")
	}
}

func TestCapability_NilExpiryForbidden(t *testing.T) {
	db := openAuthTestDB(t)
	store := NewStore(db)
	if _, _, err := store.GenerateCapability("wf-x", "agent", ModeFull, "workflow_vault", nil); err == nil {
		t.Error("workflow capabilities must have an expiry (nil ExpiresAt forbidden)")
	}
}
