package grpc_test

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	transportgrpc "github.com/scrypster/muninndb/internal/transport/grpc"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// pushServer is a gRPC server on a loopback port over a real engine whose
// trigger system is running, so a write really is pushed to the streams
// subscribed to its vault. Fixture: a fictional harbour pilot station.
type pushServer struct {
	eng      *engine.Engine
	store    *auth.Store
	pstore   *storage.PebbleStore
	triggers *trigger.TriggerSystem
	addr     string
}

func newPushServer(t *testing.T, configure func(*transportgrpc.Server)) *pushServer {
	t.Helper()
	opts := storage.DefaultOptions()
	opts.FS = vfs.NewMem()
	db, err := storage.OpenPebble("grpc-push", opts)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := activation.NewNoopEmbedder()
	authStore := auth.NewStore(db)
	triggers := trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, embedder)
	ctx, cancel := context.WithCancel(context.Background())
	go triggers.Start(ctx)
	eng := engine.NewEngine(engine.EngineConfig{
		Store: store, AuthStore: authStore, FTSIndex: ftsIdx,
		ActivationEngine: activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, embedder),
		TriggerSystem:    triggers,
		Embedder:         embedder,
	})
	addr := freePort(t)
	srv := transportgrpc.NewServer(addr, transportgrpc.NewEngineAdapter(eng), authStore, nil)
	if configure != nil {
		configure(srv)
	}
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ctx) }()
	waitListening(t, addr, 3*time.Second)
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
		}
		eng.Stop()
		store.Close()
	})
	return &pushServer{eng: eng, store: authStore, pstore: store, triggers: triggers, addr: addr}
}

// dial opens a client connection of its own.
func (p *pushServer) dial(t *testing.T) pb.MuninnDBClient {
	t.Helper()
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewMuninnDBClient(conn)
}

