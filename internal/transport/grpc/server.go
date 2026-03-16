package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// EngineAPI is the interface the gRPC server requires from the engine.
type EngineAPI interface {
	Hello(ctx context.Context, req *pb.HelloRequest) (*pb.HelloResponse, error)
	Write(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error)
	BatchWrite(ctx context.Context, req *pb.BatchWriteRequest) (*pb.BatchWriteResponse, error)
	Read(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error)
	Activate(ctx context.Context, req *pb.ActivateRequest) (*pb.ActivateResponse, error)
	Link(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error)
	Forget(ctx context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error)
	Stat(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error)
	Subscribe(ctx context.Context, req *pb.SubscribeRequest) (*pb.SubscribeResponse, error)
	SubscribeWithDeliver(ctx context.Context, req *pb.SubscribeRequest, deliver trigger.DeliverFunc) (string, error)
	Unsubscribe(ctx context.Context, subID string) error
}

// Server implements the MuninnDB gRPC service.
type Server struct {
	pb.UnimplementedMuninnDBServer
	addr      string
	engine    EngineAPI
	authStore *auth.Store
	gs        *grpc.Server
	tlsConfig *tls.Config // nil = plain TCP
}

func denyReadOnlyMutation(ctx context.Context) error {
	if auth.ReadOnlyFromContext(ctx) {
		return status.Error(codes.PermissionDenied, "read-only key cannot write")
	}
	return nil
}

// NewServer creates a new gRPC server.
// authStore is required and used to validate API keys on every inbound RPC.
// tlsConfig, if non-nil, enables TLS using gRPC transport credentials.
func NewServer(addr string, engine EngineAPI, authStore *auth.Store, tlsConfig *tls.Config) *Server {
	kasp := keepalive.ServerParameters{
		Time:              10 * time.Second, // seconds, ping interval
		Timeout:           5 * time.Second,  // seconds, ping timeout
		MaxConnectionIdle: 5 * time.Minute,  // 5 minutes
		MaxConnectionAge:  30 * time.Minute, // 30 minutes
	}

	server := &Server{
		addr:      addr,
		engine:    engine,
		authStore: authStore,
		tlsConfig: tlsConfig,
	}

	opts := []grpc.ServerOption{
		grpc.MaxConcurrentStreams(500),
		grpc.KeepaliveParams(kasp),
		grpc.UnaryInterceptor(server.authUnaryInterceptor),
		grpc.StreamInterceptor(server.authStreamInterceptor),
	}
	if tlsConfig != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfig)))
		slog.Info("grpc: TLS enabled", "addr", addr)
	}

	gs := grpc.NewServer(opts...)
	server.gs = gs

	pb.RegisterMuninnDBServer(gs, server)
	return server
}

// authUnaryInterceptor is a gRPC unary server interceptor that enforces API key
// authentication. It mirrors the logic of auth.VaultAuthMiddleware:
//
//   - If an "authorization" or "x-api-key" metadata value is present, the token is
//     validated via the auth store. An invalid token always results in
//     codes.Unauthenticated regardless of vault visibility.
//   - If no token is provided, the vault config is consulted. Non-public vaults
//     (including unconfigured vaults, which default to fail-closed) require a key.
//     Public vaults run in full mode.
//
// The vault name is read from the request if it implements VaultNamer; otherwise
// "default" is used.
func (s *Server) authUnaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	vault := "default"
	type vaultNamer interface{ GetVault() string }
	if vn, ok := req.(vaultNamer); ok {
		if v := vn.GetVault(); v != "" {
			vault = v
		}
	}

	var token string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("authorization"); len(vals) > 0 {
			token, _ = strings.CutPrefix(vals[0], "Bearer ")
		}
		if token == "" {
			if vals := md.Get("x-api-key"); len(vals) > 0 {
				token = vals[0]
			}
		}
	}

	if token != "" {
		key, err := s.authStore.ValidateAPIKey(token)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid api key")
		}
		// Inject auth context values so downstream handlers can inspect them.
		ctx = context.WithValue(ctx, auth.ContextVault, key.Vault)
		ctx = context.WithValue(ctx, auth.ContextMode, key.Mode)
		ctx = context.WithValue(ctx, auth.ContextAPIKey, &key)
		return handler(ctx, req)
	}

	// No token — check vault config (fail-closed: unconfigured vaults require a key).
	cfg, err := s.authStore.GetVaultConfig(vault)
	if err != nil || !cfg.Public {
		return nil, status.Errorf(codes.Unauthenticated, "vault %q requires an API key", vault)
	}

	ctx = context.WithValue(ctx, auth.ContextVault, vault)
	ctx = context.WithValue(ctx, auth.ContextMode, auth.ModeFull)
	return handler(ctx, req)
}

