package mcp

import (
	"strings"
	"testing"
)

// TestRemember_NoVaultArg_HintNamesResolvedVault is the #770 fix.
//
// muninn_remember with no `vault` is accepted and lands in the default vault,
// and the response was a bare {id, concept} that said nothing about where it
// went. Routing is consistent — a recall that also omits `vault` finds it — so
// the failure is the SILENCE: an agent working in vault X that omits the
// parameter once gets ok:true and a fact that is invisible from X.
func TestRemember_NoVaultArg_HintNamesResolvedVault(t *testing.T) {
	srv := newTestServerWith(&fakeEngine{})

	const noVault = `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"concept":"deploy window","content":"deploys go out on Thursdays"}}}`
	got := extractInnerJSON(t, decodeResp(t, postRPC(t, srv, noVault).Body.String()))
	hint, _ := got["hint"].(string)
	if !strings.Contains(hint, `vault "default"`) {
		t.Errorf("hint = %q, want it to name the resolved vault", hint)
	}
	if !strings.Contains(hint, "vault:<name>") {
		t.Errorf("hint = %q, want it to say how to target another vault", hint)
	}

	// An explicit vault is not nagged about — the caller already knows.
	const withVault = `{"jsonrpc":"2.0","method":"tools/call","id":2,"params":{"name":"muninn_remember","arguments":{"vault":"work","concept":"deploy window","content":"deploys go out on Thursdays"}}}`
	got = extractInnerJSON(t, decodeResp(t, postRPC(t, srv, withVault).Body.String()))
	if hint, _ := got["hint"].(string); strings.Contains(hint, "no 'vault' specified") {
		t.Errorf("explicit vault should not produce the routing hint, got %q", hint)
	}
}
