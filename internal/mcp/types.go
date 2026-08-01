package mcp

import (
	"encoding/json"
	"time"

	"github.com/scrypster/muninndb/internal/engine"
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
	// Populated when authenticated via an mk_ vault API key (not the static mdb_ token).
	Vault    string // vault the key is scoped to; empty for static-token auth
	Mode     string // "full", "observe", or "write"; empty for static-token auth
	IsAPIKey bool   // true when authed via an mk_ vault API key
	// IsCapability is true when authed via a cap_ capability token (RFC #597).
	// Capabilities are distinct from mk_ API keys: they cannot mint further
	// vaults, so the recursion guard in dispatchToolCall (Task 4) gates
	// muninn_create_workflow_vault on IsAPIKey, not merely Authorized.
	IsCapability bool // true when authed via a cap_ capability token
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
	// Notices are prospective-memory deliveries (THE PUSH): armed intentions
	// whose cue entity is focal in this write's inline entities. Omitted when
	// empty (zero token cost) and inert unless MUNINN_PROSPECTIVE=1.
	Notices []engine.Notice `json:"notices,omitempty"`
}

type Memory struct {
	ID          string  `json:"id"`
	Concept     string  `json:"concept"`
	Content     string  `json:"content"` // recall: real content (truncated); read: full content
	Summary     string  `json:"summary,omitempty"`
	Score       float64 `json:"score,omitempty"`
	VectorScore float64 `json:"vector_score,omitempty"`
	// VectorScoreRaw is the uncalibrated cosine similarity behind VectorScore
	// (COG-26's honesty backstop — see activation.ScoreComponents.
	// SemanticSimilarityRaw). Lets an operator see the raw signal for a match
	// that a low VectorScore made look weak or that abstained entirely.
	VectorScoreRaw float64   `json:"vector_score_raw,omitempty"`
	EntityBoost    float64   `json:"entity_boost,omitempty"`
	Confidence     float32   `json:"confidence"`
	Why            string    `json:"why,omitempty"`
	Tags           []string  `json:"tags,omitempty"`
	State          string    `json:"state,omitempty"`
	Type           string    `json:"type"`                 // canonical MemoryType label ("fact", "decision", ...); always present
	TypeLabel      string    `json:"type_label,omitempty"` // writer-supplied free-form label, e.g. "architectural_decision"
	CreatedAt      time.Time `json:"created_at"`
	LastAccess     time.Time `json:"last_access"`
	AccessCount    uint32    `json:"access_count,omitempty"`
	Relevance      float32   `json:"relevance,omitempty"`
	SourceType     string    `json:"source_type,omitempty"`
	Trust          string    `json:"trust,omitempty"` // "verified", "inferred", "external", "untrusted"

	// Importance is the use-time EffectiveImportance in [0,1]; always present.
	// ImportanceSource says where it came from: "explicit" (caller-asserted at
	// write/evolve) or "derived" (memory-type table + trust bump — never
	// stored, computed at read time).
	Importance       float64 `json:"importance"`
	ImportanceSource string  `json:"importance_source"` // "explicit" | "derived"

	// Valid-time (application-time) axis, half-open [valid_from, valid_until).
	// Distinct from created_at (transaction time). muninn_read always sets
	// valid_from and is_current; recall sets valid_from only when it diverges
	// from created_at. valid_until appears only when the window is closed;
	// expired marks a fact whose window closed at or before now (only
	// reachable in recall results under include_invalid=true).
	ValidFrom  *time.Time `json:"valid_from,omitempty"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
	IsCurrent  *bool      `json:"is_current,omitempty"`
	Expired    bool       `json:"expired,omitempty"`

	// Populated only by muninn_read (omitted from recall responses).
	Entities            []ReadEntity    `json:"entities,omitempty"`
	EntityRelationships []ReadEntityRel `json:"entity_relationships,omitempty"`

	// Populated by muninn_recall: supersession fields (superseded_by / current_version)
	// are always set when the memory is superseded; the rest of the fields
	// (stale, conflicts_with, last_verified) only when annotate=true.
	Annotations *MemoryAnnotations `json:"annotations,omitempty"`
}

// MemoryAnnotations contains contextual metadata about a recalled memory.
// SupersededBy / CurrentVersion are populated whenever the memory is superseded
// (always-on, from supersedes-aware recall); the other fields are populated only
// when muninn_recall is called with annotate=true.
type MemoryAnnotations struct {
	Stale         bool     `json:"stale"`
	StaleDays     float64  `json:"stale_days"`
	ConflictsWith []string `json:"conflicts_with,omitempty"`
	// SupersededBy is the immediate superseder's ULID; CurrentVersion is the chain
	// head — the fact to consult now. Both present when this memory is stale.
	SupersededBy   string `json:"superseded_by,omitempty"`
	CurrentVersion string `json:"current_version,omitempty"`
	// PossiblySupersededBy / VersionCluster / NewestOfCluster / ClusterSize are the
	// ADVISORY heuristic-currency signal (COG-25) — inferred from a same-version
	// cluster, NOT asserted. PossiblySupersededBy names a newer, highly-similar
	// fact about the same subject: a mechanical hint, verify before treating this
	// memory as false — it is still returned at full score. Distinct from the
	// authoritative SupersededBy above.
	//
	// Scope: these are computed over the CO-RETRIEVED results only. "newest_of_cluster"
	// means newest among the returned cluster members — a newer version that scored
	// below the retrieval cut is not considered — and possibly_superseded_by may name
	// an engram not present in this response (muninn_read it to inspect). Same
	// returned-set boundary the authoritative superseded_by already has.
	PossiblySupersededBy string `json:"possibly_superseded_by,omitempty"`
	VersionCluster       string `json:"version_cluster,omitempty"`
	NewestOfCluster      bool   `json:"newest_of_cluster,omitempty"`
	ClusterSize          int    `json:"cluster_size,omitempty"`
	// SubstitutedFor / SubstitutionBasis / ChainTruncated / HeadNotIndexedYet
	// are COG-28 version-head substitution (#763) — ASSERTED, from a declared
	// RelSupersedes chain. Siblings of SupersededBy/CurrentVersion above, and
	// explicitly NOT part of the advisory PossiblySupersededBy block.
	//
	// SubstitutedFor names the older, superseded memory your query's wording
	// actually matched: this memory replaced it, so recall returned or boosted
	// this one instead. On a row whose own wording did NOT match, the reported
	// score AND components are the PREDECESSOR's measurements; on a row that
	// matched on its own but was raised to the predecessor's stronger score,
	// only the score is the predecessor's — the components remain this
	// memory's own. SubstitutionBasis repeats the predecessor's load-bearing
	// measurements in both cases so the score's origin is unmissable.
	// ChainTruncated: the version chain was longer than the walk limit, so this
	// may not be the very latest version. HeadNotIndexedYet: this memory has no
	// embedding yet (indexing pending) — "not indexed", not "not relevant".
	SubstitutedFor    string             `json:"substituted_for,omitempty"`
	SubstitutionBasis *SubstitutionBasis `json:"substitution_basis,omitempty"`
	ChainTruncated    bool               `json:"chain_truncated,omitempty"`
	HeadNotIndexedYet bool               `json:"head_not_indexed_yet,omitempty"`
	LastVerified      string             `json:"last_verified,omitempty"` // RFC3339
}

// SubstitutionBasis is the superseded predecessor's measured evidence against
// the query — what admitted a COG-28 substituted row. AbsoluteScore is the
// exact quantity compared against the recall threshold.
type SubstitutionBasis struct {
	AbsoluteScore      float64 `json:"absolute_score"`
	ContentMatch       float64 `json:"content_match"`
	SemanticSimilarity float64 `json:"semantic_similarity"`
	FullTextRelevance  float64 `json:"full_text_relevance"`
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

// ContradictionPair is one contradicting pair as muninn_contradictions renders
// it.
//
// DetectedAt and DeclaredAt are pointers so an unknown time is ABSENT from the
// JSON rather than serialised as "0001-01-01T00:00:00Z". A zero time rendered
// as a real instant is a plausible wrong answer — the project's worst failure
// class (CLAUDE.md §2.1).
type ContradictionPair struct {
	IDa      string `json:"id_a"`
	ConceptA string `json:"concept_a"`
	IDb      string `json:"id_b"`
	ConceptB string `json:"concept_b"`
	// Status is "detected" (the detector has flagged this pair) or
	// "pending_detection" (an explicit contradicts link exists and the batch
	// detector has not reached it yet). Empty when the engine cannot report it.
	Status string `json:"status,omitempty"`
	// DetectedAt is when the detector flagged the pair. Absent while pending,
	// and absent for markers written before the timestamp was recorded.
	DetectedAt *time.Time `json:"detected_at,omitempty"`
	// DeclaredAt is when an explicit contradicts link was written between the
	// two engrams. Absent when the pair was found by the detector alone.
	DeclaredAt *time.Time `json:"declared_at,omitempty"`
}

// ContradictionsReport is the muninn_contradictions response.
//
// PendingCount is the point of the envelope: the contradiction detector is a
// 30s batch worker, so for up to half a minute after an explicit
// muninn_link(relation="contradicts") no marker exists. Reporting only markers
// made that window return an empty list — the same answer a vault with no
// contradictions gives. The counts let a caller tell "none" from "not computed
// yet" without waiting or guessing.
type ContradictionsReport struct {
	Contradictions []ContradictionPair `json:"contradictions"`
	DetectedCount  int                 `json:"detected_count"`
	PendingCount   int                 `json:"pending_count"`
	// ScanComplete is false when the search for declared-but-undetected links
	// hit its scan cap; PendingCount is then a lower bound, not a total.
	ScanComplete bool   `json:"scan_complete"`
	Note         string `json:"note,omitempty"`
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
	EngramID  string
	Query     []string
	Embedding []float32 // optional client-provided query embedding
}

// ExplainComponents holds the per-component score breakdown.
//
// Every field is a POINTER on purpose: a component that was never computed
// serializes as JSON null ("unknown"), never as 0. A 0 that means "unknown" is
// exactly the silent substitution this project treats as its worst failure
// class (CLAUDE.md §2.1) — and it is worst of all here, in the one tool an
// agent uses to find out why a memory did not come back.
type ExplainComponents struct {
	FullTextRelevance  *float64 `json:"full_text_relevance"`
	SemanticSimilarity *float64 `json:"semantic_similarity"`
	// SemanticSimilarityRaw is the uncalibrated cosine similarity — see
	// activation.ScoreComponents.SemanticSimilarityRaw. Lets an operator see
	// the raw signal (e.g. 0.59) behind a calibrated value that abstained
	// (e.g. 0.07) without a second tool call.
	SemanticSimilarityRaw *float64 `json:"semantic_similarity_raw"`
	DecayFactor           *float64 `json:"decay_factor"`
	HebbianBoost          *float64 `json:"hebbian_boost"`
	AccessFrequency       *float64 `json:"access_frequency"`
	// Confidence is the engram's STORED confidence. It does not depend on the
	// query, so it is non-null whenever the engram exists — including when the
	// query produced no score at all.
	Confidence *float64 `json:"confidence"`
}

// ExplainResult breaks down why an engram scored as it did for a given query.
type ExplainResult struct {
	EngramID string `json:"engram_id"`
	Concept  string `json:"concept"`
	// Found: the engram exists in this vault. Scored: this query's activation
	// run produced a score for it. When Scored is false the component values
	// are null and Note says why — final_score is likewise meaningless.
	Found       bool              `json:"found"`
	Scored      bool              `json:"scored"`
	FinalScore  *float64          `json:"final_score"`
	Components  ExplainComponents `json:"components"`
	FTSMatches  []string          `json:"fts_matches"`
	AssocPath   []string          `json:"assoc_path"`
	WouldReturn bool              `json:"would_return"`
	// Threshold is the bar a default muninn_recall applies in this vault —
	// would_return means "clears that bar", not "was a candidate".
	Threshold float64 `json:"threshold"`
	// Note explains, in plain language, anything the caller would otherwise
	// have to infer from a zero. Empty on the fully-scored happy path.
	Note string `json:"note,omitempty"`
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

// EnrichmentCandidate is one memory returned for agent-managed enrichment.
type EnrichmentCandidate struct {
	ID            string          `json:"id"`
	Concept       string          `json:"concept"`
	Content       string          `json:"content"`
	Summary       string          `json:"summary,omitempty"`
	MemoryType    string          `json:"memory_type,omitempty"`
	TypeLabel     string          `json:"type_label,omitempty"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
	MissingStages []string        `json:"missing_stages"`
	DigestFlags   map[string]bool `json:"digest_flags"`
}

