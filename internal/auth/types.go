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
	Mode        string     `json:"mode"`      // "full", "observe", or "write" (ingest-only)
	CreatedAt   time.Time  `json:"created_at"`
	StorageHash []byte     `json:"storage_hash"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"` // nil = never expires
}

type VaultConfig struct {
	Name       string           `json:"name"`
	Public     bool             `json:"public"`
	Plasticity *PlasticityConfig `json:"plasticity,omitempty"` // per-vault cognitive pipeline config
}

// API key mode constants.
const (
	ModeFull    = "full"    // full read + write access
	ModeObserve = "observe" // read-only; cognitive mutations suppressed at engine layer
	ModeWrite   = "write"   // ingest-only; read endpoints blocked at middleware layer
)

type contextKey string

const (
	ContextVault  contextKey = "auth_vault"
	ContextMode   contextKey = "auth_mode"
	ContextAPIKey contextKey = "auth_apikey"
)
