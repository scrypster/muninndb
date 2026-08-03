package mcp

import (
	"encoding/json"
	"time"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/transport/mbp"
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
	VectorScoreRaw float64 `json:"vector_score_raw,omitempty"`
	EntityBoost    float64 `json:"entity_boost,omitempty"`
	// AbsoluteScore and ContentMatch are #773. Both already existed on
	// activation.ScoreComponents and on the MBP/REST wire, and BOTH were
	// structurally invisible to every MCP agent — reachable only inside
	// annotations.substitution_basis, i.e. only on a COG-28 substituted row.
	//
	// AbsoluteScore is Raw BEFORE the per-query 1/maxRaw rescale, so unlike
	// Score it is comparable ACROSS queries: 0.9 means the same thing on a
	// good query and a garbage one. ContentMatch is the aboutness term
	// (w_sem*semCal + w_fts*ftsCoverage) the relevance calibration is actually
	// stated on. They are the audit trail behind RelevanceBand — the honesty
	// backstop must be readable without a second tool call, exactly as
	// vector_score_raw is for COG-26.
	AbsoluteScore float64 `json:"absolute_score,omitempty"`
	ContentMatch  float64 `json:"content_match,omitempty"`
	// RelevanceBand is #773's ABSOLUTE relevance band for this row:
	// strong | moderate | weak | filter_match | uncalibrated. Recall always
	// sets it; muninn_read never does (this struct is shared, hence omitempty).
	//
	// Read it, NOT `score`: score is renormalized against this query's own best
	// candidate, so the top row is near 1.0 on EVERY query, including one whose
	// answer this vault does not contain. And NOT `confidence`: that is belief
	// that the stored fact is TRUE (COG-10), not a measure of how well it
	// matched. `relevance` below is a third thing again — the engram's stored
	// decay/pruning strength.
	//
	// Deliberately TOP-LEVEL rather than inside Annotations: convert.go's
	// "allocate an annotations object at all" predicate has already silently
	// dropped a field once (#764). A top-level field cannot be dropped by it.
	RelevanceBand string `json:"relevance_band,omitempty"`
	// RelevanceBandBasis names WHY, for filter_match and uncalibrated only.
	RelevanceBandBasis string    `json:"relevance_band_basis,omitempty"`
	Confidence         float32   `json:"confidence"`
	Why                string    `json:"why,omitempty"`
	Tags               []string  `json:"tags,omitempty"`
	State              string    `json:"state,omitempty"`
	Type               string    `json:"type"`                 // canonical MemoryType label ("fact", "decision", ...); always present
	TypeLabel          string    `json:"type_label,omitempty"` // writer-supplied free-form label, e.g. "architectural_decision"
	CreatedAt          time.Time `json:"created_at"`
	// LastAccess is a POINTER for the same reason MemoryAnnotations.Stale is:
	// "we do not know when this was last accessed" must be representable, and it
	// must be sent as ABSENCE rather than as an instant. `omitempty` is a no-op
	// on a time.Time struct, so a nullable field is the only spelling available.
	//
	// The value being omitted is not hypothetical: engine.go stamps
	// mbp.ActivationItem.LastAccess as eng.LastAccess.UnixNano(), and
	// time.Time{}.UnixNano() IS erf.ZeroTimeSentinelNanos, so the #810 decode
	// repair is invisible through that round trip by construction and this field
	// rendered "1754-08-30T22:43:41.128654848Z" — on the SAME ROW where the
	// staleness annotation was omitted as unknown. One response, two
	// incompatible statements about one memory.
	//
	// Scope, deliberately: this is MCP-only. `Memory` lives in this package and
	// is used nowhere outside it, so making it nullable costs no other transport
	// anything. The int64 `last_access` on mbp.ActivationItem — shared by REST,
	// gRPC and MBP by type alias, plus openapi.yaml and the SDKs — is a separate,
	// still-open residual; see invariants.md STO-13.
	LastAccess  *time.Time `json:"last_access,omitempty"`
	AccessCount uint32     `json:"access_count,omitempty"`
	Relevance   float32    `json:"relevance,omitempty"`
	SourceType  string     `json:"source_type,omitempty"`
	Trust       string     `json:"trust,omitempty"` // "verified", "inferred", "external", "untrusted"

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
	// Stale / StaleDays are POINTERS so that "unknown" is representable and is
	// sent as absence rather than as zero. A never-accessed engram (a vault
	// cloned before #810 carries the ERF zero-time sentinel on every record) has
	// no staleness the system can assert; emitting "stale_days": 0,
	// "stale": false would read to an agent as "accessed today" — plausible and
	// wrong. Present-and-zero still means what it always did: accessed today.
	Stale         *bool    `json:"stale,omitempty"`
	StaleDays     *float64 `json:"stale_days,omitempty"`
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
	// UnresolvedContradiction is COG-29 (#764) — ASSERTED, from a declared
	// `contradicts` link that nothing has resolved. This memory is declared to
	// disagree with another one, so it must NOT be read as the answer without
	// checking this annotation: its score is demoted 10% below its earned
	// value, and results stay score-ordered (near-tied rivals land adjacent;
	// a clearly stronger match keeps its rank).
	// Resolve it with muninn_evolve, muninn_forget(not_true_since=…),
	// or muninn_link(relation="supersedes"). Distinct from the advisory
	// conflicts_with above, which is a heuristic annotate=true signal.
	UnresolvedContradiction *mbp.ContradictionConflict `json:"unresolved_contradiction,omitempty"`
	LastVerified            string                     `json:"last_verified,omitempty"` // RFC3339
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
	// Status is the pair's PROVENANCE: "declared" (an explicit contradicts
	// link exists between the two memories) or "detected" (the batch detector
	// found the pair on its own). Empty when the engine cannot report it.
	//
	// "declared" replaced "pending_detection" in #764. A declared
	// contradiction is durable at muninn_link return and is honored by recall
	// on the very next query, so nothing about it is pending — what can still
	// be outstanding is the confidence penalty, reported in
	// ConfidencePenalty below.
	Status string `json:"status,omitempty"`
	// ConfidencePenalty is "pending" or "applied": whether the asynchronous,
	// exactly-once confidence penalty for this pair has fired yet. It runs on
	// a ~30s batch interval and affects only the two memories' confidence
	// scores — never whether the contradiction is recorded or honored.
	ConfidencePenalty string `json:"confidence_penalty,omitempty"`
	// ResolvedBy names why a pair with status "resolved" is no longer a live
	// conflict: "supersedes" (an explicit supersedes link between the two) or
	// "endpoint_retired" (one side was evolved, forgotten, archived, or its
	// validity window elapsed). Empty on a live pair. Before #764 nothing in
	// the product cleared a declared contradiction, so resolving one the way
	// the tool advises left the pair listed forever.
	ResolvedBy string `json:"resolved_by,omitempty"`
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
	// ResolvedCount is how many recorded pairs are no longer live conflicts.
	// They are still listed (status "resolved", with resolved_by) rather than
	// omitted, but they are not outstanding work.
	ResolvedCount int `json:"resolved_count"`
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
