package grpc_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/storage"
	transportgrpc "github.com/scrypster/muninndb/internal/transport/grpc"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mockEngine implements EngineAPI for testing. Every method returns a zero-value
// response and no error unless the test provides specific behaviour via the
// function fields.
type mockEngine struct {
	helloFn                func(ctx context.Context, req *pb.HelloRequest) (*pb.HelloResponse, error)
	writeFn                func(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error)
	batchWriteFn           func(ctx context.Context, req *pb.BatchWriteRequest) (*pb.BatchWriteResponse, error)
	readFn                 func(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error)
	activateFn             func(ctx context.Context, req *pb.ActivateRequest) (*pb.ActivateResponse, error)
	linkFn                 func(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error)
	forgetFn               func(ctx context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error)
	statFn                 func(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error)
	subscribeFn            func(ctx context.Context, req *pb.SubscribeRequest) (*pb.SubscribeResponse, error)
	subscribeWithDeliverFn func(ctx context.Context, req *pb.SubscribeRequest, deliver trigger.DeliverFunc) (string, error)
	unsubscribeFn          func(ctx context.Context, subID string) error
}

func (m *mockEngine) Hello(ctx context.Context, req *pb.HelloRequest) (*pb.HelloResponse, error) {
	if m.helloFn != nil {
		return m.helloFn(ctx, req)
	}
	return &pb.HelloResponse{ServerVersion: "test"}, nil
}

func (m *mockEngine) Write(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
	if m.writeFn != nil {
		return m.writeFn(ctx, req)
	}
	return &pb.WriteResponse{ID: "00000000000000000000000000"}, nil
}

func (m *mockEngine) BatchWrite(ctx context.Context, req *pb.BatchWriteRequest) (*pb.BatchWriteResponse, error) {
	if m.batchWriteFn != nil {
		return m.batchWriteFn(ctx, req)
	}
	results := make([]*pb.BatchWriteItemResult, len(req.Requests))
	for i := range req.Requests {
		results[i] = &pb.BatchWriteItemResult{
			Index: int32(i),
			Id:    "00000000000000000000000000",
		}
	}
	return &pb.BatchWriteResponse{Results: results}, nil
}

func (m *mockEngine) Read(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error) {
	if m.readFn != nil {
		return m.readFn(ctx, req)
	}
	return &pb.ReadResponse{}, nil
}

func (m *mockEngine) Activate(ctx context.Context, req *pb.ActivateRequest) (*pb.ActivateResponse, error) {
	if m.activateFn != nil {
		return m.activateFn(ctx, req)
	}
	return &pb.ActivateResponse{}, nil
}

func (m *mockEngine) Link(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error) {
	if m.linkFn != nil {
		return m.linkFn(ctx, req)
	}
	return &pb.LinkResponse{OK: true}, nil
}

func (m *mockEngine) Forget(ctx context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error) {
	if m.forgetFn != nil {
		return m.forgetFn(ctx, req)
	}
	return &pb.ForgetResponse{OK: true}, nil
}

func (m *mockEngine) Stat(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error) {
	if m.statFn != nil {
		return m.statFn(ctx, req)
	}
	return &pb.StatResponse{}, nil
}

func (m *mockEngine) Subscribe(ctx context.Context, req *pb.SubscribeRequest) (*pb.SubscribeResponse, error) {
	if m.subscribeFn != nil {
		return m.subscribeFn(ctx, req)
	}
	return &pb.SubscribeResponse{SubID: "sub-1", Status: "ok"}, nil
}

func (m *mockEngine) SubscribeWithDeliver(ctx context.Context, req *pb.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
	if m.subscribeWithDeliverFn != nil {
		return m.subscribeWithDeliverFn(ctx, req, deliver)
	}
	return "mock-sub-id", nil
}

func (m *mockEngine) Unsubscribe(ctx context.Context, subID string) error {
	if m.unsubscribeFn != nil {
		return m.unsubscribeFn(ctx, subID)
	}
	return nil
}

// newTestAuthStore opens an in-memory pebble database and returns an auth.Store.
func newTestAuthStore(t *testing.T) *auth.Store {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open test auth db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return auth.NewStore(db)
}

// freePort returns an available TCP port on localhost.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not find free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// TestServerStartStop creates a Server, calls Serve in a goroutine, verifies it
// is accepting TCP connections, then cancels the context and verifies clean shutdown.
func TestServerStartStop(t *testing.T) {
	addr := freePort(t)
	engine := &mockEngine{}
	srv := transportgrpc.NewServer(addr, engine, newTestAuthStore(t), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ctx)
	}()

	// Verify the server is accepting TCP connections within a reasonable window.
	var conn net.Conn
	var dialErr error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr = net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("could not connect to server at %s: %v", addr, dialErr)
	}

	// Cancel context to trigger graceful shutdown.
	cancel()

	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("Serve returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("server did not shut down within 3 seconds after context cancellation")
	}
}

