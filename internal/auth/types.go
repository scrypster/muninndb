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
	//                          memories and recall, never overwrite/evolve/forget/archive/re-trust.
	//                          Accepted residuals — all reinforcement ("strengthens with use"), never
	//                          destruction: (a) remember on a content-hash duplicate reinforces an
	//                          existing engram's access metadata (TouchAccess, #682); (b) the read
	//                          path is NOT forced read-only for append (only for observe), so Read/
	//                          recall drive RecordFeedback (adaptive scoring weights), TouchAccess,
	//                          and Hebbian/PAS on existing state. None overwrite content/confidence/
	//                          state/tags/lifecycle or delete. See engine.refuseAppend for the full note.
)

type contextKey string

const (
	ContextVault  contextKey = "auth_vault"
	ContextMode   contextKey = "auth_mode"
	ContextAPIKey contextKey = "auth_apikey"
)