func (p *pushServer) write(t *testing.T, vault, berth string) {
	t.Helper()
	full := context.WithValue(context.Background(), auth.ContextMode, auth.ModeFull)
	_, err := p.eng.Write(full, &mbp.WriteRequest{
		Vault: vault, Concept: "berth notice " + berth, Content: "berth " + berth + " closed for dredging",
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (p *pushServer) subscriptions(vault string) int {
	ws := p.pstore.ResolveVaultPrefix(vault)
	return len(p.triggers.ForVault(binary.BigEndian.Uint32(ws[:4])))
}

func (p *pushServer) awaitSubscriptions(t *testing.T, vault string, want int, when string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for p.subscriptions(vault) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: vault %s has %d subscriptions; want %d", when, vault, p.subscriptions(vault), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pushStream is one open Subscribe stream.
type pushStream struct {
	results chan pushResult
}

type pushResult struct {
	push *pb.ActivationPush
	err  error
}

// subscribe opens a Subscribe stream on vault and waits for its confirmation.
// An empty token is a call with no credential.
func (p *pushServer) subscribe(t *testing.T, client pb.MuninnDBClient, vault, token string) *pushStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", token)
	}
	st, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := st.Send(&pb.SubscribeRequest{Vault: vault, Context: []string{"berth"}, PushOnWrite: true, Threshold: 0.01}); err != nil {
		t.Fatalf("Subscribe send: %v", err)
	}
	s := &pushStream{results: make(chan pushResult, 64)}
	go func() {
		for {
			push, err := st.Recv()
			s.results <- pushResult{push, err}
			if err != nil {
				return
			}
		}
	}()
	first := s.next(5 * time.Second)
	if first.err != nil || first.push.GetTrigger() != "subscription_created" {
		t.Fatalf("first message = %+v; want the subscription confirmation", first)
	}
	return s
}

// next returns the next thing the stream yields. A stream that stays silent for
// the whole wait yields a nil push and a nil error.
func (s *pushStream) next(wait time.Duration) pushResult {
	select {
	case r := <-s.results:
		return r
	case <-time.After(wait):
		return pushResult{}
	}
}

func carriesMemory(p *pb.ActivationPush) bool {
	return p != nil && p.Activation != nil && p.Activation.Content != ""
}

// endsWithout waits for the server to end the stream and returns the error it
// ended with. It fails if a push carrying a memory arrives first.
func (s *pushStream) endsWithout(t *testing.T, what string) error {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := s.next(time.Until(deadline))
		switch {
		case r.err != nil:
			return r.err
		case carriesMemory(r.push):
			t.Fatalf("%s: a push carrying a memory was delivered: trigger=%s content=%q", what, r.push.Trigger, r.push.Activation.Content)
		case r.push == nil:
			t.Fatalf("%s: the stream was still open after 10s", what)
		}
	}
}

// TestSubscribeStreamEndsWhenItsAuthorisationDoes pins that an open gRPC
// Subscribe stream is authorised for as long as what authorised it holds, not
// once when it opened (SEC-23). Before this, a stream opened with a key went on
// delivering pushes, memory content included, after the key had stopped
// validating.
func TestSubscribeStreamEndsWhenItsAuthorisationDoes(t *testing.T) {
	const locked, public = "pilot-station", "default"

	setup := func(t *testing.T, recheck time.Duration) *pushServer {
		p := newPushServer(t, func(s *transportgrpc.Server) {
			if recheck > 0 {
				s.SetStreamRecheckForTest(recheck)
			}
		})
		for vault, open := range map[string]bool{locked: false, public: true} {
			if err := p.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: open}); err != nil {
				t.Fatalf("configure %s: %v", vault, err)
			}
		}
		return p
	}
	mustPush := func(t *testing.T, s *pushStream, when string) {
		t.Helper()
		if r := s.next(5 * time.Second); r.err != nil || !carriesMemory(r.push) {
			t.Fatalf("%s: got %+v; want a push carrying the memory", when, r)
		}
	}
	wantCode := func(t *testing.T, err error, code codes.Code, fragment string) {
		t.Helper()
		if status.Code(err) != code || !strings.Contains(status.Convert(err).Message(), fragment) {
			t.Errorf("the stream ended with %v; want %s mentioning %q", err, code, fragment)
		}
	}

	t.Run("revoked key gets nothing after the revocation", func(t *testing.T) {
		p := setup(t, 0)
		token, key, err := p.store.GenerateAPIKey(locked, "revokee", auth.ModeObserve, nil)
		if err != nil {
			t.Fatal(err)
		}
		s := p.subscribe(t, p.dial(t), locked, token)
		p.write(t, locked, "four")
		mustPush(t, s, "before revocation")

		if err := p.store.RevokeAPIKey(locked, key.ID); err != nil {
			t.Fatal(err)
		}
		p.write(t, locked, "nine")
		wantCode(t, s.endsWithout(t, "after revocation"), codes.Unauthenticated, "api key is no longer valid")
		p.awaitSubscriptions(t, locked, 0, "after the revoked stream ended")
	})

	t.Run("expired key gets nothing after it expires", func(t *testing.T) {
		p := setup(t, 0)
		expires := time.Now().Add(1500 * time.Millisecond)
		token, _, err := p.store.GenerateAPIKey(locked, "short-lived", auth.ModeFull, &expires)
		if err != nil {
			t.Fatal(err)
		}
		s := p.subscribe(t, p.dial(t), locked, token)
		p.write(t, locked, "four")
		mustPush(t, s, "before expiry")

		time.Sleep(time.Until(expires) + 50*time.Millisecond)
		p.write(t, locked, "nine")
		wantCode(t, s.endsWithout(t, "after expiry"), codes.Unauthenticated, "api key is no longer valid")
		p.awaitSubscriptions(t, locked, 0, "after the expired stream ended")
	})

	t.Run("a stream with no key ends when its vault stops being public", func(t *testing.T) {
		p := setup(t, 0)
		s := p.subscribe(t, p.dial(t), public, "")
		p.write(t, public, "four")
		mustPush(t, s, "while public")

		if err := p.store.SetVaultConfig(auth.VaultConfig{Name: public, Public: false}); err != nil {
			t.Fatal(err)
		}
		p.write(t, public, "nine")
		wantCode(t, s.endsWithout(t, "after the vault was locked"), codes.Unauthenticated, "requires an API key")
		p.awaitSubscriptions(t, public, 0, "after the keyless stream ended")
	})

	t.Run("a revoked key's idle stream ends without a push to prompt it", func(t *testing.T) {
		p := setup(t, 100*time.Millisecond)
		token, key, err := p.store.GenerateAPIKey(locked, "idle", auth.ModeObserve, nil)
		if err != nil {
			t.Fatal(err)
		}
		s := p.subscribe(t, p.dial(t), locked, token)
		p.awaitSubscriptions(t, locked, 1, "after subscribing")
		if err := p.store.RevokeAPIKey(locked, key.ID); err != nil {
			t.Fatal(err)
		}
		// Nothing is written: only the periodic check can notice.
		wantCode(t, s.endsWithout(t, "idle after revocation"), codes.Unauthenticated, "api key is no longer valid")
		p.awaitSubscriptions(t, locked, 0, "after the idle revoked stream ended")
	})

	t.Run("revoking one key leaves another key's stream alone", func(t *testing.T) {
		p := setup(t, 100*time.Millisecond)
		goneToken, goneKey, err := p.store.GenerateAPIKey(locked, "gone", auth.ModeObserve, nil)
		if err != nil {
			t.Fatal(err)
		}
		keptToken, _, err := p.store.GenerateAPIKey(locked, "kept", auth.ModeObserve, nil)
		if err != nil {
			t.Fatal(err)
		}
		client := p.dial(t)
		gone := p.subscribe(t, client, locked, goneToken)
		kept := p.subscribe(t, client, locked, keptToken)
		if err := p.store.RevokeAPIKey(locked, goneKey.ID); err != nil {
			t.Fatal(err)
		}
		wantCode(t, gone.endsWithout(t, "the revoked key's stream"), codes.Unauthenticated, "api key is no longer valid")

		// Several periodic checks have passed for the other stream by now.
		p.write(t, locked, "nine")
		mustPush(t, kept, "the other key's stream")
		p.awaitSubscriptions(t, locked, 1, "after one of two streams ended")
	})
}
