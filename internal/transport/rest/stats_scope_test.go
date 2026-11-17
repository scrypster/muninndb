package rest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// scopeEnv is a REST server over a real engine and auth store, served on a
// loopback listener the test owns. Fixture: a fictional harbour pilot station
// with three vaults — the locked one the keys are pinned to, a public one, and
// a locked one nobody in these tests is entitled to.
type scopeEnv struct {
	eng     *engine.Engine
	store   *auth.Store
	url     string
	session string // a valid admin session cookie value

	pinned, public, other string
}

func newScopeEnv(t *testing.T) *scopeEnv {
	t.Helper()
	opts := storage.DefaultOptions()
	opts.FS = vfs.NewMem()
	db, err := storage.OpenPebble("rest-scope", opts)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := activation.NewNoopEmbedder()
	authStore := auth.NewStore(db)
	eng := engine.NewEngine(engine.EngineConfig{
		Store: store, AuthStore: authStore, FTSIndex: ftsIdx,
		ActivationEngine: activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, embedder),
		TriggerSystem:    trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, embedder),
		Embedder:         embedder,
	})
	t.Cleanup(func() {
		eng.Stop()
		store.Close()
	})

	env := &scopeEnv{eng: eng, store: authStore, pinned: "pilot-station", public: "harbour-notices", other: "customs-ledger"}
	full := context.WithValue(context.Background(), auth.ContextMode, auth.ModeFull)
	for vault, n := range map[string]int{env.pinned: 2, env.public: 3, env.other: 5} {
		if err := authStore.SetVaultConfig(auth.VaultConfig{Name: vault, Public: vault == env.public}); err != nil {
			t.Fatalf("configure %s: %v", vault, err)
		}
		for i := 0; i < n; i++ {
			_, err := eng.Write(full, &mbp.WriteRequest{
				Vault: vault, Concept: "Log entry " + strconv.Itoa(i),
				Content: "Entry " + strconv.Itoa(i) + " in the " + vault + " log.",
			})
			if err != nil {
				t.Fatalf("seed %s: %v", vault, err)
			}
		}
	}

	secret := []byte("pilot-station-session-secret-32b")
	srv := NewServer("127.0.0.1:0", NewEngineWrapper(eng, nil), authStore, secret, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	env.url = ts.URL
	if env.session, err = auth.NewSessionToken("admin", secret); err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	return env
}

func (e *scopeEnv) key(t *testing.T, mode string) string {
	t.Helper()
	token, _, err := e.store.GenerateAPIKey(e.pinned, "scope-"+mode, mode, nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey(%s): %v", mode, err)
	}
	return token
}

// get performs one GET. bearer and admin are optional credentials.
func (e *scopeEnv) get(t *testing.T, path, bearer string, admin bool) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if admin {
		req.AddCookie(&http.Cookie{Name: "muninn_session", Value: e.session})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, body
}

// statBody is the wire shape of GET /api/stats, decoded independently of the
// server's own type.
type statBody struct {
	EngramCount  int64                      `json:"engram_count"`
	VaultCount   int                        `json:"vault_count"`
	StorageBytes int64                      `json:"storage_bytes"`
	Coherence    map[string]json.RawMessage `json:"coherence"`
	Scope        string                     `json:"scope"`
}

// TestStatsScopeMatrix pins what GET /api/stats tells each kind of caller.
//
// A caller entitled to one vault — a key pinned to it, or no credential at all
// on a public vault — learns about that vault only: no other vault's name, no
// other vault's engram total, and neither the server's vault count nor its
// on-disk size, which would give the others away. The admin session, which is
// what the console's dashboard uses, still gets the server-wide answer.
//
// Before the scoping, all of them got the server-wide answer, including a
// coherence block naming every vault with its engram total.
func TestStatsScopeMatrix(t *testing.T) {
	env := newScopeEnv(t)
	others := func(own string) []string {
		var out []string
		for _, v := range []string{env.pinned, env.public, env.other} {
			if v != own {
				out = append(out, v)
			}
		}
		return out
	}

	scoped := []struct {
		name   string
		path   string
		bearer string
		vault  string
	}{
		{"no key, public vault", "/api/stats?vault=" + env.public, "", env.public},
		{"observe key", "/api/stats", env.key(t, auth.ModeObserve), env.pinned},
		{"full key", "/api/stats", env.key(t, auth.ModeFull), env.pinned},
		{"append key", "/api/stats", env.key(t, auth.ModeAppend), env.pinned},
	}
	for _, p := range scoped {
		t.Run(p.name, func(t *testing.T) {
			code, body := env.get(t, p.path, p.bearer, false)
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, body)
			}
			t.Logf("answer: %s", body)
			for _, vault := range others(p.vault) {
				if strings.Contains(string(body), vault) {
					t.Errorf("the answer names vault %q, which this caller is not entitled to", vault)
				}
			}
			var st statBody
			if err := json.Unmarshal(body, &st); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if st.Scope != "vault" {
				t.Errorf("scope = %q; want \"vault\"", st.Scope)
			}
			if st.VaultCount != 1 {
				t.Errorf("vault_count = %d; want 1, the one vault in scope (the server has 3)", st.VaultCount)
			}
			if st.StorageBytes != 0 {
				t.Errorf("storage_bytes = %d; want 0: the server's on-disk size covers every vault", st.StorageBytes)
			}
			if st.EngramCount <= 0 {
				t.Errorf("engram_count = %d; want the vault's own count", st.EngramCount)
			}
			if _, ok := st.Coherence[p.vault]; !ok || len(st.Coherence) != 1 {
				t.Errorf("coherence has %d entries; want exactly the caller's own vault", len(st.Coherence))
			}
		})
	}

	t.Run("write key", func(t *testing.T) {
		code, body := env.get(t, "/api/stats", env.key(t, auth.ModeWrite), false)
		if code != http.StatusForbidden {
			t.Fatalf("status %d: %s; want 403, an ingest-only key reads nothing", code, body)
		}
	})

	t.Run("no key, locked vault", func(t *testing.T) {
		code, body := env.get(t, "/api/stats?vault="+env.pinned, "", false)
		if code != http.StatusUnauthorized {
			t.Fatalf("status %d: %s; want 401", code, body)
		}
	})

	// The console's dashboard: an admin session asks for the selected vault's
	// engram count alongside the server's vault count and on-disk size.
	t.Run("admin session", func(t *testing.T) {
		code, body := env.get(t, "/api/stats?vault="+env.pinned, "", true)
		if code != http.StatusOK {
			t.Fatalf("status %d: %s", code, body)
		}
		t.Logf("answer: %s", body)
		var st statBody
		if err := json.Unmarshal(body, &st); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if st.Scope != "server" {
			t.Errorf("scope = %q; want \"server\"", st.Scope)
		}
		if st.VaultCount != 3 {
			t.Errorf("vault_count = %d; want 3", st.VaultCount)
		}
		for _, vault := range []string{env.pinned, env.public, env.other} {
			if _, ok := st.Coherence[vault]; !ok {
				t.Errorf("coherence has no entry for vault %q", vault)
			}
		}
	})

	// A session cookie that does not verify is not an admin session: the
	// request falls through to ordinary vault auth and gets the scoped answer.
	t.Run("forged session cookie, public vault", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, env.url+"/api/stats?vault="+env.public, nil)
		req.AddCookie(&http.Cookie{Name: "muninn_session", Value: "not-a-session"})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		for _, vault := range others(env.public) {
			if strings.Contains(string(body), vault) {
				t.Errorf("the answer names vault %q", vault)
			}
		}
	})
}
