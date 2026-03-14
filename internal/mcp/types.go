package mcp

import (
	"encoding/json"
	"time"
)

// JSON-RPC 2.0 envelope types

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      json.RawMessage `json:"id,omitempty"`
	Params  *JSONRPCParams  `json:"params,omitempty"`
}

type JSONRPCParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// AuthContext is returned by authFromRequest. Struct (not bool) so scopes can be added later.
type AuthContext struct {
	Token      string
	Authorized bool
}

// ToolDefinition is one entry in the tools/list response.
type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// MCP domain types (used by EngineInterface and handlers)

type WriteResult struct {
	ID       string   `json:"id"`
	Concept  string   `json:"concept"`
	Hint     string   `json:"hint,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

type Memory struct {
	ID          string    `json:"id"`
	Concept     string    `json:"concept"`
	Content     string    `json:"content"` // recall: summary or 500-char preview; read: full content
	Summary     string    `json:"summary,omitempty"`
	Score       float64   `json:"score,omitempty"`
	VectorScore float64   `json:"vector_score,omitempty"`
	Confidence  float32   `json:"confidence"`
	Why         string    `json:"why,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	LastAccess  time.Time `json:"last_access"`
	AccessCount uint32    `json:"access_count,omitempty"`
	Relevance   float32   `json:"relevance,omitempty"`
	SourceType  string    `json:"source_type,omitempty"`

	// Populated only by muninn_read (omitted from recall responses).
	Entities            []ReadEntity    `json:"entities,omitempty"`
	EntityRelationships []ReadEntityRel `json:"entity_relationships,omitempty"`
}

// ReadEntity is a named entity linked to a specific engram.
type ReadEntity struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// ReadEntityRel is an entity-to-entity relationship sourced from a specific engram.
type ReadEntityRel struct {
	FromEntity string  `json:"from_entity"`
	ToEntity   string  `json:"to_entity"`
	RelType    string  `json:"rel_type"`
	Weight     float32 `json:"weight,omitempty"`
}

type ContradictionPair struct {
	IDa        string    `json:"id_a"`
	ConceptA   string    `json:"concept_a"`
	IDb        string    `json:"id_b"`
	ConceptB   string    `json:"concept_b"`
	DetectedAt time.Time `json:"detected_at"`
}

// VaultStatus is returned by muninn_status.
type VaultStatus struct {
	Vault         string `json:"vault"`
	TotalMemories int64  `json:"total_memories"`
	Health        string `json:"health"`

	// Enrichment capability
	EnrichmentMode string                `json:"enrichment_mode"` // "none", "inline", "plugin:<name>"
	Plugins        []PluginStatusSummary `json:"plugins,omitempty"`
}

// PluginStatusSummary is a brief health summary for one plugin.
type PluginStatusSummary struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Mode    string `json:"mode"` // "embed" or "enrich"
}

type SessionEntry struct {
	ID        string    `json:"id"`
	Concept   string    `json:"concept"`
	CreatedAt time.Time `json:"created_at"`
}

type SessionSummary struct {
	Writes      []SessionEntry `json:"writes"`
	Activations int            `json:"activations"`
	Since       time.Time      `json:"since"`
}

type ConsolidateResult struct {
	ID       string   `json:"id"`
	Archived []string `json:"archived"`
	Warnings []string `json:"warnings,omitempty"`
}

// Epic 18: New types for tools 12-17

// RestoreResult is returned by Restore on success.
type RestoreResult struct {
	ID      string `json:"id"`
	Concept string `json:"concept"`
	State   string `json:"state"`
}

// TraverseRequest defines parameters for a BFS graph traversal.
type TraverseRequest struct {
	StartID        string
	MaxHops        int
	MaxNodes       int
	RelTypes       []string
	FollowEntities bool
}

// TraverseResult is the output of a BFS graph traversal.
type TraverseResult struct {
	Nodes          []TraversalNode `json:"nodes"`
	Edges          []TraversalEdge `json:"edges"`
	TotalReachable int             `json:"total_reachable"`
	QueryMs        float64         `json:"query_ms"`
}

