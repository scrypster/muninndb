package mql

import "time"

// ActivateQuery is the top-level AST node for an ACTIVATE query.
type ActivateQuery struct {
	Vault        string
	Context      []string
	Where        Predicate // nil if no WHERE clause
	MaxResults   int       // 0 = use default
	Hops         int       // 0 = use default
	MinRelevance float32   // 0 = use default
}

// Predicate is the base interface for WHERE predicates.
type Predicate interface {
	predicateNode()
}

// StatePredicate matches engrams by lifecycle state.
type StatePredicate struct {
	State string
}

func (*StatePredicate) predicateNode() {}

// ScorePredicate matches engrams by a score field (relevance or confidence).
// Field: "relevance" or "confidence"
// Op: ">" or ">="
// Value: threshold (0.0-1.0)
type ScorePredicate struct {
	Field string // "relevance" or "confidence"
	Op    string // ">" or ">="
	Value float32
}

func (*ScorePredicate) predicateNode() {}

// TagPredicate matches engrams with a specific tag.
type TagPredicate struct {
	Tag string
}

func (*TagPredicate) predicateNode() {}

// CreatorPredicate matches engrams created by a specific creator.
type CreatorPredicate struct {
	Creator string
}

func (*CreatorPredicate) predicateNode() {}

// CreatedAfterPredicate matches engrams created after a specific time.
type CreatedAfterPredicate struct {
	After time.Time
}

func (*CreatedAfterPredicate) predicateNode() {}

// AndPredicate combines two predicates with AND logic.
type AndPredicate struct {
	Left  Predicate
	Right Predicate
}

func (*AndPredicate) predicateNode() {}

// OrPredicate combines two predicates with OR logic.
type OrPredicate struct {
	Left  Predicate
	Right Predicate
}

func (*OrPredicate) predicateNode() {}

// ProvenanceSourcePredicate matches engrams by provenance source.
type ProvenanceSourcePredicate struct {
	Source string // "human", "llm", "document", "inferred", "external", "working_mem", "synthetic"
}

func (*ProvenanceSourcePredicate) predicateNode() {}

// ProvenanceAgentPredicate matches engrams by provenance agent ID.
type ProvenanceAgentPredicate struct {
	Agent string
}

func (*ProvenanceAgentPredicate) predicateNode() {}

// Query is the interface for all MQL query types.
type Query interface {
	mqlQuery()
}

// RecallEpisodeQuery retrieves frames from an episode.
type RecallEpisodeQuery struct {
	EpisodeID string // ULID as string
	Frames    int    // 0 = all frames
}

func (*RecallEpisodeQuery) mqlQuery() {}

// TraverseQuery walks the association graph from a starting engram.
type TraverseQuery struct {
	StartID   string  // engram ID
	Hops      int     // number of hops
	MinWeight float32 // minimum edge weight threshold
}

func (*TraverseQuery) mqlQuery() {}

// ConsolidateQuery triggers a consolidation run on a vault.
type ConsolidateQuery struct {
	Vault  string // vault name
	DryRun bool   // if true, no mutations occur
}

func (*ConsolidateQuery) mqlQuery() {}

// WorkingMemoryQuery retrieves working memory items for a session.
type WorkingMemoryQuery struct {
	SessionID string
}

func (*WorkingMemoryQuery) mqlQuery() {}

// ActivateQuery is also a Query for backward compatibility.
func (*ActivateQuery) mqlQuery() {}
