package grpc_test

import (
	"context"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	transportgrpc "github.com/scrypster/muninndb/internal/transport/grpc"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// Vault-isolation tests: the gRPC transport must enforce the same vault model
// as REST (auth.VaultAuthMiddleware + validateResolvedVault):
//
//   - keyed requests: the key's vault is authoritative — an empty request
//     vault resolves to it, a mismatched one is rejected with PermissionDenied.
//   - unkeyed stream messages: the interceptor can only pre-authorize
//     "default"; a message naming another vault must pass the same fail-closed
//     public-vault check.
// ---------------------------------------------------------------------------

// vaultRecorder wraps mockEngine and records the vault of every request that
// reaches the engine.
type vaultRecorder struct {
	mockEngine
	mu     sync.Mutex
	vaults []string
}

func (r *vaultRecorder) record(v string) {
	r.mu.Lock()
	r.vaults = append(r.vaults, v)
	r.mu.Unlock()
}

func (r *vaultRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.vaults...)
}

func newVaultRecorder() *vaultRecorder {
	r := &vaultRecorder{}
	r.mockEngine = mockEngine{
		helloFn: func(ctx context.Context, req *pb.HelloRequest) (*pb.HelloResponse, error) {
			r.record(req.Vault)
			return &pb.HelloResponse{ServerVersion: "1.0.0"}, nil
		},
		writeFn: func(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
			r.record(req.Vault)
			return &pb.WriteResponse{ID: "e-1"}, nil
		},
		readFn: func(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error) {
			r.record(req.Vault)
			return &pb.ReadResponse{ID: req.ID}, nil
		},
		statFn: func(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error) {
			r.record(req.Vault)
			return &pb.StatResponse{}, nil
		},
		forgetFn: func(ctx context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error) {
			r.record(req.Vault)
			return &pb.ForgetResponse{OK: true}, nil
		},
		linkFn: func(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error) {
			r.record(req.Vault)
			return &pb.LinkResponse{OK: true}, nil
		},
		batchWriteFn: func(ctx context.Context, req *pb.BatchWriteRequest) (*pb.BatchWriteResponse, error) {
			for _, w := range req.Requests {
				r.record(w.Vault)
			}
			return &pb.BatchWriteResponse{}, nil
		},
	}
	return r
}

// keyedCtx builds the context the auth interceptors create for a request
// authenticated with an API key scoped to the given vault.
func keyedCtx(vault string) context.Context {
	key := &auth.APIKey{Vault: vault, Mode: auth.ModeFull}
	ctx := context.WithValue(context.Background(), auth.ContextVault, vault)
	ctx = context.WithValue(ctx, auth.ContextMode, auth.ModeFull)
	ctx = context.WithValue(ctx, auth.ContextAPIKey, key)
	return ctx
}

// unkeyedCtx builds the context the stream interceptor creates for a request
// with no API key (it can only pre-authorize "default").
func unkeyedCtx() context.Context {
	ctx := context.WithValue(context.Background(), auth.ContextVault, "default")
	ctx = context.WithValue(ctx, auth.ContextMode, auth.ModeFull)
	return ctx
}

func wantCode(t *testing.T, err error, want codes.Code, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected %v error, got nil", context, want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("%s: expected gRPC status error, got %v", context, err)
	}
	if st.Code() != want {
		t.Fatalf("%s: expected code %v, got %v (%s)", context, want, st.Code(), st.Message())
	}
}

// --- keyed unary requests ----------------------------------------------------

func TestVaultScope_KeyedWrite_ForeignVault_Rejected(t *testing.T) {
	rec := newVaultRecorder()
	srv := transportgrpc.NewServer(":0", rec, newTestAuthStore(t), nil)

	_, err := srv.Write(keyedCtx("vault-a"), &pb.WriteRequest{Concept: "c", Content: "x", Vault: "vault-b"})
	wantCode(t, err, codes.PermissionDenied, "keyed write to foreign vault")
	if seen := rec.seen(); len(seen) != 0 {
		t.Fatalf("request reached the engine with vaults %v — must be rejected", seen)
	}
}

func TestVaultScope_KeyedWrite_EmptyVault_PinnedToKeyVault(t *testing.T) {
	rec := newVaultRecorder()
	srv := transportgrpc.NewServer(":0", rec, newTestAuthStore(t), nil)

	_, err := srv.Write(keyedCtx("vault-a"), &pb.WriteRequest{Concept: "c", Content: "x"})
	if err != nil {
		t.Fatalf("keyed write with empty vault: %v", err)
	}
	if seen := rec.seen(); len(seen) != 1 || seen[0] != "vault-a" {
		t.Fatalf("engine saw vaults %v, want [vault-a]", seen)
	}
}

