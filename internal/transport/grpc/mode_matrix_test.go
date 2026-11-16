package grpc_test

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	transportgrpc "github.com/scrypster/muninndb/internal/transport/grpc"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// rpcClass is what an RPC does, as far as a credential's mode is concerned.
type rpcClass int

const (
	// rpcHandshake returns nothing stored and changes nothing.
	rpcHandshake rpcClass = iota
	// rpcRead returns stored memory or vault state.
	rpcRead
	// rpcMutate creates, changes or deletes stored memory.
	rpcMutate
	// rpcMutateAndRead changes stored memory AND returns stored state, so it
	// is both: refused to an observe key as a mutation and to an ingest-only
	// key as a read.
	rpcMutateAndRead
)

// rpcClasses classifies every RPC of the MuninnDB service. A new RPC must be
// added here (TestEveryRPCHasAModeClass fails otherwise), and that entry is
// what TestCredentialModeMatrix enforces: an ingest-only key is refused every
// rpcRead, an observe key every rpcMutate, and both are refused an
// rpcMutateAndRead.
var rpcClasses = map[string]rpcClass{
	"Hello":       rpcHandshake,
	"Write":       rpcMutate,
	"BatchWrite":  rpcMutate,
	"Read":        rpcRead,
	"Forget":      rpcMutate,
	"BatchForget": rpcMutate,
	"Stat":        rpcRead,
	"Link":        rpcMutate,
	"ListVaults":  rpcRead,
	// AdjustConfidence returns the resulting confidence; with delta 0 it changes
	// nothing and is a plain read of one engram's confidence and existence.
	"AdjustConfidence": rpcMutateAndRead,
	"Activate":         rpcRead,
	"Subscribe":        rpcRead,
}

// TestEveryRPCHasAModeClass pins rpcClasses to the service descriptor, so an
// RPC cannot be added to the service without deciding which credential modes
// may call it.
func TestEveryRPCHasAModeClass(t *testing.T) {
	var inService []string
	for _, m := range pb.MuninnDB_ServiceDesc.Methods {
		inService = append(inService, m.MethodName)
	}
	for _, s := range pb.MuninnDB_ServiceDesc.Streams {
		inService = append(inService, s.StreamName)
	}
	sort.Strings(inService)

	var classified []string
	for name := range rpcClasses {
		classified = append(classified, name)
	}
	sort.Strings(classified)

	if len(inService) == 0 {
		t.Fatal("the service descriptor lists no RPCs; this test is looking in the wrong place")
	}
	for _, name := range inService {
		if _, ok := rpcClasses[name]; !ok {
			t.Errorf("RPC %q has no entry in rpcClasses: decide whether it is a handshake, a read or a mutation", name)
		}
	}
	for _, name := range classified {
		found := false
		for _, s := range inService {
			if s == name {
				found = true
			}
		}
		if !found {
			t.Errorf("rpcClasses names %q, which the service no longer has", name)
		}
	}
}

// errStopSubscribe ends a Subscribe handler once it has reached the engine;
// otherwise the handler would wait for pushes.
var errStopSubscribe = errors.New("test: subscription reached the engine")

