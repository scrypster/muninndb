package mbp

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
)

// ---------------------------------------------------------------------------
// Vault-isolation tests: the MBP transport must enforce the same fail-closed
// vault model as REST (auth.VaultAuthMiddleware) and MCP:
//
//   - token sessions: the key's vault is authoritative — an empty request
//     vault resolves to it, a mismatched one is rejected.
//   - auth_method "none" sessions: every requested vault (default "default")
//     must be configured Public; locked or unconfigured vaults are rejected.
// ---------------------------------------------------------------------------

// recEngine records the vault of every request that reaches the engine, so
// tests can assert both that rejected requests never reach it and that
// accepted requests arrive with the resolved (pinned) vault.
type recEngine struct {
	stubEngine
	mu     sync.Mutex
	vaults []string
}

func (e *recEngine) record(v string) {
	e.mu.Lock()
	e.vaults = append(e.vaults, v)
	e.mu.Unlock()
}

func (e *recEngine) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.vaults...)
}

func (e *recEngine) Hello(ctx context.Context, req *HelloRequest) (*HelloResponse, error) {
	return e.stubEngine.Hello(ctx, req)
}

func (e *recEngine) Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error) {
	e.record(req.Vault)
	return e.stubEngine.Write(ctx, req)
}

func (e *recEngine) Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error) {
	e.record(req.Vault)
	return e.stubEngine.Read(ctx, req)
}

func (e *recEngine) Activate(ctx context.Context, req *ActivateRequest) (*ActivateResponse, error) {
	e.record(req.Vault)
	return e.stubEngine.Activate(ctx, req)
}

func (e *recEngine) Link(ctx context.Context, req *LinkRequest) (*LinkResponse, error) {
	e.record(req.Vault)
	return e.stubEngine.Link(ctx, req)
}

func (e *recEngine) Forget(ctx context.Context, req *ForgetRequest) (*ForgetResponse, error) {
	e.record(req.Vault)
	return e.stubEngine.Forget(ctx, req)
}

func (e *recEngine) Stat(ctx context.Context, req *StatRequest) (*StatResponse, error) {
	e.record(req.Vault)
	return e.stubEngine.Stat(ctx, req)
}

func (e *recEngine) Subscribe(ctx context.Context, req *SubscribeRequest) (*SubscribeResponse, error) {
	e.record(req.Vault)
	return e.stubEngine.Subscribe(ctx, req)
}

// newVaultAuthStore returns an auth store backed by an in-memory pebble DB
// with the "default" vault configured Public (matching first-run bootstrap)
// and a "secret" vault configured locked.
func newVaultAuthStore(t *testing.T) *auth.Store {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open test auth db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := auth.NewStore(db)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig default: %v", err)
	}
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "secret", Public: false}); err != nil {
		t.Fatalf("SetVaultConfig secret: %v", err)
	}
	return store
}

func newAuthedTestServer(eng EngineAPI, store *auth.Store) *Server {
	return &Server{engine: eng, authStore: store, shutdown: make(chan struct{})}
}

