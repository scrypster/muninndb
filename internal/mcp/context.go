// internal/mcp/context.go
package mcp

import (
	"context"
	"net/http"

	"github.com/scrypster/muninndb/internal/auth"
)

const mcpSessionHeader = "Mcp-Session-Id"

// mcpInstructions is returned in the initialize response to tell MCP clients
// how to use MuninnDB. Kept concise — call muninn_guide for the full guide.
const mcpInstructions = `MuninnDB is a long-term memory server for AI agents. ` +
	`At session start recall recent context — muninn_where_left_off in a single-user vault; ` +
	`in a shared vault it is vault-global, so use muninn_recall scoped to your per-user tag instead ` +
	`(muninn_guide states whether this vault is shared, under Vault Configuration). ` +
	`Store with muninn_remember (include type, summary, entities). ` +
	`You are this memory's curator, not a static store: the moment you write is the moment you know something, so it is the moment to reconcile. ` +
	`Before adding a fact, recall what's related — if your new knowledge corrects, sharpens, or supersedes an existing memory, muninn_evolve that one instead of adding a rival copy (evolve supersedes and retires the old version; a second muninn_remember leaves a stale duplicate competing in recall). ` +
	`Reserve muninn_remember for genuinely new facts. Keep memories atomic — one concept each. ` +
	`Call muninn_guide for the full reference.`

// apiKeyValidator is the subset of auth.Store used by MCP for vault key auth.
// Using an interface keeps the mcp package testable without a live Pebble store.
type apiKeyValidator interface {
	ValidateAPIKey(token string) (auth.APIKey, error)
}

