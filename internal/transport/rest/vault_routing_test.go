package rest

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// vaultTrackingEngine wraps MockEngine and records the vault passed to key engine calls.
type vaultTrackingEngine struct {
	MockEngine
	lastWriteVault    string
	lastActivateVault string
	lastListVault     string
	lastReadVault     string
	lastForgetVault   string
}

func (e *vaultTrackingEngine) Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error) {
	e.lastWriteVault = req.Vault
	return e.MockEngine.Write(ctx, req)
}

func (e *vaultTrackingEngine) Activate(ctx context.Context, req *ActivateRequest) (*ActivateResponse, error) {
	e.lastActivateVault = req.Vault
	return e.MockEngine.Activate(ctx, req)
}

func (e *vaultTrackingEngine) ListEngrams(ctx context.Context, req *ListEngramsRequest) (*ListEngramsResponse, error) {
	e.lastListVault = req.Vault
	return e.MockEngine.ListEngrams(ctx, req)
}

func (e *vaultTrackingEngine) Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error) {
	e.lastReadVault = req.Vault
	return e.MockEngine.Read(ctx, req)
}

func (e *vaultTrackingEngine) Forget(ctx context.Context, req *ForgetRequest) (*ForgetResponse, error) {
	e.lastForgetVault = req.Vault
	return e.MockEngine.Forget(ctx, req)
}

// newVaultTrackingServer creates a Server with a vaultTrackingEngine and a
// public "default" vault. The store is returned so tests can configure auth.
func newVaultTrackingServer(t *testing.T) (*Server, *vaultTrackingEngine, *auth.Store) {
	t.Helper()
	eng := &vaultTrackingEngine{}
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	srv := NewServer("localhost:0", eng, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	return srv, eng, store
}