// wrappedStream wraps a grpc.ServerStream and overrides its context so that
// auth values injected by authStreamInterceptor are visible to handlers.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// authStreamInterceptor is a gRPC stream server interceptor that enforces API key
// authentication for streaming RPCs (Activate, Subscribe). Because the first
// message has not yet been received when the interceptor runs, the vault is
// always resolved as "default". The auth logic otherwise mirrors
// authUnaryInterceptor.
func (s *Server) authStreamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := ss.Context()

	var token string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("authorization"); len(vals) > 0 {
			token, _ = strings.CutPrefix(vals[0], "Bearer ")
		}
		if token == "" {
			if vals := md.Get("x-api-key"); len(vals) > 0 {
				token = vals[0]
			}
		}
	}

	if token != "" {
		key, err := s.authStore.ValidateAPIKey(token)
		if err != nil {
			return status.Error(codes.Unauthenticated, "invalid api key")
		}
		ctx = context.WithValue(ctx, auth.ContextVault, key.Vault)
		ctx = context.WithValue(ctx, auth.ContextMode, key.Mode)
		ctx = context.WithValue(ctx, auth.ContextAPIKey, &key)
		return handler(srv, &wrappedStream{ss, ctx})
	}

	// No token — check default vault config (fail-closed). Public default vault
	// requests run in full mode.
	cfg, err := s.authStore.GetVaultConfig("default")
	if err != nil || !cfg.Public {
		return status.Errorf(codes.Unauthenticated, "vault %q requires an API key", "default")
	}

	ctx = context.WithValue(ctx, auth.ContextVault, "default")
	ctx = context.WithValue(ctx, auth.ContextMode, auth.ModeFull)
	return handler(srv, &wrappedStream{ss, ctx})
}

// Serve starts listening and blocks until context is cancelled or Shutdown is called.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	slog.Info("grpc: listening", "addr", listener.Addr().String())
	defer listener.Close()

	go s.gs.Serve(listener)

	<-ctx.Done()
	s.gs.GracefulStop()
	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.gs.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.gs.Stop()
		return ctx.Err()
	}
}

// RPC Handlers

// Hello implements the Hello RPC.
func (s *Server) Hello(ctx context.Context, req *pb.HelloRequest) (*pb.HelloResponse, error) {
	resp, err := s.engine.Hello(ctx, req)
	if err != nil {
		slog.Error("hello failed", "error", err)
		return nil, err
	}
	return resp, nil
}

// Write implements the Write RPC.
func (s *Server) Write(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
	if err := denyReadOnlyMutation(ctx); err != nil {
		return nil, err
	}
	resp, err := s.engine.Write(ctx, req)
	if err != nil {
		slog.Error("write failed", "error", err)
		return nil, err
	}
	return resp, nil
}

// BatchWrite implements the BatchWrite RPC.
func (s *Server) BatchWrite(ctx context.Context, req *pb.BatchWriteRequest) (*pb.BatchWriteResponse, error) {
	if err := denyReadOnlyMutation(ctx); err != nil {
		return nil, err
	}
	resp, err := s.engine.BatchWrite(ctx, req)
	if err != nil {
		slog.Error("batch write failed", "error", err)
		return nil, err
	}
	return resp, nil
}