// capabilityValidator is the subset of auth.Store used by MCP for cap_ token
// auth (RFC #597). Kept as an interface so the mcp package remains testable
// without a live Pebble store; the real implementation is *auth.Store.
type capabilityValidator interface {
	ValidateCapability(token string) (auth.Capability, error)
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
//  1. mk_ vault API key (via apiKeyStore.ValidateAPIKey) — vault-pinned, mode-enforced.
//     Checked first so vault isolation applies even when no static token is configured.
//  2. Static mdb_ token (constant-time compare) — backward compatible, no vault pinning.
//  3. Open-server mode — if no static token configured and no mk_ key present, allow.
//
// apiKeyStore may be nil to disable mk_ key auth (legacy mode).
// capStore may be nil to disable cap_ capability auth (pre-RFC #597 mode);
// when non-nil, an invalid or expired cap_ token fails closed (never falls
// through to open-server), mirroring the mk_ posture.
func authFromRequest(r *http.Request, requiredToken string, apiKeyStore apiKeyValidator, capStore capabilityValidator) AuthContext {
	token, found := auth.ParseBearerToken(r.Header.Get("Authorization"))

	// 1. mk_ vault API key — always checked first, regardless of whether a static
	// token is configured. Presenting a scoped key is an explicit opt-in to vault
	// isolation; an invalid or revoked key must never fall through to open access.
	if found && len(token) > 3 && token[:3] == "mk_" && apiKeyStore != nil {
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

	// 1b. cap_ capability token — vault-pinned, mode-enforced, TTL'd (RFC #597).
	// Checked before the open-server fallthrough: an invalid or expired cap_
	// token must NOT drop into open-server mode. When capStore is nil, cap_
	// auth is disabled and the branch is skipped (backward-compatible).
	if found && len(token) > 4 && token[:4] == "cap_" && capStore != nil {
		if cap, err := capStore.ValidateCapability(token); err == nil {
			return AuthContext{
				Token:        token,
				Authorized:   true,
				Vault:        cap.Vault,
				Mode:         cap.Mode,
				IsCapability: true,
			}
		}
		// Invalid cap_ token: fail-closed.
		return AuthContext{Authorized: false}
	}

	// 2. Open-server mode — no static token required and no mk_ key presented.
	if requiredToken == "" {
		return AuthContext{Authorized: true}
	}

	// 3. Static token validation — constant-time compare with length cap.
	if !found {
		return AuthContext{Authorized: false}
	}
	if auth.ValidateStaticToken(token, requiredToken) {
		return AuthContext{Token: token, Authorized: true}
	}
	return AuthContext{Authorized: false}
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
	return defaultVaultName, ""
}

// defaultVaultName is the vault a request with no pinned vault and no explicit
// `vault` argument resolves to.
const defaultVaultName = "default"

// joinHints appends one hint to another with a single separating space,
// tolerating an empty base. Response hints accumulate from several
// independent sources and none of them may clobber another.
func joinHints(base, extra string) string {
	switch {
	case extra == "":
		return base
	case base == "":
		return extra
	default:
		return base + " " + extra
	}
}

// The three tables below are the MCP tool classification. They were `switch`
// bodies until #731; a switch cannot be enumerated at run time, so nothing
// could assert the reverse direction — that every name a classifier claims is
// a name some handler actually registers. A dead or misspelled entry was
// therefore invisible to the whole suite. As maps they are enumerable, and
// tool_classification_test.go walks them in both directions against
// toolHandlers(). Behaviour is unchanged: a missing key yields the zero value
// false, exactly as the switch fell through to `return false`.
//
// Every tool in the dispatchToolCall handler map MUST appear in exactly one of
// mutatingTools or readOnlyTools; additiveTools is an overlay on mutatingTools,
// not a third bucket.

// mutatingTools lists MCP tools that write, modify, or delete data. Used to
// enforce mode restrictions when authenticating via an mk_ vault API key.
//
// observe-mode keys: blocked from mutating tools.
// write-mode keys:   blocked from non-mutating (read) tools.
var mutatingTools = map[string]bool{
	"muninn_remember":       true,
	"muninn_remember_batch": true,
	"muninn_remember_tree":  true,
	"muninn_add_child":      true,
	"muninn_forget":         true,
	"muninn_link":           true,
	"muninn_evolve":         true,
	// muninn_state transitions an existing engram's lifecycle state, up to and
	// including "archived". handleState calls s.engine.UpdateState, which
	// mcpEngineAdapter forwards to Engine.UpdateLifecycleState — the rename is
	// why grepping internal/mcp/ for the engine method turns up only that
	// one-line forwarder in engine_adapter.go and never handleState, and the
	// misclassification survived, letting an observe-mode credential write
	// (#731).
	"muninn_state":                 true,
	"muninn_consolidate":           true,
	"muninn_decide":                true,
	"muninn_restore":               true,
	"muninn_retry_enrich":          true,
	"muninn_apply_enrichment":      true,
	"muninn_entity_state":          true,
	"muninn_entity_state_batch":    true,
	"muninn_merge_entity":          true,
	"muninn_replay_enrichment":     true,
	"muninn_feedback":              true,
	"muninn_trust":                 true,
	"muninn_compare_and_set":       true,
	"muninn_claim":                 true,
	"muninn_release":               true,
	"muninn_create_workflow_vault": true,
	"muninn_intend":                true,
}

// readOnlyTools lists MCP tools that only read data. This is the explicit
// counterpart of mutatingTools — together they must cover every registered
// tool name. Unknown tools are in neither, which causes mode enforcement to
// reject them (fail-closed).
var readOnlyTools = map[string]bool{
	"muninn_recall":                    true,
	"muninn_read":                      true,
	"muninn_status":                    true,
	"muninn_session":                   true,
	"muninn_contradictions":            true,
	"muninn_traverse":                  true,
	"muninn_explain":                   true,
	"muninn_list_deleted":              true,
	"muninn_get_enrichment_candidates": true,
	"muninn_guide":                     true,
	"muninn_where_left_off":            true,
	"muninn_recall_tree":               true,
	"muninn_find_by_entity":            true,
	"muninn_entity_clusters":           true,
	"muninn_export_graph":              true,
	"muninn_similar_entities":          true,
	"muninn_entity_timeline":           true,
	"muninn_provenance":                true,
	"muninn_entity":                    true,
	"muninn_entities":                  true,
}

// additiveTools is the subset of mutating tools that only CREATE new engrams
// and never modify or delete existing ones. Append-mode credentials
// (auth.ModeAppend — the flush write credential) may call these plus read
// tools, but NOT the destructive mutating tools (evolve/forget/trust/merge/…).
// Keep this a strict subset of mutatingTools.
var additiveTools = map[string]bool{
	"muninn_remember":       true,
	"muninn_remember_batch": true,
	"muninn_remember_tree":  true,
	"muninn_add_child":      true,
}

// isMutatingTool reports whether name writes, modifies, or deletes data.
func isMutatingTool(name string) bool { return mutatingTools[name] }

// isReadOnlyTool reports whether name only reads data.
func isReadOnlyTool(name string) bool { return readOnlyTools[name] }

// isAdditiveTool reports whether name is a create-only mutating tool.
func isAdditiveTool(name string) bool { return additiveTools[name] }

// resolveReadOnly computes the effective read-only decision (S3) for
// muninn_recall, muninn_read, and muninn_where_left_off:
//
//	effective = credentialObserve(a.Mode via S0, carried on ctx by
//	            auth.ContextMode) || explicit request "read_only" arg
//
// An observe-mode credential combined with an EXPLICIT read_only=false is
// rejected (errMsg non-empty) rather than silently downgraded to read-only —
// the request cannot escalate past what the credential allows, and failing
// loudly here surfaces the caller's mistaken assumption instead of masking
// it. Omitting "read_only" entirely is NOT treated as explicit false: it
// simply defers to the credential (backward compatible with callers that
// never set the flag).
func resolveReadOnly(ctx context.Context, args map[string]any) (effective bool, errMsg string) {
	credObserve := auth.ObserveFromContext(ctx)
	reqReadOnly, hasReadOnly := args["read_only"].(bool)
	if credObserve && hasReadOnly && !reqReadOnly {
		return false, "forbidden: observe-mode credential cannot request read_only=false"
	}
	return credObserve || reqReadOnly, ""
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
	if !auth.IsValidVaultName(s) {
		return "", false, true
	}
	return s, true, false
}
