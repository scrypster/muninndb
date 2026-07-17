package mcp

import (
	"strings"
	"testing"
)

func TestResolveVault_SessionPin(t *testing.T) {
	vault, errMsg := resolveVault("project-a", map[string]any{})
	if vault != "project-a" || errMsg != "" {
		t.Fatalf("got vault=%q err=%q", vault, errMsg)
	}
}

func TestResolveVault_ArgMatchesPin(t *testing.T) {
	vault, errMsg := resolveVault("project-a", map[string]any{"vault": "project-a"})
	if vault != "project-a" || errMsg != "" {
		t.Fatalf("got vault=%q err=%q", vault, errMsg)
	}
}

func TestResolveVault_Mismatch(t *testing.T) {
	_, errMsg := resolveVault("project-a", map[string]any{"vault": "project-b"})
	if errMsg == "" {
		t.Fatal("expected vault mismatch error")
	}
	if !strings.Contains(errMsg, "vault mismatch") {
		t.Fatalf("error message should mention vault mismatch, got: %q", errMsg)
	}
	// Security: pinned vault name should NOT be leaked in the error.
	if strings.Contains(errMsg, "project-a") {
		t.Fatalf("error message should not leak pinned vault name, got: %q", errMsg)
	}
}

func TestResolveVault_NonStringVault(t *testing.T) {
	// A non-string vault arg is now rejected (fail-closed) instead of falling back to "default".
	_, errMsg := resolveVault("", map[string]any{"vault": 42})
	if errMsg == "" {
		t.Fatal("expected error for non-string vault arg")
	}
	if !strings.Contains(errMsg, "invalid vault name") {
		t.Fatalf("expected 'invalid vault name' error, got: %q", errMsg)
	}
}

func TestResolveVault_NoSessionWithArg(t *testing.T) {
	vault, errMsg := resolveVault("", map[string]any{"vault": "explicit"})
	if vault != "explicit" || errMsg != "" {
		t.Fatalf("got vault=%q err=%q", vault, errMsg)
	}
}

func TestResolveVault_DefaultFallback(t *testing.T) {
	vault, errMsg := resolveVault("", map[string]any{})
	if vault != "default" || errMsg != "" {
		t.Fatalf("got vault=%q err=%q", vault, errMsg)
	}
}