// EnrichmentCandidatesResult is returned by muninn_get_enrichment_candidates.
type EnrichmentCandidatesResult struct {
	Items           []EnrichmentCandidate `json:"items"`
	StagesRequested []string              `json:"stages_requested"`
	Count           int                   `json:"count"`
	NextCursor      string                `json:"next_cursor,omitempty"`
}

// ApplyEnrichmentEntity is one externally generated entity.
type ApplyEnrichmentEntity struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	Confidence float32 `json:"confidence,omitempty"`
}

// ApplyEnrichmentRelationship is one externally generated relationship.
type ApplyEnrichmentRelationship struct {
	FromEntity string  `json:"from_entity"`
	ToEntity   string  `json:"to_entity"`
	RelType    string  `json:"rel_type"`
	Weight     float32 `json:"weight,omitempty"`
}

// ApplyEnrichmentRequest contains explicit enrichment output from an MCP agent.
type ApplyEnrichmentRequest struct {
	ID                string                        `json:"id"`
	ExpectedUpdatedAt string                        `json:"expected_updated_at"`
	Summary           string                        `json:"summary,omitempty"`
	MemoryType        string                        `json:"memory_type,omitempty"`
	TypeLabel         string                        `json:"type_label,omitempty"`
	Entities          []ApplyEnrichmentEntity       `json:"entities,omitempty"`
	Relationships     []ApplyEnrichmentRelationship `json:"relationships,omitempty"`
	StagesCompleted   []string                      `json:"stages_completed,omitempty"`
	Source            string                        `json:"source,omitempty"`
}