// modeMatrixServer builds a server whose engine records which RPC reached it.
func modeMatrixServer(t *testing.T, store *auth.Store) (*transportgrpc.Server, map[string]*bool) {
	t.Helper()
	reached := map[string]*bool{}
	for name := range rpcClasses {
		reached[name] = new(bool)
	}
	eng := &mockEngine{
		helloFn: func(context.Context, *pb.HelloRequest) (*pb.HelloResponse, error) {
			*reached["Hello"] = true
			return &pb.HelloResponse{}, nil
		},
		writeFn: func(context.Context, *pb.WriteRequest) (*pb.WriteResponse, error) {
			*reached["Write"] = true
			return &pb.WriteResponse{}, nil
		},
		batchWriteFn: func(context.Context, *pb.BatchWriteRequest) (*pb.BatchWriteResponse, error) {
			*reached["BatchWrite"] = true
			return &pb.BatchWriteResponse{}, nil
		},
		readFn: func(context.Context, *pb.ReadRequest) (*pb.ReadResponse, error) {
			*reached["Read"] = true
			return &pb.ReadResponse{}, nil
		},
		forgetFn: func(context.Context, *pb.ForgetRequest) (*pb.ForgetResponse, error) {
			*reached["Forget"] = true
			return &pb.ForgetResponse{}, nil
		},
		batchForgetFn: func(context.Context, *pb.BatchForgetRequest) (*pb.BatchForgetResponse, error) {
			*reached["BatchForget"] = true
			return &pb.BatchForgetResponse{}, nil
		},
		statFn: func(context.Context, *pb.StatRequest) (*pb.StatResponse, error) {
			*reached["Stat"] = true
			return &pb.StatResponse{}, nil
		},
		linkFn: func(context.Context, *pb.LinkRequest) (*pb.LinkResponse, error) {
			*reached["Link"] = true
			return &pb.LinkResponse{}, nil
		},
		listVaultsFn: func(context.Context, *pb.ListVaultsRequest) (*pb.ListVaultsResponse, error) {
			*reached["ListVaults"] = true
			return &pb.ListVaultsResponse{}, nil
		},
		adjustConfidenceFn: func(context.Context, *pb.AdjustConfidenceRequest) (*pb.AdjustConfidenceResponse, error) {
			*reached["AdjustConfidence"] = true
			return &pb.AdjustConfidenceResponse{}, nil
		},
		activateFn: func(context.Context, *pb.ActivateRequest) (*pb.ActivateResponse, error) {
			*reached["Activate"] = true
			return &pb.ActivateResponse{}, nil
		},
		subscribeWithDeliverFn: func(context.Context, *pb.SubscribeRequest, trigger.DeliverFunc) (string, error) {
			*reached["Subscribe"] = true
			return "", errStopSubscribe
		},
	}
	return transportgrpc.NewServer(":0", eng, store, nil), reached
}

// callRPC sends one request for the named RPC through the real auth
// interceptor (unary or stream) with the given API key, and returns the
// handler's error. Requests set only the vault and free-text fields.
func callRPC(t *testing.T, srv *transportgrpc.Server, name, token string) error {
	t.Helper()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", token))

	unary := func(req any, invoke func(context.Context) (any, error)) error {
		_, err := srv.TestableAuthUnaryInterceptor(ctx, req, nil, func(ctx context.Context, _ any) (any, error) {
			return invoke(ctx)
		})
		return err
	}

	switch name {
	case "Hello":
		req := &pb.HelloRequest{Version: "1"}
		return unary(req, func(ctx context.Context) (any, error) { return srv.Hello(ctx, req) })
	case "Write":
		req := &pb.WriteRequest{Vault: "default", Concept: "c", Content: "x"}
		return unary(req, func(ctx context.Context) (any, error) { return srv.Write(ctx, req) })
	case "BatchWrite":
		req := &pb.BatchWriteRequest{Requests: []*pb.WriteRequest{{Vault: "default", Concept: "c", Content: "x"}}}
		return unary(req, func(ctx context.Context) (any, error) { return srv.BatchWrite(ctx, req) })
	case "Read":
		req := &pb.ReadRequest{Vault: "default"}
		return unary(req, func(ctx context.Context) (any, error) { return srv.Read(ctx, req) })
	case "Forget":
		req := &pb.ForgetRequest{Vault: "default"}
		return unary(req, func(ctx context.Context) (any, error) { return srv.Forget(ctx, req) })
	case "BatchForget":
		req := &pb.BatchForgetRequest{Requests: []*pb.ForgetRequest{{Vault: "default"}}}
		return unary(req, func(ctx context.Context) (any, error) { return srv.BatchForget(ctx, req) })
	case "Stat":
		req := &pb.StatRequest{Vault: "default"}
		return unary(req, func(ctx context.Context) (any, error) { return srv.Stat(ctx, req) })
	case "Link":
		req := &pb.LinkRequest{Vault: "default"}
		return unary(req, func(ctx context.Context) (any, error) { return srv.Link(ctx, req) })
	case "ListVaults":
		req := &pb.ListVaultsRequest{}
		return unary(req, func(ctx context.Context) (any, error) { return srv.ListVaults(ctx, req) })
	case "AdjustConfidence":
		req := &pb.AdjustConfidenceRequest{Vault: "default"}
		return unary(req, func(ctx context.Context) (any, error) { return srv.AdjustConfidence(ctx, req) })
	case "Activate":
		return srv.TestableAuthStreamInterceptor(nil, &mockServerStream{ctx: ctx}, nil, func(_ any, ss grpc.ServerStream) error {
			return srv.Activate(&pb.ActivateRequest{Vault: "default", Context: []string{"q"}}, &mockActivateStream{ctx: ss.Context()})
		})
	case "Subscribe":
		return srv.TestableAuthStreamInterceptor(nil, &mockServerStream{ctx: ctx}, nil, func(_ any, ss grpc.ServerStream) error {
			return srv.Subscribe(&mockSubscribeStream{ctx: ss.Context(), recvReq: &pb.SubscribeRequest{Vault: "default", Context: []string{"q"}}})
		})
	}
	t.Fatalf("callRPC has no request for RPC %q: add one", name)
	return nil
}

