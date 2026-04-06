// internal/mcp/context.go
package mcp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
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

// maxTokenLen caps the bearer token length to prevent abuse of the constant-time
// compare (e.g., a 100 MB token would waste CPU). Real tokens are ≤ 100 chars.
const maxTokenLen = 4096

// authFromRequest extracts the Bearer token from the Authorization header and
// authenticates it in priority order:
//
//  1. mk_ vault API key (via apiKeyStore.ValidateAPIKey) — vault-pinned, mode-enforced.
//     Checked first so vault isolation applies even when no static token is configured.
//  2. Static mdb_ token (constant-time compare) — backward compatible, no vault pinning.
//  3. Open-server mode — if no static token configured and no mk_ key present, allow.
//
// apiKeyStore may be nil to disable mk_ key auth (legacy mode).
func authFromRequest(r *http.Request, requiredToken string, apiKeyStore apiKeyValidator) AuthContext {
	header := r.Header.Get("Authorization")
	token, found := strings.CutPrefix(header, "Bearer ")

	// 1. mk_ vault API key — always checked first, regardless of whether a static
	// token is configured. Presenting a scoped key is an explicit opt-in to vault
	// isolation; an invalid or revoked key must never fall through to open access.
	if found && token != "" && strings.HasPrefix(token, "mk_") && apiKeyStore != nil {
		if key, err := apiKeyStore.ValidateAPIKey(token); err == nil {
			return AuthContext{
				Token:      token,
				Authorized: true,
				Vault:      key.Vault,
				Mode:       key.Mode,
				IsAPIKey:   true,
			}
		}
		// Invalid mk_ key: fail-closed. Do not fall through to open-server mode.
		return AuthContext{Authorized: false}
	}

	// 2. Open-server mode — no static token required and no mk_ key presented.
	if requiredToken == "" {
		return AuthContext{Authorized: true}
	}

	// 3. Static token validation.
	if !found || token == "" {
		return AuthContext{Authorized: false}
	}
	// Reject absurdly long tokens before any crypto work.
	if len(token) > maxTokenLen {
		return AuthContext{Authorized: false}
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(requiredToken)) == 1 {
		return AuthContext{Token: token, Authorized: true}
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
//  3. No pinned vault + explicit arg → use arg (must be valid)
//  4. No pinned vault + no arg → use "default"
//
// Returns (vault, errMsg). errMsg is non-empty on error.
func resolveVault(pinnedVault string, args map[string]any) (vault string, errMsg string) {
	argVault, hasArg, invalidArg := vaultFromArgs(args)

	// Reject explicitly provided but invalid vault names rather than silently
	// falling back to "default" — fail-closed on malformed input.
	if invalidArg {
		return "", "invalid vault name: must be 1-64 lowercase alphanumeric, hyphen, or underscore characters"
	}

	if pinnedVault != "" {
		if !hasArg || argVault == "" || argVault == pinnedVault {
			return pinnedVault, ""
		}
		// Do not echo the pinned vault name back to the client — it may be
		// sensitive. The client already knows which vault they requested.
		return "", "vault mismatch: this key is scoped to a specific vault — " +
			"omit the vault arg or use a key scoped to the requested vault"
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
// write-mode keys:   blocked from non-mutating (read) tools.
//
// IMPORTANT: every tool in the dispatchToolCall handler map MUST appear in
// exactly one of isMutatingTool or isReadOnlyTool. The test
// TestToolClassification_CoversAllRegisteredHandlers enforces this invariant.
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
		"muninn_apply_enrichment",
		"muninn_entity_state",
		"muninn_entity_state_batch",
		"muninn_merge_entity",
		"muninn_replay_enrichment",
		"muninn_feedback":
		return true
	}
	return false
}

// isReadOnlyTool returns true for MCP tools that only read data.
// This is the explicit counterpart of isMutatingTool — together they must
// cover every registered tool name. Unknown tools are classified as neither,
// which causes mode enforcement to reject them (fail-closed).
func isReadOnlyTool(name string) bool {
	switch name {
	case "muninn_recall",
		"muninn_read",
		"muninn_status",
		"muninn_session",
		"muninn_contradictions",
		"muninn_traverse",
		"muninn_explain",
		"muninn_state",
		"muninn_list_deleted",
		"muninn_get_enrichment_candidates",
		"muninn_guide",
		"muninn_where_left_off",
		"muninn_recall_tree",
		"muninn_find_by_entity",
		"muninn_entity_clusters",
		"muninn_export_graph",
		"muninn_similar_entities",
		"muninn_entity_timeline",
		"muninn_provenance",
		"muninn_entity",
		"muninn_entities":
		return true
	}
	return false
}

// vaultFromArgs extracts the vault parameter from tool arguments.
// Returns (name, present, invalid):
//   - ("", false, false): vault key absent from args
//   - ("", false, true):  vault key present but value is invalid (bad type, empty, bad chars)
//   - (name, true, false): vault key present and valid
func vaultFromArgs(args map[string]any) (string, bool, bool) {
	v, ok := args["vault"]
	if !ok {
		return "", false, false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false, true
	}
	if !isValidVaultName(s) {
		return "", false, true
	}
	return s, true, false
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
