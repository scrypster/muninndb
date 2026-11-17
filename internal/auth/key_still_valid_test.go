package auth

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

func openKeyStore(tb testing.TB) *Store {
	tb.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		tb.Fatalf("open db: %v", err)
	}
	tb.Cleanup(func() { db.Close() })
	return NewStore(db)
}

// TestAPIKeyStillValid pins what a long-lived session is told when it asks
// whether the key it was opened with still holds.
func TestAPIKeyStillValid(t *testing.T) {
	store := openKeyStore(t)

	validate := func(t *testing.T, mode string, expires *time.Time) (string, APIKey) {
		t.Helper()
		token, _, err := store.GenerateAPIKey("pilot-station", "session", mode, expires)
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		key, err := store.ValidateAPIKey(token)
		if err != nil {
			t.Fatalf("ValidateAPIKey: %v", err)
		}
		return token, key
	}

	t.Run("a live key is still valid, every time it is asked", func(t *testing.T) {
		_, key := validate(t, ModeObserve, nil)
		for i := 0; i < 3; i++ {
			if err := store.APIKeyStillValid(&key); err != nil {
				t.Fatalf("ask %d: %v", i+1, err)
			}
		}
	})

	t.Run("revoked", func(t *testing.T) {
		_, key := validate(t, ModeFull, nil)
		_, other := validate(t, ModeFull, nil)
		if err := store.RevokeAPIKey("pilot-station", key.ID); err != nil {
			t.Fatalf("RevokeAPIKey: %v", err)
		}
		if err := store.APIKeyStillValid(&key); !errors.Is(err, ErrKeyNoLongerValid) {
			t.Errorf("revoked key: err = %v; want ErrKeyNoLongerValid", err)
		}
		if err := store.APIKeyStillValid(&other); err != nil {
			t.Errorf("another key of the same vault was taken down with it: %v", err)
		}
	})

	t.Run("expired since it was validated", func(t *testing.T) {
		expires := time.Now().Add(150 * time.Millisecond)
		_, key := validate(t, ModeWrite, &expires)
		if err := store.APIKeyStillValid(&key); err != nil {
			t.Fatalf("before expiry: %v", err)
		}
		time.Sleep(time.Until(expires) + 50*time.Millisecond)
		if err := store.APIKeyStillValid(&key); !errors.Is(err, ErrKeyNoLongerValid) {
			t.Errorf("after expiry: err = %v; want ErrKeyNoLongerValid", err)
		}
	})

	// The session's copy of the key is what it was authorised as. If the
	// record under the same secret ever stops matching it, the session does
	// not carry on as what it used to be.
	t.Run("the record no longer matches the session's copy", func(t *testing.T) {
		_, key := validate(t, ModeObserve, nil)
		for name, change := range map[string]func(k *APIKey){
			"mode":  func(k *APIKey) { k.Mode = ModeFull },
			"vault": func(k *APIKey) { k.Vault = "customs-ledger" },
			"id":    func(k *APIKey) { k.ID = "someone-else" },
		} {
			session := key
			change(&session)
			if err := store.APIKeyStillValid(&session); !errors.Is(err, ErrKeyNoLongerValid) {
				t.Errorf("session copy differing in %s: err = %v; want ErrKeyNoLongerValid", name, err)
			}
		}
	})

	t.Run("no key, or one that was never validated", func(t *testing.T) {
		if err := store.APIKeyStillValid(nil); !errors.Is(err, ErrKeyNoLongerValid) {
			t.Errorf("nil key: err = %v", err)
		}
		if err := store.APIKeyStillValid(&APIKey{ID: "x", Vault: "pilot-station", Mode: ModeFull}); !errors.Is(err, ErrKeyNoLongerValid) {
			t.Errorf("key with no storage hash: err = %v", err)
		}
	})
}

// BenchmarkAPIKeyStillValid is the cost a keyed MBP connection pays on every
// frame so that a revoked or expired key stops working on connections that are
// already open: one point lookup in the auth store and one small JSON decode.
// The store holds a thousand keys across fifty vaults.
func BenchmarkAPIKeyStillValid(b *testing.B) {
	store := openKeyStore(b)
	var key APIKey
	for i := 0; i < 1000; i++ {
		token, _, err := store.GenerateAPIKey("vault-"+strconv.Itoa(i%50), "k"+strconv.Itoa(i), ModeFull, nil)
		if err != nil {
			b.Fatal(err)
		}
		if i == 500 {
			if key, err = store.ValidateAPIKey(token); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := store.APIKeyStillValid(&key); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetVaultConfig is the same cost for a session with no key: whether
// the vault it was opened on is still public.
func BenchmarkGetVaultConfig(b *testing.B) {
	store := openKeyStore(b)
	for i := 0; i < 50; i++ {
		if err := store.SetVaultConfig(VaultConfig{Name: "vault-" + strconv.Itoa(i), Public: i%2 == 0}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.GetVaultConfig("vault-24"); err != nil {
			b.Fatal(err)
		}
	}
}
