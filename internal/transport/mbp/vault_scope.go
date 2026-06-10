package mbp

import (
	"context"
	"fmt"
	"strings"

	"github.com/scrypster/muninndb/internal/auth"
)

// vaultScope is the per-connection authorization established at HELLO time.
// A non-nil key means the connection authenticated with an API key and is
// pinned to that key's vault; a nil key means an unauthenticated ("none")
// session that may only touch public vaults.
type vaultScope struct {
	key *auth.APIKey
}

type vaultScopeKeyType struct{}

var vaultScopeKey = vaultScopeKeyType{}

// withVaultScope returns a context carrying the connection's vault scope so
// per-frame handlers can enforce it without threading extra parameters.
func withVaultScope(ctx context.Context, sc *vaultScope) context.Context {
	return context.WithValue(ctx, vaultScopeKey, sc)
}

// scopeVault validates the vault named in a single request frame against the
// connection's vault scope and returns the resolved vault to hand to the
// engine. It mirrors auth.VaultAuthMiddleware so MBP enforces the same
// fail-closed model as REST and MCP:
//
//   - Keyed session: the key's vault is authoritative. An empty request vault
//     resolves to it; any other value is rejected (a key scoped to vault A can
//     never operate on vault B, even a public one).
//   - Unauthenticated session: the request vault (default "default") must be a
//     vault configured Public. Locked or unconfigured vaults are rejected.
//
// It fails closed if the context carries no scope or the server has no auth
// store, so a misconfigured server denies access rather than exposing every
// vault.
func (s *Server) scopeVault(ctx context.Context, reqVault string) (string, error) {
	reqVault = strings.TrimSpace(reqVault)
	sc, _ := ctx.Value(vaultScopeKey).(*vaultScope)
	if sc == nil || s.authStore == nil {
		return "", fmt.Errorf("vault access denied: connection is not authorized")
	}

	if sc.key != nil {
		if reqVault != "" && reqVault != sc.key.Vault {
			return "", fmt.Errorf("api key is not authorized for vault %q", reqVault)
		}
		return sc.key.Vault, nil
	}

	vault := reqVault
	if vault == "" {
		vault = "default"
	}
	cfg, err := s.authStore.GetVaultConfig(vault)
	if err != nil || !cfg.Public {
		return "", fmt.Errorf("vault %q requires an API key", vault)
	}
	return vault, nil
}
