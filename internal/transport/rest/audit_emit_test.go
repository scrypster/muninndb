package rest

import (
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/audit"
)

func TestEmitAudit_NilLogger(t *testing.T) {
	store := newTestAuthStore(t)
	s := newTestServer(t, store)
	// Must not panic with nil auditLog.
	req := httptest.NewRequest("DELETE", "/api/admin/vaults/prod", nil)
	s.EmitAudit(req, "vault.delete", "vault", "prod", "ok", nil)
}

func TestEmitAudit_LogsEvent(t *testing.T) {
	var mu sync.Mutex
	var got []audit.AuditEvent
	sink := audit.SinkFunc(func(e audit.AuditEvent) error {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		return nil
	})
	l := audit.New(audit.Config{BufferSize: 16}, sink)

	store := newTestAuthStore(t)
	s := newTestServer(t, store)
	s.SetAuditLogger(l)

	req := httptest.NewRequest("DELETE", "/api/admin/vaults/prod", nil)
	s.EmitAudit(req, "vault.delete", "vault", "prod", "ok", map[string]string{"reason": "test"})

	_ = l.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].Action != "vault.delete" {
		t.Errorf("want vault.delete, got %s", got[0].Action)
	}
	if got[0].TargetID != "prod" {
		t.Errorf("want prod, got %s", got[0].TargetID)
	}
	if got[0].Result != "ok" {
		t.Errorf("want ok, got %s", got[0].Result)
	}
	if got[0].EventID == "" {
		t.Error("EventID must be populated")
	}
}