func TestVaultScope_KeyedWrite_MatchingVault_Allowed(t *testing.T) {
	rec := newVaultRecorder()
	srv := transportgrpc.NewServer(":0", rec, newTestAuthStore(t), nil)

	_, err := srv.Write(keyedCtx("vault-a"), &pb.WriteRequest{Concept: "c", Content: "x", Vault: "vault-a"})
	if err != nil {
		t.Fatalf("keyed write to own vault: %v", err)
	}
	if seen := rec.seen(); len(seen) != 1 || seen[0] != "vault-a" {
		t.Fatalf("engine saw vaults %v, want [vault-a]", seen)
	}
}

// TestVaultScope_KeyedAllUnaryOps_ForeignVault_Rejected sweeps every unary RPC
// with a vault field.
func TestVaultScope_KeyedAllUnaryOps_ForeignVault_Rejected(t *testing.T) {
	rec := newVaultRecorder()
	srv := transportgrpc.NewServer(":0", rec, newTestAuthStore(t), nil)
	ctx := keyedCtx("vault-a")

	ops := []struct {
		name string
		call func() error
	}{
		{"hello", func() error { _, err := srv.Hello(ctx, &pb.HelloRequest{Version: "1", Vault: "vault-b"}); return err }},
		{"read", func() error { _, err := srv.Read(ctx, &pb.ReadRequest{ID: "x", Vault: "vault-b"}); return err }},
		{"forget", func() error { _, err := srv.Forget(ctx, &pb.ForgetRequest{ID: "x", Vault: "vault-b"}); return err }},
		{"link", func() error {
			_, err := srv.Link(ctx, &pb.LinkRequest{SourceID: "a", TargetID: "b", Vault: "vault-b"})
			return err
		}},
		{"stat", func() error { _, err := srv.Stat(ctx, &pb.StatRequest{Vault: "vault-b"}); return err }},
		{"batchwrite", func() error {
			_, err := srv.BatchWrite(ctx, &pb.BatchWriteRequest{Requests: []*pb.WriteRequest{
				{Concept: "c", Content: "x", Vault: "vault-a"},
				{Concept: "c", Content: "x", Vault: "vault-b"},
			}})
			return err
		}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			wantCode(t, op.call(), codes.PermissionDenied, op.name+" naming foreign vault")
		})
	}
	if seen := rec.seen(); len(seen) != 0 {
		t.Fatalf("requests reached the engine with vaults %v — all must be rejected", seen)
	}
}

// --- unkeyed unary requests --------------------------------------------------

// TestVaultScope_UnkeyedWrite_LockedVault_Rejected covers the path where the
// unary interceptor pre-authorizes "default" (it cannot read the vault of a
// non-batch request) and the handler-level resolveRequestVault is the sole
// enforcement against an unkeyed request naming a locked vault.
func TestVaultScope_UnkeyedWrite_LockedVault_Rejected(t *testing.T) {
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig default: %v", err)
	}
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "secret", Public: false}); err != nil {
		t.Fatalf("SetVaultConfig secret: %v", err)
	}
	rec := newVaultRecorder()
	srv := transportgrpc.NewServer(":0", rec, store, nil)

	_, err := srv.Write(unkeyedCtx(), &pb.WriteRequest{Concept: "c", Content: "x", Vault: "secret"})
	wantCode(t, err, codes.Unauthenticated, "unkeyed write to locked vault")
	if seen := rec.seen(); len(seen) != 0 {
		t.Fatalf("request reached the engine with vaults %v — must be rejected", seen)
	}
}

func TestVaultScope_UnkeyedWrite_PublicVault_Allowed(t *testing.T) {
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "shared", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	rec := newVaultRecorder()
	srv := transportgrpc.NewServer(":0", rec, store, nil)

	if _, err := srv.Write(unkeyedCtx(), &pb.WriteRequest{Concept: "c", Content: "x", Vault: "shared"}); err != nil {
		t.Fatalf("unkeyed write to public vault: %v", err)
	}
	if seen := rec.seen(); len(seen) != 1 || seen[0] != "shared" {
		t.Fatalf("engine saw vaults %v, want [shared]", seen)
	}
}

// TestVaultScope_NilAuthStore_FailsClosed verifies resolveRequestVault denies an
// unkeyed request when the server has no auth store, rather than defaulting open.
func TestVaultScope_NilAuthStore_FailsClosed(t *testing.T) {
	rec := newVaultRecorder()
	srv := transportgrpc.NewServer(":0", rec, nil, nil)

	_, err := srv.Write(unkeyedCtx(), &pb.WriteRequest{Concept: "c", Content: "x", Vault: "default"})
	wantCode(t, err, codes.Unauthenticated, "unkeyed write with nil auth store")
	if seen := rec.seen(); len(seen) != 0 {
		t.Fatalf("request reached the engine with vaults %v — must fail closed", seen)
	}
}

