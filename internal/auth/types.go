package auth

import "time"

type AdminUser struct {
	Username  string    `json:"username"`
	PassHash  []byte    `json:"pass_hash"`
	CreatedAt time.Time `json:"created_at"`
}

type APIKey struct {
	ID          string     `json:"id"`
	Vault       string     `json:"vault"`
	Label       string     `json:"label"`
	Mode        string     `json:"mode"` // "full", "observe", or "write" (ingest-only)
	CreatedAt   time.Time  `json:"created_at"`
	StorageHash []byte     `json:"storage_hash"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"` // nil = never expires
}

type VaultConfig struct {
	Name       string            `json:"name"`
	Public     bool              `json:"public"`
	Plasticity *PlasticityConfig `json:"plasticity,omitempty"` // per-vault cognitive pipeline config
	// ExpiresAt is the workflow-vault expiry instant (#973). muninn_create_workflow_vault
	// stamps the SAME instant here and on the minted capability. The engine's reclamation
	// sweep and muninn_status's expiry notice read it — for wf-* vaults only. nil (the
	// default for every operator-created vault) is NEVER a delete reason: absent expiry
	// means no expiry, fail-safe not fail-open.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// API key mode constants.
const (
	ModeFull    = "full"    // full read + write access
	ModeObserve = "observe" // read-only; cognitive mutations suppressed at engine layer
	ModeWrite   = "write"   // ingest-only; read endpoints blocked at middleware layer
	ModeAppend  = "append"  // read + create-new. EVERY engine op that modifies or deletes an
	//                          existing engram/entity/lease/enrichment is refused (engine-level
	//                          via refuseAppend, so it holds on all transports; MCP also gates at
	//                          dispatch). The credential for automated capture (flush): it can add
	//                          memories and recall, never overwrite/evolve/forget/archive/re-trust —
	//                          nor supersede: a write that declares an inline `supersedes` retires
	//                          the memory it names and is refused like muninn_link (COG-38).
	//                          Accepted residuals — all reinforcement ("strengthens with use"), never
	//                          destruction: (a) remember on a content-hash duplicate reinforces an
	//                          existing engram's access metadata (TouchAccess, #682); (b) the read
	//                          path is NOT forced read-only for append (only for observe), so Read/
	//                          recall drive RecordFeedback (an inert per-vault weight update), TouchAccess,
	//                          and Hebbian/PAS on existing state. None overwrite content/confidence/
	//                          state/tags/lifecycle or delete. See engine.refuseAppend for the full note.
)

type contextKey string

const (
	ContextVault  contextKey = "auth_vault"
	ContextMode   contextKey = "auth_mode"
	ContextAPIKey contextKey = "auth_apikey"
	// ContextAdminSession is set (to true) only by the two middlewares that
	// have verified an admin session cookie. Read it with
	// AdminSessionFromContext.
	ContextAdminSession contextKey = "auth_admin_session"
)
