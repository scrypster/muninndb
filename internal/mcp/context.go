// internal/mcp/context.go
package mcp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/scrypster/muninndb/internal/auth"
)

const mcpSessionHeader = "Mcp-Session-Id"

// apiKeyValidator is the subset of auth.Store used by MCP for vault key auth.
// Using an interface keeps the mcp package testable without a live Pebble store.
type apiKeyValidator interface {
	ValidateAPIKey(token string) (auth.APIKey, error)
}

// mcpAuthContextKey is the unexported key used to store AuthContext in request context.
type mcpAuthContextKey struct{}

// contextWithAuth returns a new context carrying the given AuthContext.
func contextWithAuth(ctx context.Context, a AuthContext) context.Context {
	return context.WithValue(ctx, mcpAuthContextKey{}, a)
}

// authFromContext retrieves the AuthContext stored by contextWithAuth.
// Returns a zero-value AuthContext if none is present.
func authFromContext(ctx context.Context) AuthContext {
	a, _ := ctx.Value(mcpAuthContextKey{}).(AuthContext)
	return a
}

// authFromRequest extracts the Bearer token from the Authorization header and
// authenticates it in priority order:
//
//  1. Static mdb_ token (constant-time compare) — backward compatible, no vault pinning.
//  2. mk_ vault API key (via apiKeyStore.ValidateAPIKey) — vault-pinned, mode-enforced.
//
// Returns AuthContext{Authorized: true} if the server has no token configured.
// apiKeyStore may be nil to disable mk_ key auth (legacy mode).
func authFromRequest(r *http.Request, requiredToken string, apiKeyStore apiKeyValidator) AuthContext {
	if requiredToken == "" {
		return AuthContext{Authorized: true}
	}
	header := r.Header.Get("Authorization")
	token, found := strings.CutPrefix(header, "Bearer ")
	if !found || token == "" {
		return AuthContext{Authorized: false}
	}
	// 1. Static token — always tried first (constant-time to prevent timing attacks).
	if subtle.ConstantTimeCompare([]byte(token), []byte(requiredToken)) == 1 {
		return AuthContext{Token: token, Authorized: true}
	}
	// 2. Vault API key — only attempted for mk_ prefixed tokens when store is available.
	if apiKeyStore != nil && strings.HasPrefix(token, "mk_") {
		if key, err := apiKeyStore.ValidateAPIKey(token); err == nil {
			return AuthContext{
				Token:      token,
				Authorized: true,
				Vault:      key.Vault,
				Mode:       key.Mode,
				IsAPIKey:   true,
			}
		}
	}
	return AuthContext{Authorized: false}
}

// sessionFromRequest looks up a session by the Mcp-Session-Id header.
// Returns (nil, "") if no header present.
// Returns (nil, sessionID) if header present but session not found or expired.
func sessionFromRequest(r *http.Request, store sessionStore) (sess *mcpSession, sessionID string) {
	sessionID = r.Header.Get(mcpSessionHeader)
	if sessionID == "" {
		return nil, ""
	}
	sess, ok := store.Get(sessionID)
	if !ok {
		return nil, sessionID
	}
	return sess, sessionID
}

// validateSessionToken checks that the bearer token matches the session's token hash.
// Returns an error string if invalid, "" if valid.
// Precondition: sess must not be nil.
func validateSessionToken(sess *mcpSession, token string) string {
	h := sha256.Sum256([]byte(token))
	if h != sess.tokenHash {
		return "token does not match session"
	}
	return ""
}

// resolveVault determines the effective vault for a tool call.
//
// Resolution order:
//  1. pinnedVault non-empty (from mk_ key auth) + arg absent or matching → use pinnedVault
//  2. pinnedVault non-empty + arg differs → vault mismatch error
//  3. No pinned vault + explicit arg → use arg
//  4. No pinned vault + no arg → use "default"
//
// Returns (vault, errMsg). errMsg is non-empty on error.
func resolveVault(pinnedVault string, args map[string]any) (vault string, errMsg string) {
	argVault, hasArg := vaultFromArgs(args)

	if pinnedVault != "" {
		if !hasArg || argVault == "" || argVault == pinnedVault {
			return pinnedVault, ""
		}
		return "", fmt.Sprintf(
			"vault mismatch: key is scoped to %q but tool call specified %q — "+
				"omit the vault arg or use a key scoped to that vault",
			pinnedVault, argVault,
		)
	}

	if hasArg && argVault != "" {
		return argVault, ""
	}
	return "default", ""
}

// isMutatingTool returns true for MCP tools that write, modify, or delete data.
// Used to enforce mode restrictions when authenticating via an mk_ vault API key.
//
// observe-mode keys: blocked from mutating tools.
// write-mode keys:   blocked from non-mutating tools.
func isMutatingTool(name string) bool {
	switch name {
	case "muninn_remember",
		"muninn_remember_batch",
		"muninn_remember_tree",
		"muninn_add_child",
		"muninn_forget",
		"muninn_link",
		"muninn_evolve",
		"muninn_consolidate",
		"muninn_decide",
		"muninn_restore",
		"muninn_retry_enrich",
		"muninn_entity_state",
		"muninn_entity_state_batch",
		"muninn_merge_entity",
		"muninn_replay_enrichment",
		"muninn_feedback":
		return true
	}
	return false
}

// vaultFromArgs extracts the vault parameter from tool arguments.
// Returns ("", false) if vault is missing or empty.
// Validates that the vault name contains only lowercase letters, digits, hyphens, and underscores (max 64 chars).
func vaultFromArgs(args map[string]any) (string, bool) {
	v, ok := args["vault"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	if !isValidVaultName(s) {
		return "", false
	}
	return s, true
}

// isValidVaultName returns true if name is a valid vault name: 1–64 characters,
// containing only lowercase letters, digits, hyphens, and underscores.
func isValidVaultName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