// --- streaming RPCs -----------------------------------------------------------

// stubActivateStream implements pb.MuninnDB_ActivateServer.
type stubActivateStream struct {
	googlegrpc.ServerStream
	ctx  context.Context
	sent []*pb.ActivateResponse
}

func (s *stubActivateStream) Context() context.Context { return s.ctx }
func (s *stubActivateStream) Send(r *pb.ActivateResponse) error {
	s.sent = append(s.sent, r)
	return nil
}

func TestVaultScope_KeyedActivateStream_ForeignVault_Rejected(t *testing.T) {
	rec := newVaultRecorder()
	rec.activateFn = func(ctx context.Context, req *pb.ActivateRequest) (*pb.ActivateResponse, error) {
		rec.record(req.Vault)
		return &pb.ActivateResponse{}, nil
	}
	srv := transportgrpc.NewServer(":0", rec, newTestAuthStore(t), nil)

	stream := &stubActivateStream{ctx: keyedCtx("vault-a")}
	err := srv.Activate(&pb.ActivateRequest{Context: []string{"q"}, Vault: "vault-b"}, stream)
	wantCode(t, err, codes.PermissionDenied, "keyed activate stream to foreign vault")
	if seen := rec.seen(); len(seen) != 0 {
		t.Fatalf("request reached the engine with vaults %v — must be rejected", seen)
	}
}

func TestVaultScope_KeyedActivateStream_EmptyVault_Pinned(t *testing.T) {
	rec := newVaultRecorder()
	rec.activateFn = func(ctx context.Context, req *pb.ActivateRequest) (*pb.ActivateResponse, error) {
		rec.record(req.Vault)
		return &pb.ActivateResponse{}, nil
	}
	srv := transportgrpc.NewServer(":0", rec, newTestAuthStore(t), nil)

	stream := &stubActivateStream{ctx: keyedCtx("vault-a")}
	if err := srv.Activate(&pb.ActivateRequest{Context: []string{"q"}}, stream); err != nil {
		t.Fatalf("keyed activate with empty vault: %v", err)
	}
	if seen := rec.seen(); len(seen) != 1 || seen[0] != "vault-a" {
		t.Fatalf("engine saw vaults %v, want [vault-a]", seen)
	}
}

// stubSubscribeStream implements pb.MuninnDB_SubscribeServer, delivering one
// SubscribeRequest then blocking until the context is done.
type stubSubscribeStream struct {
	googlegrpc.ServerStream
	ctx    context.Context
	cancel context.CancelFunc
	req    *pb.SubscribeRequest
	once   sync.Once
}

func (s *stubSubscribeStream) Context() context.Context { return s.ctx }
func (s *stubSubscribeStream) Send(p *pb.ActivationPush) error {
	// First send confirms the subscription; cancel so the handler loop exits.
	s.cancel()
	return nil
}
func (s *stubSubscribeStream) Recv() (*pb.SubscribeRequest, error) {
	var req *pb.SubscribeRequest
	s.once.Do(func() { req = s.req })
	if req != nil {
		return req, nil
	}
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func TestVaultScope_UnkeyedSubscribeStream_LockedVault_Rejected(t *testing.T) {
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "secret", Public: false}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	rec := newVaultRecorder()
	rec.subscribeWithDeliverFn = func(ctx context.Context, req *pb.SubscribeRequest, _ trigger.DeliverFunc) (string, error) {
		rec.record(req.Vault)
		return "sub-1", nil
	}
	srv := transportgrpc.NewServer(":0", rec, store, nil)

	ctx, cancel := context.WithCancel(unkeyedCtx())
	defer cancel()
	stream := &stubSubscribeStream{ctx: ctx, cancel: cancel, req: &pb.SubscribeRequest{Vault: "secret"}}
	err := srv.Subscribe(stream)
	wantCode(t, err, codes.Unauthenticated, "unkeyed subscribe to locked vault")
	if seen := rec.seen(); len(seen) != 0 {
		t.Fatalf("request reached the engine with vaults %v — must be rejected", seen)
	}
}

func TestVaultScope_KeyedSubscribeStream_ForeignVault_Rejected(t *testing.T) {
	rec := newVaultRecorder()
	srv := transportgrpc.NewServer(":0", rec, newTestAuthStore(t), nil)

	ctx, cancel := context.WithCancel(keyedCtx("vault-a"))
	defer cancel()
	stream := &stubSubscribeStream{ctx: ctx, cancel: cancel, req: &pb.SubscribeRequest{Vault: "vault-b"}}
	err := srv.Subscribe(stream)
	wantCode(t, err, codes.PermissionDenied, "keyed subscribe to foreign vault")
}