// Read implements the Read RPC.
func (s *Server) Read(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error) {
	resp, err := s.engine.Read(ctx, req)
	if err != nil {
		slog.Error("read failed", "error", err)
		return nil, err
	}
	return resp, nil
}

// Forget implements the Forget RPC.
func (s *Server) Forget(ctx context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error) {
	if err := denyReadOnlyMutation(ctx); err != nil {
		return nil, err
	}
	resp, err := s.engine.Forget(ctx, req)
	if err != nil {
		slog.Error("forget failed", "error", err)
		return nil, err
	}
	return resp, nil
}

// Stat implements the Stat RPC.
func (s *Server) Stat(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error) {
	resp, err := s.engine.Stat(ctx, req)
	if err != nil {
		slog.Error("stat failed", "error", err)
		return nil, err
	}
	return resp, nil
}

// Link implements the Link RPC.
func (s *Server) Link(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error) {
	if err := denyReadOnlyMutation(ctx); err != nil {
		return nil, err
	}
	resp, err := s.engine.Link(ctx, req)
	if err != nil {
		slog.Error("link failed", "error", err)
		return nil, err
	}
	return resp, nil
}

// Activate implements the Activate RPC (server-streaming).
func (s *Server) Activate(req *pb.ActivateRequest, stream pb.MuninnDB_ActivateServer) error {
	ctx := stream.Context()
	resp, err := s.engine.Activate(ctx, req)
	if err != nil {
		slog.Error("activate failed", "error", err)
		return err
	}

	// Send response to client
	if err := stream.Send(resp); err != nil {
		slog.Error("send activate response failed", "error", err)
		return err
	}

	return nil
}

// Subscribe implements the Subscribe RPC (bidirectional streaming).
// The client sends one SubscribeRequest; the server streams ActivationPush
// messages until the client disconnects or the subscription TTL expires.
func (s *Server) Subscribe(stream pb.MuninnDB_SubscribeServer) error {
	ctx := stream.Context()

	req, err := stream.Recv()
	if err != nil {
		return err
	}

	// Buffered push channel. The deliver func is non-blocking: it drops the push
	// if the channel is full so the trigger worker goroutine is never blocked.
	// When the stream context is cancelled the deliver func returns an error,
	// which causes DeliveryRouter to remove the subscription automatically.
	pushCh := make(chan *trigger.ActivationPush, 32)

	deliver := func(ctx context.Context, push *trigger.ActivationPush) error {
		select {
		case pushCh <- push:
			return nil
		case <-ctx.Done():
			// Stream closed — signal DeliveryRouter to remove this subscription.
			return ctx.Err()
		default:
			// Client too slow — drop this push, keep subscription alive.
			return nil
		}
	}

	subID, err := s.engine.SubscribeWithDeliver(ctx, req, deliver)
	if err != nil {
		slog.Error("subscribe failed", "error", err)
		return err
	}
	defer s.engine.Unsubscribe(ctx, subID)

	// Confirm subscription to the client.
	if err := stream.Send(&pb.ActivationPush{
		SubscriptionID: subID,
		Trigger:        "subscription_created",
		At:             time.Now().UnixNano(),
	}); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case push, ok := <-pushCh:
			if !ok {
				return nil
			}
			pbPush := &pb.ActivationPush{
				SubscriptionID: push.SubscriptionID,
				Trigger:        string(push.Trigger),
				PushNumber:     int32(push.PushNumber),
				At:             push.At.UnixNano(),
			}
			if push.Engram != nil {
				pbPush.Activation = &pb.ActivationItem{
					ID:      push.Engram.ID.String(),
					Concept: push.Engram.Concept,
					Content: push.Engram.Content,
					Score:   float32(push.Score),
					Why:     push.Why,
				}
			}
			if err := stream.Send(pbPush); err != nil {
				slog.Error("grpc send push failed", "error", err)
				return err
			}
		}
	}
}
