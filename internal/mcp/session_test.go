package mcp

import (
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionStore_CreateAndGet(t *testing.T) {
	now := time.Now()
	store := newSessionStore(8, func() time.Time { return now })
	defer store.Close()

	tokenHash := sha256.Sum256([]byte("mytoken"))
	id, err := store.Create("myvault", tokenHash)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("empty session ID")
	}
	if len(id) != 64 { // 32 bytes hex = 64 chars
		t.Fatalf("session ID wrong length: %d", len(id))
	}

	sess, ok := store.Get(id)
	if !ok {
		t.Fatal("expected session to exist")
	}
	if sess.vault != "myvault" {
		t.Fatalf("vault: got %q, want %q", sess.vault, "myvault")
	}
	if sess.tokenHash != tokenHash {
		t.Fatal("tokenHash mismatch")
	}
	if sess.initialized.Load() {
		t.Fatal("session should not be initialized yet")
	}
}

func TestSessionStore_MarkInitialized(t *testing.T) {
	store := newSessionStore(8, time.Now)
	defer store.Close()

	tokenHash := sha256.Sum256([]byte("t"))
	id, _ := store.Create("v", tokenHash)

	if err := store.MarkInitialized(id); err != nil {
		t.Fatalf("MarkInitialized: %v", err)
	}
	sess, _ := store.Get(id)
	if !sess.initialized.Load() {
		t.Fatal("expected initialized == true")
	}
}

func TestSessionStore_MarkInitialized_NotFound(t *testing.T) {
	store := newSessionStore(8, time.Now)
	defer store.Close()

	err := store.MarkInitialized("bogus-session-id-that-does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown session ID, got nil")
	}
}

func TestSessionStore_Cap(t *testing.T) {
	store := newSessionStore(2, time.Now)
	defer store.Close()

	h := sha256.Sum256([]byte("t"))
	store.Create("v", h)
	store.Create("v", h)
	_, err := store.Create("v", h) // should fail
	if !errors.Is(err, ErrSessionCapReached) {
		t.Fatalf("expected ErrSessionCapReached, got %v", err)
	}
}

func TestSessionStore_TTLExpiry(t *testing.T) {
	var mu sync.Mutex
	tick := time.Now()
	store := newSessionStore(8, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return tick
	})
	defer store.Close()

	h := sha256.Sum256([]byte("t"))
	id, _ := store.Create("v", h)

	// advance clock past TTL
	mu.Lock()
	tick = tick.Add(25 * time.Hour)
	mu.Unlock()

	store.(*concreteSessionStore).sweep()

	_, ok := store.Get(id)
	if ok {
		t.Fatal("expected session to be expired and removed")
	}
}

func TestSessionStore_ByVault(t *testing.T) {
	store := newSessionStore(8, time.Now)
	defer store.Close()

	h := sha256.Sum256([]byte("t"))
	store.Create("vault-a", h)
	store.Create("vault-a", h)
	store.Create("vault-b", h)

	sessions := store.ByVault("vault-a")
	if len(sessions) != 2 {
		t.Fatalf("ByVault: got %d, want 2", len(sessions))
	}
	bSessions := store.ByVault("vault-b")
	if len(bSessions) != 1 {
		t.Fatalf("ByVault vault-b: got %d, want 1", len(bSessions))
	}
}

func TestSessionStore_DroppedCount(t *testing.T) {
	store := newSessionStore(8, time.Now)
	defer store.Close()

	h := sha256.Sum256([]byte("t"))
	id, _ := store.Create("v", h)
	sess, _ := store.Get(id)
	sess.droppedEvents.Add(3)

	if store.DroppedCount(id) != 3 {
		t.Fatalf("expected 3 dropped, got %d", store.DroppedCount(id))
	}
}

func TestSessionStore_SweepClosesPushCh(t *testing.T) {
	var mu sync.Mutex
	tick := time.Now()
	store := newSessionStore(8, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return tick
	})
	defer store.Close()

	h := sha256.Sum256([]byte("t"))
	id, _ := store.Create("v", h)
	sess, _ := store.Get(id)

	// advance clock and sweep
	mu.Lock()
	tick = tick.Add(25 * time.Hour)
	mu.Unlock()

	store.(*concreteSessionStore).sweep()

	// pushCh should be closed — receive should return zero value immediately
	select {
	case _, ok := <-sess.pushCh:
		if ok {
			t.Fatal("expected closed channel, got value")
		}
	default:
		t.Fatal("expected closed channel to be readable immediately")
	}
}

func TestSessionStore_Touch(t *testing.T) {
	var mu sync.Mutex
	tick := time.Now()
	store := newSessionStore(8, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return tick
	})
	defer store.Close()

	h := sha256.Sum256([]byte("t"))
	id, _ := store.Create("v", h)

	sess, _ := store.Get(id)
	beforeTouch := sess.lastUsed

	// advance clock
	mu.Lock()
	tick = tick.Add(1 * time.Hour)
	mu.Unlock()

	store.Touch(id)

	sess, _ = store.Get(id)
	if !sess.lastUsed.After(beforeTouch) {
		t.Fatalf("expected lastUsed to advance after Touch; before=%v after=%v", beforeTouch, sess.lastUsed)
	}
}

func TestSessionStore_DoubleClose(t *testing.T) {
	store := newSessionStore(8, time.Now)
	// Should not panic on second Close call
	store.Close()
	store.Close()
}

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
