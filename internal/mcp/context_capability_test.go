package mcp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
)

// stubCapStore satisfies capabilityValidator without a live Pebble store.
type stubCapStore struct {
	cap auth.Capability
	err error
}

func (s stubCapStore) ValidateCapability(token string) (auth.Capability, error) {
	return s.cap, s.err
}

func TestAuthFromRequest_CapToken(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	stub := stubCapStore{cap: auth.Capability{Vault: "wf-x", Mode: auth.ModeFull, ExpiresAt: &exp}}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer cap_abc")
	a := authFromRequest(req, "", nil, stub)
	if !a.Authorized || !a.IsCapability || a.IsAPIKey || a.Vault != "wf-x" || a.Mode != auth.ModeFull {
		t.Errorf("cap auth resolved wrong: %+v", a)
	}
}

func TestAuthFromRequest_InvalidCapFailClosed(t *testing.T) {
	stub := stubCapStore{err: errors.New("nope")}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer cap_abc")
	a := authFromRequest(req, "", nil, stub)
	if a.Authorized {
		t.Error("invalid cap_ token must not fall through to open-server mode")
	}
}
