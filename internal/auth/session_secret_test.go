package auth_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
)

// forgeSessionToken builds a session token signed with key, the way anyone who
// knows the key can. With an empty key that is everyone.
func forgeSessionToken(key []byte) string {
	payload := "root|" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// TestBootstrap_BlankSecretFileIsReplaced pins that Bootstrap never returns a
// blank session secret. A zero-length auth_secret (a first-run write that failed
// part-way, a truncated restore, a placeholder created by provisioning) used to
// be read back as a valid secret, and a zero-length secret switches admin
// authentication off in the REST and UI servers. A file of only whitespace or
// only NUL bytes (what some filesystems leave after a crash) was honored as a
// key anyone can guess, so a session cookie could be forged under it.
func TestBootstrap_BlankSecretFileIsReplaced(t *testing.T) {
	for name, blank := range map[string][]byte{
		"zero bytes":              nil,
		"single newline":          []byte("\n"),
		"whitespace only":         []byte(" \t\r\n"),
		"32 NUL bytes":            make([]byte, 32),
		"NUL bytes and a newline": append(make([]byte, 16), '\n'),
	} {
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			secretPath := filepath.Join(t.TempDir(), "auth_secret")
			if err := os.WriteFile(secretPath, blank, 0600); err != nil {
				t.Fatalf("write blank secret file: %v", err)
			}

			secret, err := auth.Bootstrap(store, secretPath)
			if err != nil {
				t.Fatalf("Bootstrap on a blank secret file: %v", err)
			}
			if len(secret) != 32 {
				t.Fatalf("Bootstrap returned a %d-byte secret for a blank file; want a freshly generated 32-byte secret", len(secret))
			}
			// A 32-byte blank file has the right length without being replaced,
			// so length alone proves nothing: the bytes must be new.
			if bytes.Equal(secret, blank) {
				t.Fatal("Bootstrap returned the blank file's own bytes as the secret; want it replaced")
			}
			if len(bytes.Trim(secret, "\x00 \t\r\n")) == 0 {
				t.Fatal("Bootstrap returned a secret made only of NUL and whitespace bytes")
			}
			onDisk, err := os.ReadFile(secretPath)
			if err != nil {
				t.Fatalf("read secret file: %v", err)
			}
			if !bytes.Equal(onDisk, secret) {
				t.Fatal("the secret on disk is not the secret Bootstrap returned; the next start would use a different one")
			}

			again, err := auth.Bootstrap(store, secretPath)
			if err != nil {
				t.Fatalf("second Bootstrap: %v", err)
			}
			if !bytes.Equal(again, secret) {
				t.Fatal("second Bootstrap returned a different secret; the replacement was not persisted")
			}
		})
	}
}

// TestBootstrap_UnreadableSecretIsNotReplaced pins that a secret file which
// exists but cannot be read is an error, not a reason to mint a new secret over
// it. Only a missing file means "first run".
func TestBootstrap_UnreadableSecretIsNotReplaced(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permission bits and a non-root user")
	}
	store := newTestStore(t)
	secretPath := filepath.Join(t.TempDir(), "auth_secret")
	original := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(secretPath, original, 0600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	// Write-only: reading fails, writing would succeed.
	if err := os.Chmod(secretPath, 0200); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := auth.Bootstrap(store, secretPath)

	if chmodErr := os.Chmod(secretPath, 0600); chmodErr != nil {
		t.Fatalf("chmod back: %v", chmodErr)
	}
	onDisk, readErr := os.ReadFile(secretPath)
	if readErr != nil {
		t.Fatalf("read secret file: %v", readErr)
	}
	if !bytes.Equal(onDisk, original) {
		t.Fatal("Bootstrap replaced a secret file it could not read")
	}
	if err == nil {
		t.Fatal("Bootstrap succeeded with a secret file it could not read; want an error")
	}
}

// TestBootstrap_FirstRunWritesOnlyTheSecretFile checks what a successful first
// run leaves behind: exactly one file, owner-only. It does not test the
// failure behaviour of the write; TestWriteFileAtomic_FailedWriteLeavesTargetUntouched does.
func TestBootstrap_FirstRunWritesOnlyTheSecretFile(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "auth_secret")

	if _, err := auth.Bootstrap(store, secretPath); err != nil {
		t.Fatalf("Bootstrap first run: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "auth_secret" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v after first run; want only auth_secret", names)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(secretPath)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("auth_secret mode is %o; want 600", perm)
		}
	}
}

// TestAdminAPIMiddleware_EmptySecretAcceptsNoToken pins that a token signed
// with an empty key never authenticates. Anyone can compute an HMAC under an
// empty key, so such a token proves nothing about who made it.
func TestAdminAPIMiddleware_EmptySecretAcceptsNoToken(t *testing.T) {
	store := newTestStore(t)
	for name, secret := range map[string][]byte{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			called := false
			handler := store.AdminAPIMiddleware(secret, func(w http.ResponseWriter, r *http.Request) {
				called = true
			})
			req := httptest.NewRequest("GET", "/api/admin/keys", nil)
			req.AddCookie(&http.Cookie{Name: "muninn_session", Value: forgeSessionToken(nil)})
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if called {
				t.Fatal("the admin handler ran for a token signed with an empty key")
			}
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status %d; want 401", w.Code)
			}
		})
	}
}

// TestVaultAuthWithAdminBypass_EmptySecretGrantsNoBypass pins the same property
// on the vault routes: a forged cookie must not unlock a locked vault.
func TestVaultAuthWithAdminBypass_EmptySecretGrantsNoBypass(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: false}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	called := false
	handler := store.VaultAuthWithAdminBypass(nil, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	req := httptest.NewRequest("GET", "/api/engrams?vault=default", nil)
	req.AddCookie(&http.Cookie{Name: "muninn_session", Value: forgeSessionToken(nil)})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if called {
		t.Fatal("a locked vault was served to a cookie signed with an empty key")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d; want 401", w.Code)
	}
}

// TestNewSessionToken_EmptySecretIsRefused pins that no session token is ever
// issued under an empty key.
func TestNewSessionToken_EmptySecretIsRefused(t *testing.T) {
	for name, secret := range map[string][]byte{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			tok, err := auth.NewSessionToken("root", secret)
			if err == nil {
				t.Fatalf("NewSessionToken issued %q under an empty secret; want an error", tok)
			}
		})
	}
}