// TestCredentialModeMatrix pins what each credential mode may do over gRPC, for
// every RPC of the service:
//
//   - write (ingest-only): refused every read — the gRPC counterpart of REST's
//     auth.WriteOnlyGuard and MCP's write-mode tool gate. Before this guard an
//     ingest-only key could Read, Activate, Stat, ListVaults and Subscribe, and
//     could read an engram's confidence through AdjustConfidence.
//   - observe: refused every mutation.
//   - full and append: nothing is refused at the transport. What an append key
//     may not modify is enforced in the engine, on every transport.
//
// A refusal is PermissionDenied and the engine is never called.
func TestCredentialModeMatrix(t *testing.T) {
	const (
		writeOnlyMsg = "write-only key cannot read"
		readOnlyMsg  = "read-only key cannot write"
	)
	refusal := func(mode string, class rpcClass) string {
		switch {
		case mode == auth.ModeWrite && (class == rpcRead || class == rpcMutateAndRead):
			return writeOnlyMsg
		case mode == auth.ModeObserve && (class == rpcMutate || class == rpcMutateAndRead):
			return readOnlyMsg
		}
		return ""
	}

	names := make([]string, 0, len(rpcClasses))
	for name := range rpcClasses {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, mode := range []string{auth.ModeFull, auth.ModeObserve, auth.ModeWrite, auth.ModeAppend} {
		t.Run(mode, func(t *testing.T) {
			store := newTestAuthStore(t)
			token, _, err := store.GenerateAPIKey("default", "matrix-"+mode, mode, nil)
			if err != nil {
				t.Fatalf("GenerateAPIKey(%s): %v", mode, err)
			}
			srv, reached := modeMatrixServer(t, store)

			for _, name := range names {
				t.Run(name, func(t *testing.T) {
					*reached[name] = false
					err := callRPC(t, srv, name, token)
					want := refusal(mode, rpcClasses[name])

					if want == "" {
						if !*reached[name] {
							t.Fatalf("a %s key was stopped before the engine (err = %v); want the call to go through", mode, err)
						}
						return
					}
					if *reached[name] {
						t.Fatalf("a %s key reached the engine; want it refused with %q", mode, want)
					}
					st, ok := status.FromError(err)
					if !ok || st.Code() != codes.PermissionDenied {
						t.Fatalf("err = %v; want PermissionDenied", err)
					}
					if st.Message() != want {
						t.Fatalf("message = %q; want %q", st.Message(), want)
					}
				})
			}
		})
	}
}
