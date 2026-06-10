package main

import (
	"strings"
	"testing"
)

// Issue #481: when a service is reachable but its TLS certificate fails
// verification, the daemon is UP — only trust failed. The status output must
// not report "stopped / muninn start" in that case, and a mix of cert-failed
// and genuinely-dead services must surface BOTH the trust guidance and the
// restart hint.

// TestOverallState_AllCertFail_IsDegraded: a cert-verification failure means
// the server answered the TLS handshake, so an all-cert-fail deployment is
// degraded (trust problem), not stopped.
func TestOverallState_AllCertFail_IsDegraded(t *testing.T) {
	svcs := []serviceStatus{
		{name: "database", up: false, certErr: true},
		{name: "mcp", up: false, certErr: true},
		{name: "web ui", up: false, certErr: true},
	}
	if got := overallState(svcs); got != stateDegraded {
		t.Errorf("all cert-fail: got %v, want stateDegraded", got)
	}
}

// TestOverallState_AllGenuinelyDown_IsStopped: with no cert errors and nothing
// reachable, the daemon really is down.
func TestOverallState_AllGenuinelyDown_IsStopped(t *testing.T) {
	svcs := []serviceStatus{
		{name: "database", up: false},
		{name: "mcp", up: false},
		{name: "web ui", up: false},
	}
	if got := overallState(svcs); got != stateStopped {
		t.Errorf("all down: got %v, want stateStopped", got)
	}
}

// TestOverallState_CertFailPlusUp_IsDegraded: any cert failure among otherwise
// up services is degraded, not running.
func TestOverallState_CertFailPlusUp_IsDegraded(t *testing.T) {
	svcs := []serviceStatus{
		{name: "database", up: true},
		{name: "mcp", up: false, certErr: true},
		{name: "web ui", up: true},
	}
	if got := overallState(svcs); got != stateDegraded {
		t.Errorf("cert-fail + up: got %v, want stateDegraded", got)
	}
}

// TestPrintStatus_AllCertFail_ShowsTrustGuidanceNotStart: the all-cert-fail
// display must explain the trust problem and must NOT tell the user to start a
// server that is already running.
func TestPrintStatus_AllCertFail_ShowsTrustGuidanceNotStart(t *testing.T) {
	restore := probeServicesFn
	probeServicesFn = func() []serviceStatus {
		return []serviceStatus{
			{name: "database", port: 8475, up: false, certErr: true},
			{name: "mcp", port: 8750, up: false, certErr: true},
			{name: "web ui", port: 8476, up: false, certErr: true},
		}
	}
	defer func() { probeServicesFn = restore }()

	out := captureStdout(func() { printStatusDisplay(false) })

	if !strings.Contains(out, "certificate isn't trusted") {
		t.Errorf("expected TLS trust guidance, got:\n%s", out)
	}
	if strings.Contains(out, "muninn start") {
		t.Errorf("must not suggest 'muninn start' when the server is up (cert-only failure), got:\n%s", out)
	}
}

// TestPrintStatus_MixedCertAndDown_ShowsBothGuidances: when one service is
// cert-failed (server up) and another is genuinely down, the output must
// surface both the trust guidance AND the restart hint.
func TestPrintStatus_MixedCertAndDown_ShowsBothGuidances(t *testing.T) {
	restore := probeServicesFn
	probeServicesFn = func() []serviceStatus {
		return []serviceStatus{
			{name: "database", port: 8475, up: true},
			{name: "mcp", port: 8750, up: false, certErr: true},
			{name: "web ui", port: 8476, up: false}, // genuinely down
		}
	}
	defer func() { probeServicesFn = restore }()

	out := captureStdout(func() { printStatusDisplay(false) })

	if !strings.Contains(out, "certificate isn't trusted") {
		t.Errorf("expected TLS trust guidance for the cert-failed service, got:\n%s", out)
	}
	if !strings.Contains(out, "muninn restart") {
		t.Errorf("expected 'muninn restart' hint for the genuinely-down service, got:\n%s", out)
	}
}
