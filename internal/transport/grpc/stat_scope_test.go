package grpc_test

import (
	"context"
	"strconv"
	"testing"

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
	"google.golang.org/grpc/metadata"
)

// scopeServer is a gRPC server over a real engine and auth store holding three
// vaults: the locked one the keys are pinned to, a public one, and a locked one
// nobody in these tests is entitled to. Fixture: a fictional harbour pilot
// station.
type scopeServer struct {
	srv   *transportgrpc.Server
	store *auth.Store

	pinned, public, other string
}

func newScopeServer(t *testing.T) *scopeServer {
	t.Helper()
	opts := storage.DefaultOptions()
	opts.FS = vfs.NewMem()
	db, err := storage.OpenPebble("grpc-scope", opts)
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

	s := &scopeServer{store: authStore, pinned: "pilot-station", public: "harbour-notices", other: "customs-ledger"}
	full := context.WithValue(context.Background(), auth.ContextMode, auth.ModeFull)
	for vault, n := range map[string]int{s.pinned: 2, s.public: 3, s.other: 5} {
		if err := authStore.SetVaultConfig(auth.VaultConfig{Name: vault, Public: vault == s.public}); err != nil {
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
	s.srv = transportgrpc.NewServer(":0", transportgrpc.NewEngineAdapter(eng), authStore, nil)
	return s
}

func (s *scopeServer) key(t *testing.T, mode string) string {
	t.Helper()
	token, _, err := s.store.GenerateAPIKey(s.pinned, "scope-"+mode, mode, nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey(%s): %v", mode, err)
	}
	return token
}

// unary sends one request through the real auth interceptor. An empty token is
// a call with no credential.
func (s *scopeServer) unary(token string, req any, invoke func(context.Context) (any, error)) (any, error) {
	ctx := context.Background()
	if token != "" {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-api-key", token))
	}
	return s.srv.TestableAuthUnaryInterceptor(ctx, req, nil, func(ctx context.Context, _ any) (any, error) {
		return invoke(ctx)
	})
}

// TestStatScope pins what the Stat RPC tells a caller entitled to one vault: a
// key pinned to it, or no credential on a public vault. The answer covers that
// vault only. The server's vault count and on-disk size, which give the other
// vaults away, are not in it.
//
// Before the scoping, both got the server's vault count and on-disk size.
func TestStatScope(t *testing.T) {
	s := newScopeServer(t)
	callers := []struct {
		name, token, vault string
	}{
		{"no key, public vault", "", s.public},
		{"observe key", s.key(t, auth.ModeObserve), s.pinned},
		{"full key", s.key(t, auth.ModeFull), s.pinned},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			req := &pb.StatRequest{Vault: c.vault}
			out, err := s.unary(c.token, req, func(ctx context.Context) (any, error) { return s.srv.Stat(ctx, req) })
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			resp := out.(*pb.StatResponse)
			t.Logf("answer: engram_count=%d vault_count=%d storage_bytes=%d", resp.EngramCount, resp.VaultCount, resp.StorageBytes)
			if resp.VaultCount != 1 {
				t.Errorf("vault_count = %d; want 1, the one vault in scope (the server has 3)", resp.VaultCount)
			}
			if resp.StorageBytes != 0 {
				t.Errorf("storage_bytes = %d; want 0: the server's on-disk size covers every vault", resp.StorageBytes)
			}
			if resp.EngramCount <= 0 {
				t.Errorf("engram_count = %d; want the vault's own count", resp.EngramCount)
			}
		})
	}
}
