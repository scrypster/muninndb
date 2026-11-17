package auth

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// TestStreamStillAuthorized pins what a long-lived stream is told when it asks
// whether what authorised it at open still holds, for each of the three ways a
// stream can have been authorised.
func TestStreamStillAuthorized(t *testing.T) {
	const locked, public = "pilot-station", "harbour-notices"
	store := openKeyStore(t)
	for vault, open := range map[string]bool{locked: false, public: true} {
		if err := store.SetVaultConfig(VaultConfig{Name: vault, Public: open}); err != nil {
			t.Fatalf("configure %s: %v", vault, err)
		}
	}
	keyed := func(t *testing.T, expires *time.Time) (context.Context, APIKey) {
		t.Helper()
		token, _, err := store.GenerateAPIKey(locked, "stream", ModeObserve, expires)
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		key, err := store.ValidateAPIKey(token)
		if err != nil {
			t.Fatalf("ValidateAPIKey: %v", err)
		}
		ctx := context.WithValue(context.Background(), ContextVault, key.Vault)
		ctx = context.WithValue(ctx, ContextAPIKey, &key)
		return ctx, key
	}
	keyless := func(vault string) context.Context {
		return context.WithValue(context.Background(), ContextVault, vault)
	}

	t.Run("a live key", func(t *testing.T) {
		ctx, _ := keyed(t, nil)
		if err := store.StreamStillAuthorized(ctx, locked); err != nil {
			t.Errorf("err = %v; want nil", err)
		}
	})
	t.Run("a revoked key", func(t *testing.T) {
		ctx, key := keyed(t, nil)
		if err := store.RevokeAPIKey(locked, key.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.StreamStillAuthorized(ctx, locked); !errors.Is(err, ErrKeyNoLongerValid) {
			t.Errorf("err = %v; want ErrKeyNoLongerValid", err)
		}
	})
	t.Run("an expired key", func(t *testing.T) {
		expires := time.Now().Add(50 * time.Millisecond)
		ctx, _ := keyed(t, &expires)
		time.Sleep(time.Until(expires) + 20*time.Millisecond)
		if err := store.StreamStillAuthorized(ctx, locked); !errors.Is(err, ErrKeyNoLongerValid) {
			t.Errorf("err = %v; want ErrKeyNoLongerValid", err)
		}
	})
	t.Run("a key is asked about, not the vault: a key on a vault that is public is still judged by the key", func(t *testing.T) {
		token, k, err := store.GenerateAPIKey(public, "stream", ModeObserve, nil)
		if err != nil {
			t.Fatal(err)
		}
		key, err := store.ValidateAPIKey(token)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(keyless(public), ContextAPIKey, &key)
		if err := store.RevokeAPIKey(public, k.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.StreamStillAuthorized(ctx, public); !errors.Is(err, ErrKeyNoLongerValid) {
			t.Errorf("err = %v; want ErrKeyNoLongerValid", err)
		}
	})
	t.Run("no key, public vault", func(t *testing.T) {
		if err := store.StreamStillAuthorized(keyless(public), public); err != nil {
			t.Errorf("err = %v; want nil", err)
		}
	})
	t.Run("no key, locked vault", func(t *testing.T) {
		if err := store.StreamStillAuthorized(keyless(locked), locked); !errors.Is(err, ErrVaultNoLongerPublic) {
			t.Errorf("err = %v; want ErrVaultNoLongerPublic", err)
		}
	})
	t.Run("no key, vault with no configuration", func(t *testing.T) {
		if err := store.StreamStillAuthorized(keyless("tug-roster"), "tug-roster"); !errors.Is(err, ErrVaultNoLongerPublic) {
			t.Errorf("err = %v; want ErrVaultNoLongerPublic", err)
		}
	})
	t.Run("no key: the vault asked about is the one the stream is on, not the one in the context", func(t *testing.T) {
		// gRPC authorises a stream with no key against "default" before its
		// first message names the vault; the stream then runs on that vault.
		if err := store.StreamStillAuthorized(keyless(public), locked); !errors.Is(err, ErrVaultNoLongerPublic) {
			t.Errorf("err = %v; want ErrVaultNoLongerPublic", err)
		}
	})
	t.Run("an admin session", func(t *testing.T) {
		ctx := context.WithValue(keyless(locked), ContextAdminSession, true)
		if err := store.StreamStillAuthorized(ctx, locked); err != nil {
			t.Errorf("err = %v; want nil", err)
		}
	})
}

// BenchmarkStreamStillAuthorized is what one push on an open subscription
// stream costs in authorisation: one read of the key's record (keyed) or of the
// vault's configuration (no key).
func BenchmarkStreamStillAuthorized(b *testing.B) {
	store := openKeyStore(b)
	var key APIKey
	for i := 0; i < 1000; i++ {
		vault := "vault-" + strconv.Itoa(i%50)
		if i < 50 {
			if err := store.SetVaultConfig(VaultConfig{Name: vault, Public: i%2 == 0}); err != nil {
				b.Fatal(err)
			}
		}
		token, _, err := store.GenerateAPIKey(vault, "k"+strconv.Itoa(i), ModeFull, nil)
		if err != nil {
			b.Fatal(err)
		}
		if i == 500 {
			if key, err = store.ValidateAPIKey(token); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.Run("keyed", func(b *testing.B) {
		ctx := context.WithValue(context.Background(), ContextAPIKey, &key)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := store.StreamStillAuthorized(ctx, key.Vault); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("no key", func(b *testing.B) {
		ctx := context.WithValue(context.Background(), ContextVault, "vault-24")
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := store.StreamStillAuthorized(ctx, "vault-24"); err != nil {
				b.Fatal(err)
			}
		}
	})
}