// TraversalNode is a single node returned in a traversal.
type TraversalNode struct {
	ID      string `json:"id"`
	Concept string `json:"concept"`
	HopDist int    `json:"hop_dist"`
	Summary string `json:"summary,omitempty"`
}

// TraversalEdge is an association edge returned in a traversal.
type TraversalEdge struct {
	FromID  string  `json:"from_id"`
	ToID    string  `json:"to_id"`
	RelType string  `json:"rel_type"`
	Weight  float32 `json:"weight"`
}

// ExplainRequest defines the context for a score explanation.
type ExplainRequest struct {
	EngramID string
	Query    []string
}

// ExplainComponents holds the per-component score breakdown.
type ExplainComponents struct {
	FullTextRelevance  float64 `json:"full_text_relevance"`
	SemanticSimilarity float64 `json:"semantic_similarity"`
	DecayFactor        float64 `json:"decay_factor"`
	HebbianBoost       float64 `json:"hebbian_boost"`
	AccessFrequency    float64 `json:"access_frequency"`
	Confidence         float64 `json:"confidence"`
}

// ExplainResult breaks down why an engram scored as it did for a given query.
type ExplainResult struct {
	EngramID    string            `json:"engram_id"`
	Concept     string            `json:"concept"`
	FinalScore  float64           `json:"final_score"`
	Components  ExplainComponents `json:"components"`
	FTSMatches  []string          `json:"fts_matches"`
	AssocPath   []string          `json:"assoc_path"`
	WouldReturn bool              `json:"would_return"`
	Threshold   float64           `json:"threshold"`
}

// DeletedEngram is a summary of a soft-deleted engram still within the recovery window.
type DeletedEngram struct {
	ID               string    `json:"id"`
	Concept          string    `json:"concept"`
	DeletedAt        time.Time `json:"deleted_at"`
	RecoverableUntil time.Time `json:"recoverable_until"`
	Tags             []string  `json:"tags,omitempty"`
}

// RetryEnrichResult reports which plugins were queued for re-processing.
type RetryEnrichResult struct {
	EngramID        string   `json:"engram_id"`
	PluginsQueued   []string `json:"plugins_queued"`
	AlreadyComplete []string `json:"already_complete"`
	Note            string   `json:"note,omitempty"`
}

// ── Tree types ────────────────────────────────────────────────────────────────

// TreeNodeInput is one node in a tree passed to muninn_remember_tree.
type TreeNodeInput struct {
	Concept  string          `json:"concept"`
	Content  string          `json:"content"`
	Type     string          `json:"type,omitempty"`
	Tags     []string        `json:"tags,omitempty"`
	Children []TreeNodeInput `json:"children,omitempty"`
}

// RememberTreeRequest is the input to RememberTree.
type RememberTreeRequest struct {
	Vault string        `json:"vault"`
	Root  TreeNodeInput `json:"root"`
}

// RememberTreeResult is returned by RememberTree.
type RememberTreeResult struct {
	RootID  string            `json:"root_id"`
	NodeMap map[string]string `json:"node_map"`
}

// TreeNode is a node in the recalled tree returned by muninn_recall_tree.
type TreeNode struct {
	ID           string     `json:"id"`
	Concept      string     `json:"concept"`
	State        string     `json:"state"`
	Ordinal      int32      `json:"ordinal"`
	LastAccessed string     `json:"last_accessed,omitempty"`
	Children     []TreeNode `json:"children"`
}

// RecallTreeResult wraps the root TreeNode.
type RecallTreeResult struct {
	Root *TreeNode `json:"root"`
}