// doHandshakeReq sends an arbitrary HELLO and returns the response frame.
func doHandshakeReq(t *testing.T, c net.Conn, req HelloRequest) *Frame {
	t.Helper()
	p, err := EncodeMsgpack(&req)
	if err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	if err := WriteFrame(c, &Frame{Version: 0x01, Type: TypeHello, Payload: p}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	f, err := ReadFrame(c)
	if err != nil {
		t.Fatalf("read hello response: %v", err)
	}
	return f
}

func assertAuthError(t *testing.T, f *Frame, context string) {
	t.Helper()
	if f.Type != TypeError {
		t.Fatalf("%s: expected TypeError frame, got 0x%02x", context, f.Type)
	}
	var ep ErrorPayload
	if err := DecodeMsgpack(f.Payload, &ep); err != nil {
		t.Fatalf("%s: decode error payload: %v", context, err)
	}
	if ep.Code != ErrAuthFailed {
		t.Fatalf("%s: expected ErrAuthFailed (%d), got %d (%s)", context, ErrAuthFailed, ep.Code, ep.Message)
	}
}

// --- HELLO-time enforcement ------------------------------------------------

func TestHello_NoneAuth_LockedVault_Rejected(t *testing.T) {
	eng := &recEngine{}
	s := newAuthedTestServer(eng, newVaultAuthStore(t))
	c, wait := startTestConn(t, s)
	defer wait()

	f := doHandshakeReq(t, c, HelloRequest{Version: "1", AuthMethod: "none", Vault: "secret"})
	assertAuthError(t, f, "HELLO none/secret")
}

func TestHello_NoneAuth_UnconfiguredVault_Rejected(t *testing.T) {
	eng := &recEngine{}
	s := newAuthedTestServer(eng, newVaultAuthStore(t))
	c, wait := startTestConn(t, s)
	defer wait()

	f := doHandshakeReq(t, c, HelloRequest{Version: "1", AuthMethod: "none", Vault: "never-configured"})
	assertAuthError(t, f, "HELLO none/unconfigured")
}

func TestHello_NoneAuth_PublicVault_OK(t *testing.T) {
	eng := &recEngine{}
	s := newAuthedTestServer(eng, newVaultAuthStore(t))
	c, wait := startTestConn(t, s)
	defer wait()

	f := doHandshakeReq(t, c, HelloRequest{Version: "1", AuthMethod: "none"})
	if f.Type != TypeHelloOK {
		t.Fatalf("HELLO none/default-public: expected HELLO_OK, got 0x%02x", f.Type)
	}
}

func TestHello_Token_VaultMismatch_Rejected(t *testing.T) {
	eng := &recEngine{}
	store := newVaultAuthStore(t)
	token, _, err := store.GenerateAPIKey("vault-a", "test", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	s := newAuthedTestServer(eng, store)
	c, wait := startTestConn(t, s)
	defer wait()

	f := doHandshakeReq(t, c, HelloRequest{Version: "1", AuthMethod: "token", Token: token, Vault: "vault-b"})
	assertAuthError(t, f, "HELLO token vault mismatch")
}

func TestHello_Token_MatchingVault_OK(t *testing.T) {
	eng := &recEngine{}
	store := newVaultAuthStore(t)
	token, _, err := store.GenerateAPIKey("vault-a", "test", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	s := newAuthedTestServer(eng, store)
	c, wait := startTestConn(t, s)
	defer wait()

	f := doHandshakeReq(t, c, HelloRequest{Version: "1", AuthMethod: "token", Token: token, Vault: "vault-a"})
	if f.Type != TypeHelloOK {
		t.Fatalf("HELLO token matching vault: expected HELLO_OK, got 0x%02x", f.Type)
	}
}

// --- Per-request enforcement: auth_method "none" sessions -------------------

// TestNoneSession_LockedVault_AllOpsRejected verifies that a "none" session
// (established against the public default vault) cannot touch a locked vault
// with any vault-scoped operation — the request must be rejected with
// ErrAuthFailed and must never reach the engine.
func TestNoneSession_LockedVault_AllOpsRejected(t *testing.T) {
	ops := []struct {
		name  string
		ftype uint8
		req   interface{}
	}{
		{"write", TypeWrite, &WriteRequest{Concept: "c", Content: "x", Vault: "secret"}},
		{"read", TypeRead, &ReadRequest{ID: "01HZZZZZZZZZZZZZZZZZZZZZZZ", Vault: "secret"}},
		{"activate", TypeActivate, &ActivateRequest{Context: []string{"q"}, Vault: "secret"}},
		{"link", TypeLink, &LinkRequest{SourceID: "a", TargetID: "b", Vault: "secret"}},
		{"forget", TypeForget, &ForgetRequest{ID: "01HZZZZZZZZZZZZZZZZZZZZZZZ", Vault: "secret"}},
		{"stat", TypeStat, &StatRequest{Vault: "secret"}},
		{"subscribe", TypeSubscribe, &SubscribeRequest{Context: []string{"q"}, Vault: "secret"}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			eng := &recEngine{}
			s := newAuthedTestServer(eng, newVaultAuthStore(t))
			c, wait := startTestConn(t, s)
			defer wait()
			doHandshake(t, c)

			f := sendAndReceive(t, c, op.ftype, 42, op.req)
			assertAuthError(t, f, op.name+" to locked vault")
			if seen := eng.seen(); len(seen) != 0 {
				t.Fatalf("%s: request reached the engine with vaults %v — must be rejected before dispatch", op.name, seen)
			}
		})
	}
}

func TestNoneSession_PublicVault_Allowed(t *testing.T) {
	eng := &recEngine{}
	s := newAuthedTestServer(eng, newVaultAuthStore(t))
	c, wait := startTestConn(t, s)
	defer wait()
	doHandshake(t, c)

	f := sendAndReceive(t, c, TypeWrite, 7, &WriteRequest{Concept: "c", Content: "x", Vault: "default"})
	if f.Type != TypeWriteOK {
		t.Fatalf("write to public vault: expected TypeWriteOK, got 0x%02x", f.Type)
	}
	if seen := eng.seen(); len(seen) != 1 || seen[0] != "default" {
		t.Fatalf("engine saw vaults %v, want [default]", seen)
	}
}

func TestNoneSession_EmptyVault_ResolvesToDefault(t *testing.T) {
	eng := &recEngine{}
	s := newAuthedTestServer(eng, newVaultAuthStore(t))
	c, wait := startTestConn(t, s)
	defer wait()
	doHandshake(t, c)

	f := sendAndReceive(t, c, TypeWrite, 7, &WriteRequest{Concept: "c", Content: "x"})
	if f.Type != TypeWriteOK {
		t.Fatalf("write with empty vault: expected TypeWriteOK, got 0x%02x", f.Type)
	}
	if seen := eng.seen(); len(seen) != 1 || seen[0] != "default" {
		t.Fatalf("engine saw vaults %v, want [default]", seen)
	}
}

// --- Per-request enforcement: token sessions --------------------------------

func tokenSession(t *testing.T, eng EngineAPI, store *auth.Store, vault string) (net.Conn, func()) {
	t.Helper()
	token, _, err := store.GenerateAPIKey(vault, "test", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	s := newAuthedTestServer(eng, store)
	c, wait := startTestConn(t, s)
	f := doHandshakeReq(t, c, HelloRequest{Version: "1", AuthMethod: "token", Token: token})
	if f.Type != TypeHelloOK {
		t.Fatalf("token handshake: expected HELLO_OK, got 0x%02x", f.Type)
	}
	return c, wait
}

func TestTokenSession_OtherVault_Rejected(t *testing.T) {
	eng := &recEngine{}
	c, wait := tokenSession(t, eng, newVaultAuthStore(t), "vault-a")
	defer wait()

	f := sendAndReceive(t, c, TypeWrite, 9, &WriteRequest{Concept: "c", Content: "x", Vault: "vault-b"})
	assertAuthError(t, f, "keyed write to foreign vault")
	if seen := eng.seen(); len(seen) != 0 {
		t.Fatalf("request reached the engine with vaults %v — must be rejected", seen)
	}
}

// TestTokenSession_PublicVaultStillPinned: a key scoped to vault-a must not be
// usable against any other vault — not even a public one. The key pins the
// session; public access is for unauthenticated clients (REST behaves the same).
func TestTokenSession_PublicVaultStillPinned(t *testing.T) {
	eng := &recEngine{}
	c, wait := tokenSession(t, eng, newVaultAuthStore(t), "vault-a")
	defer wait()

	f := sendAndReceive(t, c, TypeWrite, 9, &WriteRequest{Concept: "c", Content: "x", Vault: "default"})
	assertAuthError(t, f, "keyed write to public-but-foreign vault")
}

func TestTokenSession_EmptyVault_ResolvesToKeyVault(t *testing.T) {
	eng := &recEngine{}
	c, wait := tokenSession(t, eng, newVaultAuthStore(t), "vault-a")
	defer wait()

	f := sendAndReceive(t, c, TypeWrite, 9, &WriteRequest{Concept: "c", Content: "x"})
	if f.Type != TypeWriteOK {
		t.Fatalf("keyed write with empty vault: expected TypeWriteOK, got 0x%02x", f.Type)
	}
	if seen := eng.seen(); len(seen) != 1 || seen[0] != "vault-a" {
		t.Fatalf("engine saw vaults %v, want [vault-a]", seen)
	}
}

func TestTokenSession_MatchingVault_Allowed(t *testing.T) {
	eng := &recEngine{}
	c, wait := tokenSession(t, eng, newVaultAuthStore(t), "vault-a")
	defer wait()

	f := sendAndReceive(t, c, TypeRead, 9, &ReadRequest{ID: "01HZZZZZZZZZZZZZZZZZZZZZZZ", Vault: "vault-a"})
	if f.Type != TypeReadResp {
		t.Fatalf("keyed read of own vault: expected TypeReadResp, got 0x%02x", f.Type)
	}
	if seen := eng.seen(); len(seen) != 1 || seen[0] != "vault-a" {
		t.Fatalf("engine saw vaults %v, want [vault-a]", seen)
	}
}

// --- Fail-closed without an auth store ---------------------------------------

// TestNilAuthStore_FailsClosed: a server constructed without an auth store
// (misconfiguration) must reject sessions rather than silently allowing
// unrestricted access to every vault.
func TestNilAuthStore_FailsClosed(t *testing.T) {
	eng := &recEngine{}
	s := &Server{engine: eng, shutdown: make(chan struct{})} // nil authStore
	c, wait := startTestConn(t, s)
	defer wait()

	f := doHandshakeReq(t, c, HelloRequest{Version: "1", AuthMethod: "none"})
	assertAuthError(t, f, "HELLO with nil auth store")
}
