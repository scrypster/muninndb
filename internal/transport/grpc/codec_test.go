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
// versa). This is the path that was broken by the hand-written stubs (#873):
// they lacked proto.Message, so the server's default-proto-codec decode failed
// before reading any wire bytes ("want proto.Message").
//
// Against the hand-written stubs this test fails — every RPC returns
// codes.Internal with the issue's exact "want proto.Message" message.
// Against regenerated (protoc-gen-go) stubs it passes: the mock engine's
// responses round-trip with all fields intact.
//
// Every pre-existing gRPC test constructs DTOs directly in Go or drives mock
// streams that bypass the codec — which is why the broken stubs shipped
// undetected. This is the regression test that gap required.
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