// TestGracefulShutdown starts a server, calls Shutdown with a reasonable timeout,
// and verifies it returns nil.
func TestGracefulShutdown(t *testing.T) {
	addr := freePort(t)
	engine := &mockEngine{}
	srv := transportgrpc.NewServer(addr, engine, newTestAuthStore(t), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ctx)
	}()

	// Wait for server to be up.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Call Shutdown with a 2-second timeout.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Errorf("Shutdown returned unexpected error: %v", err)
	}
}

// TestEngineAPIInterface verifies at compile time that mockEngine satisfies the
// EngineAPI interface. This test contains no runtime assertions — if it compiles,
// the interface contract is met.
func TestEngineAPIInterface(t *testing.T) {
	// Verify that NewServer accepts our mock without a cast.
	engine := &mockEngine{}
	_ = transportgrpc.NewServer(":0", engine, newTestAuthStore(t), nil)
}

// TestSubscribeWithDeliverInterface verifies at compile-time and runtime that:
//   - mockEngine satisfies the full EngineAPI interface including SubscribeWithDeliver
//   - The deliver func passed to SubscribeWithDeliver correctly channels pushes
//
// Note: the pb.* types in this project are hand-written Go structs with protobuf
// struct tags but without proto.Message implementation (ProtoReflect), so wire-level
// gRPC streaming cannot be tested end-to-end in unit tests. The server-side logic for
// the Subscribe streaming handler is exercised indirectly via the engine adapter and
// trigger system integration tests.
func TestSubscribeWithDeliverInterface(t *testing.T) {
	// Compile-time check: mockEngine satisfies transportgrpc.EngineAPI.
	var _ transportgrpc.EngineAPI = &mockEngine{}

	// Runtime: verify the SubscribeWithDeliver mock correctly invokes the deliver func.
	received := make(chan *trigger.ActivationPush, 4)
	var capturedDeliver trigger.DeliverFunc

	eng := &mockEngine{
		subscribeWithDeliverFn: func(ctx context.Context, req *pb.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
			capturedDeliver = deliver
			return "test-sub-id", nil
		},
	}

	// Call SubscribeWithDeliver — in production this is called by grpc.Server.Subscribe.
	ctx := context.Background()
	req := &pb.SubscribeRequest{Vault: "default", PushOnWrite: true}
	deliver := func(ctx context.Context, push *trigger.ActivationPush) error {
		received <- push
		return nil
	}

	subID, err := eng.SubscribeWithDeliver(ctx, req, deliver)
	if err != nil {
		t.Fatalf("SubscribeWithDeliver: %v", err)
	}
	if subID != "test-sub-id" {
		t.Errorf("subID = %q, want test-sub-id", subID)
	}
	if capturedDeliver == nil {
		t.Fatal("deliver func was not captured")
	}

	// Simulate the trigger system calling the deliver func.
	push := &trigger.ActivationPush{
		SubscriptionID: subID,
		Trigger:        trigger.TriggerNewWrite,
		PushNumber:     1,
		At:             time.Now(),
	}
	if err := capturedDeliver(ctx, push); err != nil {
		t.Fatalf("capturedDeliver: %v", err)
	}

	select {
	case got := <-received:
		if got.SubscriptionID != subID {
			t.Errorf("push.SubscriptionID = %q, want %q", got.SubscriptionID, subID)
		}
		if string(got.Trigger) != string(trigger.TriggerNewWrite) {
			t.Errorf("push.Trigger = %q, want %q", got.Trigger, trigger.TriggerNewWrite)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for push")
	}
}

// ---------------------------------------------------------------------------
// Auth interceptor tests
// ---------------------------------------------------------------------------

// TestAuthUnaryInterceptor_ValidKey generates a real API key, sends it via the
// authorization metadata header, and verifies the handler receives the correct
// vault, mode, and APIKey in its context.
func TestAuthUnaryInterceptor_ValidKey(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateAPIKey("testvault", "test-label", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	var capturedVault, capturedMode string
	var capturedKey *auth.APIKey
	handler := func(ctx context.Context, req any) (any, error) {
		capturedVault, _ = ctx.Value(auth.ContextVault).(string)
		capturedMode, _ = ctx.Value(auth.ContextMode).(string)
		capturedKey, _ = ctx.Value(auth.ContextAPIKey).(*auth.APIKey)
		return "ok", nil
	}

	md := metadata.Pairs("authorization", "Bearer "+token)
	ctx := metadata.NewIncomingContext(context.Background(), md)

	resp, err := srv.TestableAuthUnaryInterceptor(ctx, nil, nil, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	if resp != "ok" {
		t.Errorf("resp = %v, want \"ok\"", resp)
	}
	if capturedVault != "testvault" {
		t.Errorf("vault = %q, want \"testvault\"", capturedVault)
	}
	if capturedMode != "full" {
		t.Errorf("mode = %q, want \"full\"", capturedMode)
	}
	if capturedKey == nil {
		t.Fatal("context APIKey is nil")
	}
	if capturedKey.Vault != "testvault" {
		t.Errorf("key.Vault = %q, want \"testvault\"", capturedKey.Vault)
	}
}

// TestAuthUnaryInterceptor_InvalidKey sends a malformed token via the x-api-key
// metadata header and verifies the interceptor returns codes.Unauthenticated
// without invoking the handler.
func TestAuthUnaryInterceptor_InvalidKey(t *testing.T) {
	store := newTestAuthStore(t)
	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	handler := func(ctx context.Context, req any) (any, error) {
		t.Fatal("handler should not be called with invalid key")
		return nil, nil
	}

	md := metadata.Pairs("x-api-key", "mk_not-a-valid-base64-token!!")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := srv.TestableAuthUnaryInterceptor(ctx, nil, nil, handler)
	if err == nil {
		t.Fatal("expected error for invalid key, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", st.Code())
	}
}

// TestAuthUnaryInterceptor_NoKeyPublicVault configures the default vault as
// public, sends a request without any auth metadata, and verifies the handler
// is called with vault "default" and mode "full".
func TestAuthUnaryInterceptor_NoKeyPublicVault(t *testing.T) {
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	var capturedVault, capturedMode string
	handler := func(ctx context.Context, req any) (any, error) {
		capturedVault, _ = ctx.Value(auth.ContextVault).(string)
		capturedMode, _ = ctx.Value(auth.ContextMode).(string)
		return "ok", nil
	}

	ctx := context.Background()
	resp, err := srv.TestableAuthUnaryInterceptor(ctx, nil, nil, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	if resp != "ok" {
		t.Errorf("resp = %v, want \"ok\"", resp)
	}
	if capturedVault != "default" {
		t.Errorf("vault = %q, want \"default\"", capturedVault)
	}
	if capturedMode != "full" {
		t.Errorf("mode = %q, want \"full\"", capturedMode)
	}
}

// TestAuthUnaryInterceptor_NoKeyLockedVault explicitly locks the default vault
// and verifies that an unauthenticated request is rejected with
// codes.Unauthenticated.
func TestAuthUnaryInterceptor_NoKeyLockedVault(t *testing.T) {
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: false}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	handler := func(ctx context.Context, req any) (any, error) {
		t.Fatal("handler should not be called for locked vault without auth")
		return nil, nil
	}

	ctx := context.Background()
	_, err := srv.TestableAuthUnaryInterceptor(ctx, nil, nil, handler)
	if err == nil {
		t.Fatal("expected error for locked vault, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", st.Code())
	}
}

// TestAuthUnaryInterceptor_MissingKeyStore uses a fresh auth store with no
// vault configuration at all. The fail-closed default in GetVaultConfig returns
// Public: false for unknown vaults, so an unauthenticated request must be
// rejected with codes.Unauthenticated.
func TestAuthUnaryInterceptor_MissingKeyStore(t *testing.T) {
	store := newTestAuthStore(t)
	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	handler := func(ctx context.Context, req any) (any, error) {
		t.Fatal("handler should not be called when vault is unconfigured")
		return nil, nil
	}

	ctx := context.Background()
	_, err := srv.TestableAuthUnaryInterceptor(ctx, nil, nil, handler)
	if err == nil {
		t.Fatal("expected error for unconfigured vault (fail-closed), got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", st.Code())
	}
}

// vaultRequest satisfies the vaultNamer interface used by authUnaryInterceptor.
type vaultRequest struct{ vault string }

func (v *vaultRequest) GetVault() string { return v.vault }

// TestAuthUnaryInterceptor_VaultFromRequest verifies that authUnaryInterceptor
// extracts the vault name from a request implementing GetVault().
func TestAuthUnaryInterceptor_VaultFromRequest(t *testing.T) {
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	var capturedVault string
	handler := func(ctx context.Context, req any) (any, error) {
		capturedVault, _ = ctx.Value(auth.ContextVault).(string)
		return "ok", nil
	}

	req := &vaultRequest{vault: "myvault"}
	ctx := context.Background()
	_, err := srv.TestableAuthUnaryInterceptor(ctx, req, nil, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	if capturedVault != "myvault" {
		t.Errorf("vault = %q, want \"myvault\"", capturedVault)
	}
}

// TestAuthUnaryInterceptor_XApiKeyHeader verifies that the x-api-key metadata
// header is accepted as an alternative to the authorization header.
func TestAuthUnaryInterceptor_XApiKeyHeader(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateAPIKey("default", "test-label", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	var capturedMode string
	handler := func(ctx context.Context, req any) (any, error) {
		capturedMode, _ = ctx.Value(auth.ContextMode).(string)
		return "ok", nil
	}

	md := metadata.Pairs("x-api-key", token)
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err = srv.TestableAuthUnaryInterceptor(ctx, nil, nil, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	if capturedMode != "full" {
		t.Errorf("mode = %q, want \"full\"", capturedMode)
	}
}

// ---------------------------------------------------------------------------
// Auth stream interceptor tests
// ---------------------------------------------------------------------------

// mockServerStream implements grpc.ServerStream for testing stream interceptors.
type mockServerStream struct {
	ctx      context.Context
	sentMsgs []any
	recvMsgs []any
	recvIdx  int
}

func (m *mockServerStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockServerStream) SendHeader(metadata.MD) error { return nil }
func (m *mockServerStream) SetTrailer(metadata.MD)       {}
func (m *mockServerStream) Context() context.Context     { return m.ctx }
func (m *mockServerStream) SendMsg(msg any) error {
	m.sentMsgs = append(m.sentMsgs, msg)
	return nil
}
func (m *mockServerStream) RecvMsg(msg any) error { return nil }

func TestAuthStreamInterceptor_ValidKey(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateAPIKey("testvault", "test-label", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	var capturedVault, capturedMode string
	handler := func(srv any, stream grpc.ServerStream) error {
		ctx := stream.Context()
		capturedVault, _ = ctx.Value(auth.ContextVault).(string)
		capturedMode, _ = ctx.Value(auth.ContextMode).(string)
		return nil
	}

	md := metadata.Pairs("authorization", "Bearer "+token)
	ctx := metadata.NewIncomingContext(context.Background(), md)
	ss := &mockServerStream{ctx: ctx}

	err = srv.TestableAuthStreamInterceptor(nil, ss, nil, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	if capturedVault != "testvault" {
		t.Errorf("vault = %q, want \"testvault\"", capturedVault)
	}
	if capturedMode != "full" {
		t.Errorf("mode = %q, want \"full\"", capturedMode)
	}
}

func TestAuthStreamInterceptor_InvalidKey(t *testing.T) {
	store := newTestAuthStore(t)
	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	handler := func(srv any, stream grpc.ServerStream) error {
		t.Fatal("handler should not be called with invalid key")
		return nil
	}

	md := metadata.Pairs("x-api-key", "mk_invalid-token")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	ss := &mockServerStream{ctx: ctx}

	err := srv.TestableAuthStreamInterceptor(nil, ss, nil, handler)
	if err == nil {
		t.Fatal("expected error for invalid key, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", st.Code())
	}
}

func TestAuthStreamInterceptor_NoKeyPublicVault(t *testing.T) {
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	var capturedVault, capturedMode string
	handler := func(srv any, stream grpc.ServerStream) error {
		ctx := stream.Context()
		capturedVault, _ = ctx.Value(auth.ContextVault).(string)
		capturedMode, _ = ctx.Value(auth.ContextMode).(string)
		return nil
	}

	ctx := context.Background()
	ss := &mockServerStream{ctx: ctx}

	err := srv.TestableAuthStreamInterceptor(nil, ss, nil, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	if capturedVault != "default" {
		t.Errorf("vault = %q, want \"default\"", capturedVault)
	}
	if capturedMode != "full" {
		t.Errorf("mode = %q, want \"full\"", capturedMode)
	}
}

func TestAuthStreamInterceptor_NoKeyLockedVault(t *testing.T) {
	store := newTestAuthStore(t)
	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	handler := func(srv any, stream grpc.ServerStream) error {
		t.Fatal("handler should not be called for locked vault without auth")
		return nil
	}

	ctx := context.Background()
	ss := &mockServerStream{ctx: ctx}

	err := srv.TestableAuthStreamInterceptor(nil, ss, nil, handler)
	if err == nil {
		t.Fatal("expected error for locked vault, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", st.Code())
	}
}

func TestAuthStreamInterceptor_XApiKey(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateAPIKey("default", "test-label", "observe", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	srv := transportgrpc.NewServer(":0", &mockEngine{}, store, nil)

	var capturedMode string
	handler := func(srv any, stream grpc.ServerStream) error {
		ctx := stream.Context()
		capturedMode, _ = ctx.Value(auth.ContextMode).(string)
		return nil
	}

	md := metadata.Pairs("x-api-key", token)
	ctx := metadata.NewIncomingContext(context.Background(), md)
	ss := &mockServerStream{ctx: ctx}

	err = srv.TestableAuthStreamInterceptor(nil, ss, nil, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	if capturedMode != "observe" {
		t.Errorf("mode = %q, want \"observe\"", capturedMode)
	}
}

func TestPublicVaultFullMutatingUnaryRPCsAllowed(t *testing.T) {
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	writeCalled := false
	batchWriteCalled := false
	linkCalled := false
	forgetCalled := false
	srv := transportgrpc.NewServer(":0", &mockEngine{
		writeFn: func(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
			writeCalled = true
			return &pb.WriteResponse{ID: "engram-123"}, nil
		},
		batchWriteFn: func(ctx context.Context, req *pb.BatchWriteRequest) (*pb.BatchWriteResponse, error) {
			batchWriteCalled = true
			return &pb.BatchWriteResponse{}, nil
		},
		linkFn: func(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error) {
			linkCalled = true
			return &pb.LinkResponse{OK: true}, nil
		},
		forgetFn: func(ctx context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error) {
			forgetCalled = true
			return &pb.ForgetResponse{OK: true}, nil
		},
	}, store, nil)

	cases := []struct {
		name   string
		req    any
		invoke func(context.Context, any) (any, error)
		called *bool
	}{
		{
			name:   "Write",
			req:    &pb.WriteRequest{Vault: "default", Concept: "test", Content: "hello"},
			invoke: func(ctx context.Context, req any) (any, error) { return srv.Write(ctx, req.(*pb.WriteRequest)) },
			called: &writeCalled,
		},
		{
			name: "BatchWrite",
			req:  &pb.BatchWriteRequest{Requests: []*pb.WriteRequest{{Vault: "default", Concept: "a", Content: "x"}}},
			invoke: func(ctx context.Context, req any) (any, error) {
				return srv.BatchWrite(ctx, req.(*pb.BatchWriteRequest))
			},
			called: &batchWriteCalled,
		},
		{
			name:   "Link",
			req:    &pb.LinkRequest{Vault: "default", SourceID: "id1", TargetID: "id2", RelType: 1},
			invoke: func(ctx context.Context, req any) (any, error) { return srv.Link(ctx, req.(*pb.LinkRequest)) },
			called: &linkCalled,
		},
		{
			name:   "Forget",
			req:    &pb.ForgetRequest{Vault: "default", ID: "id1"},
			invoke: func(ctx context.Context, req any) (any, error) { return srv.Forget(ctx, req.(*pb.ForgetRequest)) },
			called: &forgetCalled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			*tc.called = false
			_, err := srv.TestableAuthUnaryInterceptor(context.Background(), tc.req, nil, func(ctx context.Context, req any) (any, error) {
				return tc.invoke(ctx, req)
			})
			if err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if !*tc.called {
				t.Fatal("engine method should be called for public full-mode access")
			}
		})
	}
}

func TestObserveKeyMutatingUnaryRPCsDenied(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateAPIKey("default", "observer", auth.ModeObserve, nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	writeCalled := false
	batchWriteCalled := false
	linkCalled := false
	forgetCalled := false
	srv := transportgrpc.NewServer(":0", &mockEngine{
		writeFn: func(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
			writeCalled = true
			return &pb.WriteResponse{ID: "engram-123"}, nil
		},
		batchWriteFn: func(ctx context.Context, req *pb.BatchWriteRequest) (*pb.BatchWriteResponse, error) {
			batchWriteCalled = true
			return &pb.BatchWriteResponse{}, nil
		},
		linkFn: func(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error) {
			linkCalled = true
			return &pb.LinkResponse{OK: true}, nil
		},
		forgetFn: func(ctx context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error) {
			forgetCalled = true
			return &pb.ForgetResponse{OK: true}, nil
		},
	}, store, nil)

	cases := []struct {
		name   string
		req    any
		invoke func(context.Context, any) (any, error)
		called *bool
	}{
		{
			name:   "Write",
			req:    &pb.WriteRequest{Vault: "default", Concept: "test", Content: "hello"},
			invoke: func(ctx context.Context, req any) (any, error) { return srv.Write(ctx, req.(*pb.WriteRequest)) },
			called: &writeCalled,
		},
		{
			name: "BatchWrite",
			req:  &pb.BatchWriteRequest{Requests: []*pb.WriteRequest{{Vault: "default", Concept: "a", Content: "x"}}},
			invoke: func(ctx context.Context, req any) (any, error) {
				return srv.BatchWrite(ctx, req.(*pb.BatchWriteRequest))
			},
			called: &batchWriteCalled,
		},
		{
			name:   "Link",
			req:    &pb.LinkRequest{Vault: "default", SourceID: "id1", TargetID: "id2", RelType: 1},
			invoke: func(ctx context.Context, req any) (any, error) { return srv.Link(ctx, req.(*pb.LinkRequest)) },
			called: &linkCalled,
		},
		{
			name:   "Forget",
			req:    &pb.ForgetRequest{Vault: "default", ID: "id1"},
			invoke: func(ctx context.Context, req any) (any, error) { return srv.Forget(ctx, req.(*pb.ForgetRequest)) },
			called: &forgetCalled,
		},
	}

	md := metadata.Pairs("x-api-key", token)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			*tc.called = false
			ctx := metadata.NewIncomingContext(context.Background(), md)
			_, err := srv.TestableAuthUnaryInterceptor(ctx, tc.req, nil, func(ctx context.Context, req any) (any, error) {
				return tc.invoke(ctx, req)
			})
			if err == nil {
				t.Fatal("expected permission error, got nil")
			}
			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("error is not a gRPC status: %v", err)
			}
			if st.Code() != codes.PermissionDenied {
				t.Fatalf("code = %v, want PermissionDenied", st.Code())
			}
			if st.Message() != "read-only key cannot write" {
				t.Fatalf("message = %q, want %q", st.Message(), "read-only key cannot write")
			}
			if *tc.called {
				t.Fatal("engine method should not be called for observe-mode access")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RPC handler tests — Unary RPCs
// ---------------------------------------------------------------------------

// newPublicTestServer creates a Server with a public default vault, suitable
// for testing RPC handlers without auth ceremony.
func newPublicTestServer(t *testing.T, eng *mockEngine) *transportgrpc.Server {
	t.Helper()
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	return transportgrpc.NewServer(":0", eng, store, nil)
}

func TestHello_Success(t *testing.T) {
	eng := &mockEngine{
		helloFn: func(ctx context.Context, req *pb.HelloRequest) (*pb.HelloResponse, error) {
			return &pb.HelloResponse{ServerVersion: "1.0.0", SessionID: "sess-1"}, nil
		},
	}
	srv := newPublicTestServer(t, eng)

	resp, err := srv.Hello(context.Background(), &pb.HelloRequest{Version: "1.0"})
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if resp.ServerVersion != "1.0.0" {
		t.Errorf("ServerVersion = %q, want \"1.0.0\"", resp.ServerVersion)
	}
	if resp.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want \"sess-1\"", resp.SessionID)
	}
}

func TestHello_Error(t *testing.T) {
	eng := &mockEngine{
		helloFn: func(ctx context.Context, req *pb.HelloRequest) (*pb.HelloResponse, error) {
			return nil, errors.New("engine down")
		},
	}
	srv := newPublicTestServer(t, eng)

	_, err := srv.Hello(context.Background(), &pb.HelloRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestWrite_Success(t *testing.T) {
	eng := &mockEngine{
		writeFn: func(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
			return &pb.WriteResponse{ID: "engram-123", CreatedAt: 1000}, nil
		},
	}
	srv := newPublicTestServer(t, eng)

	resp, err := srv.Write(context.Background(), &pb.WriteRequest{
		Concept: "test", Content: "hello world",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if resp.ID != "engram-123" {
		t.Errorf("ID = %q, want \"engram-123\"", resp.ID)
	}
}

func TestWrite_Error(t *testing.T) {
	eng := &mockEngine{
		writeFn: func(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
			return nil, errors.New("disk full")
		},
	}
	srv := newPublicTestServer(t, eng)

	_, err := srv.Write(context.Background(), &pb.WriteRequest{
		Concept: "test", Content: "hello world",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestBatchWrite_Success(t *testing.T) {
	eng := &mockEngine{}
	srv := newPublicTestServer(t, eng)

	resp, err := srv.BatchWrite(context.Background(), &pb.BatchWriteRequest{
		Requests: []*pb.WriteRequest{
			{Concept: "a", Content: "content a"},
			{Concept: "b", Content: "content b"},
		},
	})
	if err != nil {
		t.Fatalf("BatchWrite: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(resp.Results))
	}
}

func TestRead_Success(t *testing.T) {
	eng := &mockEngine{
		readFn: func(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error) {
			return &pb.ReadResponse{
				ID: req.ID, Concept: "test-concept", Content: "test-content",
			}, nil
		},
	}
	srv := newPublicTestServer(t, eng)

	resp, err := srv.Read(context.Background(), &pb.ReadRequest{ID: "engram-1"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if resp.Concept != "test-concept" {
		t.Errorf("Concept = %q, want \"test-concept\"", resp.Concept)
	}
}

func TestRead_Error(t *testing.T) {
	eng := &mockEngine{
		readFn: func(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error) {
			return nil, errors.New("not found")
		},
	}
	srv := newPublicTestServer(t, eng)

	_, err := srv.Read(context.Background(), &pb.ReadRequest{ID: "missing"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestForget_Success(t *testing.T) {
	eng := &mockEngine{
		forgetFn: func(ctx context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error) {
			return &pb.ForgetResponse{OK: true}, nil
		},
	}
	srv := newPublicTestServer(t, eng)

	resp, err := srv.Forget(context.Background(), &pb.ForgetRequest{ID: "engram-1"})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if !resp.OK {
		t.Error("OK = false, want true")
	}
}

func TestForget_Error(t *testing.T) {
	eng := &mockEngine{
		forgetFn: func(ctx context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error) {
			return nil, errors.New("permission denied")
		},
	}
	srv := newPublicTestServer(t, eng)

	_, err := srv.Forget(context.Background(), &pb.ForgetRequest{ID: "engram-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestStat_Success(t *testing.T) {
	eng := &mockEngine{
		statFn: func(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error) {
			return &pb.StatResponse{EngramCount: 42, StorageBytes: 1024}, nil
		},
	}
	srv := newPublicTestServer(t, eng)

	resp, err := srv.Stat(context.Background(), &pb.StatRequest{})
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if resp.EngramCount != 42 {
		t.Errorf("EngramCount = %d, want 42", resp.EngramCount)
	}
}

func TestStat_Error(t *testing.T) {
	eng := &mockEngine{
		statFn: func(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error) {
			return nil, errors.New("internal error")
		},
	}
	srv := newPublicTestServer(t, eng)

	_, err := srv.Stat(context.Background(), &pb.StatRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestLink_Success(t *testing.T) {
	eng := &mockEngine{
		linkFn: func(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error) {
			return &pb.LinkResponse{OK: true}, nil
		},
	}
	srv := newPublicTestServer(t, eng)

	resp, err := srv.Link(context.Background(), &pb.LinkRequest{
		SourceID: "a", TargetID: "b", RelType: 1, Weight: 0.5,
	})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if !resp.OK {
		t.Error("OK = false, want true")
	}
}

func TestLink_Error(t *testing.T) {
	eng := &mockEngine{
		linkFn: func(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error) {
			return nil, errors.New("invalid source")
		},
	}
	srv := newPublicTestServer(t, eng)

	_, err := srv.Link(context.Background(), &pb.LinkRequest{SourceID: "bad"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Streaming RPC tests
// ---------------------------------------------------------------------------

// mockActivateStream implements pb.MuninnDB_ActivateServer for testing.
type mockActivateStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent []*pb.ActivateResponse
}

func (m *mockActivateStream) Context() context.Context { return m.ctx }
func (m *mockActivateStream) Send(resp *pb.ActivateResponse) error {
	m.sent = append(m.sent, resp)
	return nil
}

func TestActivate_Success(t *testing.T) {
	eng := &mockEngine{
		activateFn: func(ctx context.Context, req *pb.ActivateRequest) (*pb.ActivateResponse, error) {
			return &pb.ActivateResponse{
				QueryID:    "q-1",
				TotalFound: 2,
				Activations: []pb.ActivationItem{
					{ID: "e1", Concept: "concept1", Score: 0.9},
					{ID: "e2", Concept: "concept2", Score: 0.7},
				},
				LatencyMs: 1.5,
			}, nil
		},
	}
	srv := newPublicTestServer(t, eng)

	stream := &mockActivateStream{ctx: context.Background()}
	err := srv.Activate(&pb.ActivateRequest{Context: []string{"test"}}, stream)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent %d responses, want 1", len(stream.sent))
	}
	if stream.sent[0].TotalFound != 2 {
		t.Errorf("TotalFound = %d, want 2", stream.sent[0].TotalFound)
	}
}

func TestActivate_EngineError(t *testing.T) {
	eng := &mockEngine{
		activateFn: func(ctx context.Context, req *pb.ActivateRequest) (*pb.ActivateResponse, error) {
			return nil, errors.New("activation failed")
		},
	}
	srv := newPublicTestServer(t, eng)

	stream := &mockActivateStream{ctx: context.Background()}
	err := srv.Activate(&pb.ActivateRequest{}, stream)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

type errActivateStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (m *errActivateStream) Context() context.Context { return m.ctx }
func (m *errActivateStream) Send(*pb.ActivateResponse) error {
	return errors.New("stream send error")
}

func TestActivate_SendError(t *testing.T) {
	eng := &mockEngine{}
	srv := newPublicTestServer(t, eng)

	stream := &errActivateStream{ctx: context.Background()}
	err := srv.Activate(&pb.ActivateRequest{}, stream)
	if err == nil {
		t.Fatal("expected error from stream send, got nil")
	}
}

// mockSubscribeStream implements pb.MuninnDB_SubscribeServer for testing.
type mockSubscribeStream struct {
	grpc.ServerStream
	ctx     context.Context
	recvReq *pb.SubscribeRequest
	recvErr error
	sent    []*pb.ActivationPush
	sendErr error
}

func (m *mockSubscribeStream) Context() context.Context { return m.ctx }
func (m *mockSubscribeStream) Send(push *pb.ActivationPush) error {
	if m.sendErr != nil {
		return m.sendErr
	}
	m.sent = append(m.sent, push)
	return nil
}
func (m *mockSubscribeStream) Recv() (*pb.SubscribeRequest, error) {
	if m.recvErr != nil {
		return nil, m.recvErr
	}
	return m.recvReq, nil
}

func TestSubscribe_Success(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	eng := &mockEngine{
		subscribeWithDeliverFn: func(ctx context.Context, req *pb.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
			go func() {
				push := &trigger.ActivationPush{
					SubscriptionID: "sub-1",
					Trigger:        trigger.TriggerNewWrite,
					PushNumber:     1,
					At:             time.Now(),
					Engram: &storage.Engram{
						Concept: "test-concept",
						Content: "test-content",
					},
					Score: 0.85,
					Why:   "semantic match",
				}
				_ = deliver(ctx, push)
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()
			return "sub-1", nil
		},
	}
	srv := newPublicTestServer(t, eng)

	stream := &mockSubscribeStream{
		ctx:     ctx,
		recvReq: &pb.SubscribeRequest{Vault: "default", PushOnWrite: true},
	}

	err := srv.Subscribe(stream)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if len(stream.sent) < 1 {
		t.Fatal("expected at least 1 sent message (subscription confirmation)")
	}
	if stream.sent[0].Trigger != "subscription_created" {
		t.Errorf("first push trigger = %q, want \"subscription_created\"", stream.sent[0].Trigger)
	}
	if stream.sent[0].SubscriptionID != "sub-1" {
		t.Errorf("SubscriptionID = %q, want \"sub-1\"", stream.sent[0].SubscriptionID)
	}

	// The second message should be the actual push with engram data.
	if len(stream.sent) >= 2 {
		push := stream.sent[1]
		if push.Activation == nil {
			t.Fatal("expected Activation in push, got nil")
		}
		if push.Activation.Concept != "test-concept" {
			t.Errorf("Activation.Concept = %q, want \"test-concept\"", push.Activation.Concept)
		}
		if push.Activation.Score != 0.85 {
			t.Errorf("Activation.Score = %f, want 0.85", push.Activation.Score)
		}
	}
}

func TestSubscribe_RecvError(t *testing.T) {
	eng := &mockEngine{}
	srv := newPublicTestServer(t, eng)

	stream := &mockSubscribeStream{
		ctx:     context.Background(),
		recvErr: errors.New("client disconnected"),
	}

	err := srv.Subscribe(stream)
	if err == nil {
		t.Fatal("expected error from Recv, got nil")
	}
}

func TestSubscribe_EngineError(t *testing.T) {
	eng := &mockEngine{
		subscribeWithDeliverFn: func(ctx context.Context, req *pb.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
			return "", errors.New("subscribe limit reached")
		},
	}
	srv := newPublicTestServer(t, eng)

	stream := &mockSubscribeStream{
		ctx:     context.Background(),
		recvReq: &pb.SubscribeRequest{Vault: "default"},
	}

	err := srv.Subscribe(stream)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSubscribe_SendConfirmError(t *testing.T) {
	eng := &mockEngine{
		subscribeWithDeliverFn: func(ctx context.Context, req *pb.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
			return "sub-1", nil
		},
	}
	srv := newPublicTestServer(t, eng)

	stream := &mockSubscribeStream{
		ctx:     context.Background(),
		recvReq: &pb.SubscribeRequest{Vault: "default"},
		sendErr: errors.New("broken pipe"),
	}

	err := srv.Subscribe(stream)
	if err == nil {
		t.Fatal("expected error from send, got nil")
	}
}

func TestSubscribe_NilEngram(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	eng := &mockEngine{
		subscribeWithDeliverFn: func(ctx context.Context, req *pb.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
			go func() {
				push := &trigger.ActivationPush{
					SubscriptionID: "sub-1",
					Trigger:        trigger.TriggerNewWrite,
					PushNumber:     1,
					At:             time.Now(),
				}
				_ = deliver(ctx, push)
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()
			return "sub-1", nil
		},
	}
	srv := newPublicTestServer(t, eng)

	stream := &mockSubscribeStream{
		ctx:     ctx,
		recvReq: &pb.SubscribeRequest{Vault: "default"},
	}

	err := srv.Subscribe(stream)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Should have confirmation + push without activation.
	for _, msg := range stream.sent {
		if msg.Trigger == string(trigger.TriggerNewWrite) && msg.Activation != nil {
			t.Error("expected nil Activation for push with nil Engram")
		}
	}
}

// ---------------------------------------------------------------------------
// Shutdown tests
// ---------------------------------------------------------------------------

func TestShutdown_ContextTimeout(t *testing.T) {
	addr := freePort(t)
	eng := &mockEngine{}
	srv := transportgrpc.NewServer(addr, eng, newTestAuthStore(t), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Use an already-expired context so Shutdown must force-stop.
	expiredCtx, expiredCancel := context.WithTimeout(context.Background(), 0)
	defer expiredCancel()
	time.Sleep(5 * time.Millisecond) // let the context expire

	err := srv.Shutdown(expiredCtx)
	if err == nil {
		// GracefulStop may finish before the deadline in a lightly loaded test.
		// Both outcomes (nil or context.DeadlineExceeded) are acceptable.
		return
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown error = %v, want context.DeadlineExceeded", err)
	}
}
