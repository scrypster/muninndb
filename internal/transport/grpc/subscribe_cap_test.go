package grpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// openSubscribe opens one Subscribe stream and reports how the server answered
// it: nil once the subscription is confirmed, or the error it was refused with.
// cancel ends the stream.
func openSubscribe(t *testing.T, client pb.MuninnDBClient, vault, token string) (cancel context.CancelFunc, err error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", token)
	}
	st, err := client.Subscribe(ctx)
	if err != nil {
		return cancel, err
	}
	if err := st.Send(&pb.SubscribeRequest{Vault: vault, Context: []string{"tide tables"}}); err != nil {
		return cancel, err
	}
	if _, err := st.Recv(); err != nil {
		return cancel, err
	}
	return cancel, nil
}

// TestOneConnectionCannotFillAVaultsSubscriptions pins that one gRPC connection
// holds a bounded number of Subscribe streams at a time (SEC-26). A vault
// allows 100 subscriptions in all, and one connection, with one key, used to be
// able to take every one of them: 100 of 110 streams were accepted and the next
// subscriber, on any transport, was refused.
func TestOneConnectionCannotFillAVaultsSubscriptions(t *testing.T) {
	const vault = "pilot-station"
	p := newPushServer(t, nil)
	if err := p.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: false}); err != nil {
		t.Fatal(err)
	}
	token, _, err := p.store.GenerateAPIKey(vault, "streams", auth.ModeObserve, nil)
	if err != nil {
		t.Fatal(err)
	}

	greedy := p.dial(t)
	var cancels []context.CancelFunc
	accepted := 0
	var refusal error
	for i := 0; i < 110; i++ {
		cancel, err := openSubscribe(t, greedy, vault, token)
		if err != nil {
			refusal = err
			continue
		}
		accepted++
		cancels = append(cancels, cancel)
	}
	t.Logf("one connection, one key: %d of 110 Subscribe streams accepted; the rest refused with: %v", accepted, refusal)
	if accepted > 8 {
		t.Errorf("one connection holds %d of the 100 subscriptions the vault allows", accepted)
	}
	if accepted == 0 {
		t.Fatalf("no stream was accepted at all: %v", refusal)
	}
	if status.Code(refusal) != codes.ResourceExhausted {
		t.Errorf("the streams past the limit ended with %v; want ResourceExhausted", refusal)
	}
	p.awaitSubscriptions(t, vault, accepted, "with the greedy connection's streams open")

	// Somebody else, on a connection of their own, can still subscribe.
	if _, err := openSubscribe(t, p.dial(t), vault, token); err != nil {
		t.Errorf("another connection was refused after one had its fill: %v", err)
	}

	// Ending a stream gives the connection room for another.
	cancels[0]()
	p.awaitSubscriptions(t, vault, accepted, "after the greedy connection ended one stream (its own count, plus the other connection's one)")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := openSubscribe(t, greedy, vault, token); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the connection could not open a stream after ending one: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