// AddChildRequest is the input for a single child node in muninn_add_child.
type AddChildRequest struct {
	Concept string   `json:"concept"`
	Content string   `json:"content"`
	Type    string   `json:"type,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Ordinal *int32   `json:"ordinal,omitempty"` // nil = append at end
}

// AddChildResult is returned by AddChild.
type AddChildResult struct {
	ChildID string `json:"child_id"`
	Ordinal int32  `json:"ordinal"`
}

// WhereLeftOffEntry is one result from muninn_where_left_off.
type WhereLeftOffEntry struct {
	ID         string    `json:"id"`
	Concept    string    `json:"concept"`
	Summary    string    `json:"summary,omitempty"`
	LastAccess time.Time `json:"last_access"`
	State      string    `json:"state"`
}

// EntityClusterResult is one entity co-occurrence pair returned by muninn_entity_clusters.
type EntityClusterResult struct {
	EntityA string `json:"entity_a"`
	EntityB string `json:"entity_b"`
	Count   int    `json:"count"`
}

// --- Cognitive push notification param types ---
// These are pre-serialized to json.RawMessage at emission sites.

// ContradictionParams is the params payload for "notifications/muninn/contradiction".
type ContradictionParams struct {
	IDa     string `json:"id_a"`
	IDb     string `json:"id_b"`
	Concept string `json:"concept,omitempty"`
}

// ActivationParams is the params payload for "notifications/muninn/activation".
type ActivationParams struct {
	ID      string  `json:"id"`
	Concept string  `json:"concept"`
	Score   float64 `json:"score"`
	Vault   string  `json:"vault"`
}

// AssociationParams is the params payload for "notifications/muninn/association".
type AssociationParams struct {
	SourceID string  `json:"source_id"`
	TargetID string  `json:"target_id"`
	Weight   float32 `json:"weight"`
}

// ProvenanceEntry is a single audit log record returned by muninn_provenance.
type ProvenanceEntry struct {
	Timestamp string `json:"timestamp"` // RFC3339
	Source    string `json:"source"`    // "human", "llm", "inferred", etc.
	AgentID   string `json:"agent_id,omitempty"`
	Operation string `json:"operation"` // "write", "update", "read", etc.
	Note      string `json:"note,omitempty"`
}

// ProvenanceResult is the response from muninn_provenance.
type ProvenanceResult struct {
	ID      string            `json:"id"`
	Entries []ProvenanceEntry `json:"entries"`
}

// EntityEngramSummary is a brief projection of an engram mentioning an entity.
type EntityEngramSummary struct {
	ID        string `json:"id"`
	Concept   string `json:"concept"`
	CreatedAt string `json:"created_at"` // RFC3339
}

// EntityRelSummary is one relationship involving an entity.
type EntityRelSummary struct {
	FromEntity string  `json:"from_entity"`
	ToEntity   string  `json:"to_entity"`
	RelType    string  `json:"rel_type"`
	Weight     float32 `json:"weight"`
}

// EntityCoOccurrence is a co-occurring entity with its count.
type EntityCoOccurrence struct {
	EntityName string `json:"entity_name"`
	Count      int    `json:"count"`
}

// EntityAggregate is the full aggregate view returned by muninn_entity.
type EntityAggregate struct {
	Name          string                `json:"name"`
	Type          string                `json:"type"`
	Confidence    float32               `json:"confidence"`
	State         string                `json:"state"`
	MentionCount  int32                 `json:"mention_count"`
	FirstSeen     string                `json:"first_seen,omitempty"`  // RFC3339
	UpdatedAt     string                `json:"updated_at,omitempty"`  // RFC3339
	MergedInto    string                `json:"merged_into,omitempty"`
	Engrams       []EntityEngramSummary `json:"engrams"`
	Relationships []EntityRelSummary    `json:"relationships"`
	CoOccurring   []EntityCoOccurrence  `json:"co_occurring"`
}

// EntitySummary is a lightweight entity record for muninn_entities list view.
type EntitySummary struct {
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	Confidence   float32 `json:"confidence"`
	State        string  `json:"state"`
	MentionCount int32   `json:"mention_count"`
	FirstSeen    string  `json:"first_seen,omitempty"` // RFC3339
}
