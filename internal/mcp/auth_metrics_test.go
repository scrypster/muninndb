package mcp

// auth_metrics_test.go — #648: observability for MCP auth source.
//
// authFromRequest already discriminates credential class structurally
// (IsAPIKey / IsCapability / neither); these tests pin that the discrimination
// is also surfaced as muninn_mcp_auth_total{source} for each of the four
// authorized branches, and that a DENIED request emits nothing (only
// authenticated traffic is counted — see #648's motivation: confirming zero
// static-token traffic before a retirement).

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/metrics"
)

func TestAuthFromRequest_MetricsSource_APIKey(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{ID: "metrics001", Vault: "v", Mode: auth.ModeFull})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mk_metrics001")

	before := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceAPIKey))
	a := authFromRequest(req, "mdb_static", store, nil)
	if !a.Authorized {
		t.Fatal("expected Authorized=true")
	}
	after := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceAPIKey))
	if after != before+1 {
		t.Fatalf("muninn_mcp_auth_total{source=api_key} = %v, want %v", after, before+1)
	}
}

func TestAuthFromRequest_MetricsSource_Capability(t *testing.T) {
	stub := stubCapStore{cap: auth.Capability{Vault: "v", Mode: auth.ModeFull}}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer cap_metrics001")

	before := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceCapability))
	a := authFromRequest(req, "mdb_static", nil, stub)
	if !a.Authorized {
		t.Fatal("expected Authorized=true")
	}
	after := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceCapability))
	if after != before+1 {
		t.Fatalf("muninn_mcp_auth_total{source=capability} = %v, want %v", after, before+1)
	}
}

func TestAuthFromRequest_MetricsSource_StaticToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mdb_metricstoken")

	before := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceStatic))
	a := authFromRequest(req, "mdb_metricstoken", nil, nil)
	if !a.Authorized {
		t.Fatal("expected Authorized=true")
	}
	after := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceStatic))
	if after != before+1 {
		t.Fatalf("muninn_mcp_auth_total{source=static_token} = %v, want %v", after, before+1)
	}
}

func TestAuthFromRequest_MetricsSource_Open(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)

	before := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceOpen))
	a := authFromRequest(req, "", nil, nil)
	if !a.Authorized {
		t.Fatal("expected Authorized=true (open-server mode)")
	}
	after := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceOpen))
	if after != before+1 {
		t.Fatalf("muninn_mcp_auth_total{source=open} = %v, want %v", after, before+1)
	}
}

// TestAuthFromRequest_MetricsSource_DeniedNotCounted proves a rejected
// credential does not inflate any source counter — only authenticated
// traffic is observable here (the request log / 401 response already carries
// the denial signal).
func TestAuthFromRequest_MetricsSource_DeniedNotCounted(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mdb_wrong")

	beforeStatic := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceStatic))
	beforeOpen := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceOpen))

	a := authFromRequest(req, "mdb_correct", nil, nil)
	if a.Authorized {
		t.Fatal("expected Authorized=false for wrong static token")
	}

	if got := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceStatic)); got != beforeStatic {
		t.Errorf("source=static_token counted a denied request: %v -> %v", beforeStatic, got)
	}
	if got := testutil.ToFloat64(metrics.MCPAuthTotal.WithLabelValues(authSourceOpen)); got != beforeOpen {
		t.Errorf("source=open counted a denied request: %v -> %v", beforeOpen, got)
	}
}
