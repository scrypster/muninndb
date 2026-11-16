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

	// openedOn is the vault the session named at HELLO. A session with no key
	// exists because that vault is public, and ends when it stops being so.
	openedOn string

	// end stops the connection reading. The frames it is already handling
	// finish and are answered; then it is closed. Set by handleConnection.
	end func()
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

// sessionEnded reports why the connection's session is over, or "" while it is
// not. It is asked for every frame, before anything else is decided about the
// frame (dispatchFrame), so what authorised the session at HELLO is checked
// again each time it is relied on, not once (SEC-23):
//
//   - a keyed session ends when its key would no longer validate — revoked,
//     or expired;
//   - a session with no key ends when the vault it was opened on is no longer
//     public.
//
// Both are read from the auth store on each frame. That is the cost of being
// right on every node: see auth.Store.APIKeyStillValid.
func (s *Server) sessionEnded(ctx context.Context) string {
	sc, _ := ctx.Value(vaultScopeKey).(*vaultScope)
	if sc == nil || s.authStore == nil {
		// No session to have ended. Such a frame gets nowhere anyway: scopeVault
		// refuses it for the same reason, and admit refuses a connection with
		// no mode.
		return ""
	}
	if sc.key != nil {
		if err := s.authStore.APIKeyStillValid(sc.key); err != nil {
			return msgKeyNoLongerValid
		}
		return ""
	}
	cfg, err := s.authStore.GetVaultConfig(sc.openedOn)
	if err != nil || !cfg.Public {
		return fmt.Sprintf("vault %q requires an API key", sc.openedOn)
	}
	return ""
}

// endSession stops the connection reading, if ctx belongs to a connection.
func endSession(ctx context.Context) {
	if sc, _ := ctx.Value(vaultScopeKey).(*vaultScope); sc != nil && sc.end != nil {
		sc.end()
	}
}