// ApplyEnrichmentResult is returned by muninn_apply_enrichment.
type ApplyEnrichmentResult struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	AppliedStages []string        `json:"applied_stages"`
	UpdatedAt     string          `json:"updated_at"`
	DigestFlags   map[string]bool `json:"digest_flags"`
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
	Concept   string    `json:"concept"`
	Content   string    `json:"content"`
	Type      string    `json:"type,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	Ordinal   *int32    `json:"ordinal,omitempty"` // nil = append at end
	Embedding []float32 `json:"embedding,omitempty"`
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
	Type       string    `json:"type"`                 // canonical MemoryType label; always present
	TypeLabel  string    `json:"type_label,omitempty"` // writer-supplied free-form label
	Tags       []string  `json:"tags,omitempty"`
	// Importance is the use-time EffectiveImportance; ImportanceSource is
	// "explicit" or "derived" (same convention as Memory).
	Importance       float64 `json:"importance"`
	ImportanceSource string  `json:"importance_source"`
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
//
// The trailing three fields are the operation-specific "what changed and why".
// They are omitted whenever the recorded operation carries none — including for
// every entry written before the record format carried them. An omitted field
// means unknown, never "empty": absence must not be read as a claim.
type ProvenanceEntry struct {
	Timestamp string `json:"timestamp"` // RFC3339
	Source    string `json:"source"`    // "human", "llm", "inferred", etc.
	AgentID   string `json:"agent_id,omitempty"`
	Operation string `json:"operation"` // "write", "update", "read", etc.
	Note      string `json:"note,omitempty"`
	// PredecessorID is the engram this version replaced (evolve).
	PredecessorID string `json:"predecessor_id,omitempty"`
	// Reason is the caller-supplied justification for the change (evolve).
	Reason string `json:"reason,omitempty"`
	// EffectiveAt is the valid-time instant the change took effect (RFC3339) —
	// distinct from timestamp, which is when the write happened.
	EffectiveAt string `json:"effective_at,omitempty"`
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
	FirstSeen     string                `json:"first_seen,omitempty"` // RFC3339
	UpdatedAt     string                `json:"updated_at,omitempty"` // RFC3339
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
