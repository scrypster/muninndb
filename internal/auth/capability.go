package auth

import "time"

// Capability is a scoped, revocable, TTL'd credential distinct from an APIKey.
// It authenticates via the "cap_" token prefix and resolves to {Vault, Mode}.
// Capabilities are minted by muninn_create_workflow_vault so worker agents can
// access a workflow vault without holding an mk_ key (which would let it mint
// more vaults). See RFC #597.
type Capability struct {
	ID          string     `json:"id"`
	Vault       string     `json:"vault"`
	Mode        string     `json:"mode"` // ModeFull | ModeObserve | ModeWrite
	Label       string     `json:"label"`
	Origin      string     `json:"origin"` // provenance, e.g. "workflow_vault"
	CreatedAt   time.Time  `json:"created_at"`
	StorageHash []byte     `json:"storage_hash"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}
