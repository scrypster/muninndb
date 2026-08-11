package grpc_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	transportgrpc "github.com/scrypster/muninndb/internal/transport/grpc"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestGRPCWireRoundTrip exercises the gRPC proto codec end-to-end: a real
// grpc.NewClient sends requests over TCP, so every request/response traverses
// proto.Marshal on the client and proto.Unmarshal on the server (and vice
// versa) — including the server-streaming Activate path, which crosses the
// streaming codec (grpc.ServerStreamingServer Send / client stream Recv)
// rather than a mock.
//
// This is the path that was broken by the hand-written stubs (#873): they
// lacked proto.Message, so the codec rejected every message before any wire
// bytes were read. Every pre-existing gRPC test constructs DTOs directly in Go
// or drives mock streams that bypass the codec — which is why the broken stubs
// shipped undetected.
//
// The GREEN above is this test (regenerated stubs round-trip every RPC). The
// RED proof — that the hand-written stubs were broken — is reproduced
// separately: a minimal Hello-only wire test against the pre-fix stubs fails
// with the codec rejecting the message at the marshal stage for want of
// proto.Message, the same root cause as #873's server-side unmarshal failure.
func TestGRPCWireRoundTrip(t *testing.T) {
	eng := &mockEngine{
		helloFn: func(ctx context.Context, req *pb.HelloRequest) (*pb.HelloResponse, error) {
			return &pb.HelloResponse{ServerVersion: "test-wire", SessionId: "sess-wire"}, nil
		},
		writeFn: func(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
			return &pb.WriteResponse{Id: "01TESTWIRE000000000000000A", CreatedAt: 42}, nil
		},
		readFn: func(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error) {
			return &pb.ReadResponse{Id: req.Id, Concept: "wire-concept", Content: "wire-content"}, nil
		},
		statFn: func(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error) {
			return &pb.StatResponse{EngramCount: 7, VaultCount: 1}, nil
		},
		activateFn: func(ctx context.Context, req *pb.ActivateRequest) (*pb.ActivateResponse, error) {
			return &pb.ActivateResponse{
				QueryId:    "q-wire",
				TotalFound: 1,
				Activations: []*pb.ActivationItem{
					{Id: "a1", Concept: "wire-activation", Score: 0.9},
				},
			}, nil
		},
	}

	// Public default vault → requests need no API key. The test exercises the
	// codec, not auth. Mirrors newPublicTestServer's setup.
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	addr := freePort(t)
	srv := transportgrpc.NewServer(addr, eng, store, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()

	// Confirm the listener is accepting before dialing, without the deprecated
	// grpc.WithBlock (golangci-lint SA1019). A fixed sleep would race the bind.
	waitListening(t, addr, 2*time.Second)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()
	client := pb.NewMuninnDBClient(conn)

	// Hello round-trip.
	hello, err := client.Hello(ctx, &pb.HelloRequest{Client: "wire-test"})
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if hello.ServerVersion != "test-wire" {
		t.Fatalf("Hello.ServerVersion = %q, want %q", hello.ServerVersion, "test-wire")
	}

	// Write round-trip (exercises a float-bearing message).
	wresp, err := client.Write(ctx, &pb.WriteRequest{Concept: "c", Content: "x", Confidence: 0.5})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if wresp.Id != "01TESTWIRE000000000000000A" {
		t.Fatalf("Write.Id = %q, want 01TESTWIRE000000000000000A", wresp.Id)
	}

	// Read round-trip (verifies the response payload survives marshal/unmarshal).
	rr, err := client.Read(ctx, &pb.ReadRequest{Id: "01TESTWIRE000000000000000A"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rr.Concept != "wire-concept" || rr.Content != "wire-content" {
		t.Fatalf("Read payload corrupted: concept=%q content=%q", rr.Concept, rr.Content)
	}

	// Stat round-trip (int64 fields).
	st, err := client.Stat(ctx, &pb.StatRequest{})
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.EngramCount != 7 {
		t.Fatalf("Stat.EngramCount = %d, want 7", st.EngramCount)
	}

	// Activate round-trip — SERVER-STREAMING. This is the path the pre-existing
	// tests bypassed with mock streams: here ActivateResponse marshals on the
	// server's grpc.ServerStreamingServer and unmarshals on the client stream,
	// proving the regenerated []*ActivationItem (pointer slice) repeated field
	// crosses the real codec intact.
	astream, err := client.Activate(ctx, &pb.ActivateRequest{Context: []string{"wire"}, MaxResults: 5})
	if err != nil {
		t.Fatalf("Activate open stream: %v", err)
	}
	ar, err := astream.Recv()
	if err != nil {
		t.Fatalf("Activate Recv: %v", err)
	}
	if ar.TotalFound != 1 || len(ar.Activations) != 1 || ar.Activations[0].Concept != "wire-activation" {
		t.Fatalf("Activate payload corrupted: total=%d acts=%d concept=%q",
			ar.TotalFound, len(ar.Activations), func() string {
				if len(ar.Activations) > 0 {
					return ar.Activations[0].Concept
				}
				return ""
			}())
	}
}

// TestGRPCWireRoundTrip_BatchWrite_GetVault pins the relocated
// BatchWriteRequest.GetVault() (proto/gen/go/muninn/v1/batch_extra.go) against
// the real *pb.BatchWriteRequest type and the auth interceptor's vaultNamer
// path, over the real wire. The getter is hand-maintained and deliberately
// exempt from `make proto` (protoc-gen-go cannot generate it), so without a
// runtime test a future hand-edit would compile and pass while silently
// degrading unkeyed BatchWrite vault resolution.
//
// Setup makes ONLY "wire-batch" public; "default" is left locked. The no-token
// interceptor must therefore resolve the vault from the request itself via the
// relocated GetVault() (returning the first item's "wire-batch"). If GetVault
// were broken/removed, the interceptor would fall back to "default" and reject
// the request as Unauthenticated — which is exactly the failure this asserts
// does NOT happen.
func TestGRPCWireRoundTrip_BatchWrite_GetVault(t *testing.T) {
	var gotVault string
	eng := &mockEngine{
		batchWriteFn: func(ctx context.Context, req *pb.BatchWriteRequest) (*pb.BatchWriteResponse, error) {
			if len(req.Requests) > 0 {
				gotVault = req.Requests[0].GetVault()
			}
			return &pb.BatchWriteResponse{Results: []*pb.BatchWriteItemResult{{Index: 0, Id: "bw-1"}}}, nil
		},
	}
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "wire-batch", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig wire-batch: %v", err)
	}
	// "default" deliberately left unconfigured → fail-closed. Forces the
	// interceptor to resolve "wire-batch" via GetVault, not fall back to default.

	addr := freePort(t)
	srv := transportgrpc.NewServer(addr, eng, store, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	waitListening(t, addr, 2*time.Second)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()
	client := pb.NewMuninnDBClient(conn)

	resp, err := client.BatchWrite(ctx, &pb.BatchWriteRequest{Requests: []*pb.WriteRequest{
		{Concept: "c", Content: "x", Vault: "wire-batch"},
	}})
	if err != nil {
		t.Fatalf("BatchWrite: %v (expected GetVault to resolve the public 'wire-batch' vault; "+
			"an Unauthenticated here means BatchWriteRequest.GetVault() is broken)", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Id != "bw-1" {
		t.Fatalf("BatchWrite result = %+v, want one item Id=bw-1", resp.Results)
	}
	if gotVault != "wire-batch" {
		t.Fatalf("engine saw vault %q, want %q (BatchWriteRequest.GetVault must return first item's vault)",
			gotVault, "wire-batch")
	}
}

// waitListening polls addr until it accepts a TCP connection or the deadline
// passes. Keeps the test off deprecated grpc.WithBlock while avoiding a fixed
// sleep that can race the server's bind.
func waitListening(t *testing.T, addr string, deadline time.Duration) {
	t.Helper()
	deadlineCh := time.After(deadline)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return
		}
		select {
		case <-deadlineCh:
			t.Fatalf("server at %s did not start listening within %s", addr, deadline)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
}
