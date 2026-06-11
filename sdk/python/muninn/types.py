"""MuninnDB type definitions."""

from dataclasses import dataclass, field
from typing import Any


@dataclass
class WriteRequest:
    """Request to write an engram."""

    vault: str
    concept: str
    content: str
    tags: list[str] | None = None
    confidence: float = 0.9
    stability: float = 0.5
    embedding: list[float] | None = None
    associations: dict[str, Any] | None = None
    memory_type: int | None = None
    type_label: str | None = None
    summary: str | None = None
    entities: list[dict] | None = None  # each: {"name": str, "type": str}
    relationships: list[dict] | None = None  # each: {"target_id": str, "relation": str, "weight": float}


@dataclass
class BatchWriteResult:
    """Result for a single engram in a batch write."""

    index: int
    status: str
    id: str | None = None
    error: str | None = None


@dataclass
class BatchWriteResponse:
    """Response from a batch write operation."""

    results: list[BatchWriteResult]


@dataclass
class WriteResponse:
    """Response from writing an engram."""

    id: str
    created_at: int
    hint: str | None = None


@dataclass
class ActivateRequest:
    """Request to activate memory."""

    vault: str
    context: list[str]
    max_results: int = 10
    threshold: float = 0.1
    max_hops: int = 0
    include_why: bool = False
    brief_mode: str = "auto"


@dataclass
class ActivationItem:
    """A single activated memory item."""

    id: str
    concept: str
    content: str
    score: float
    confidence: float
    why: str | None = None
    hop_path: list[str] | None = None
    dormant: bool = False
    memory_type: int = 0
    type_label: str = ""


@dataclass
class BriefSentence:
    """A sentence extracted by brief mode."""

    engram_id: str
    text: str
    score: float


@dataclass
class ActivateResponse:
    """Response from activating memory."""

    query_id: str
    total_found: int
    activations: list[ActivationItem]
    latency_ms: float = 0.0
    brief: list[BriefSentence] | None = None


@dataclass
class ReadResponse:
    """Response from reading an engram."""

    id: str
    concept: str
    content: str
    confidence: float
    relevance: float
    stability: float
    access_count: int
    tags: list[str]
    state: str
    created_at: int
    updated_at: int
    last_access: int | None = None
    coherence: "dict[str, CoherenceResult] | None" = None


@dataclass
class CoherenceResult:
    """Coherence metrics for a vault."""

    score: float
    orphan_ratio: float
    contradiction_density: float
    duplication_pressure: float
    temporal_variance: float
    total_engrams: int


@dataclass
class StatResponse:
    """Response from stats endpoint."""

    engram_count: int
    vault_count: int
    storage_bytes: int
    coherence: dict[str, CoherenceResult] | None = None


@dataclass
class PushEngram:
    """Engram payload nested inside an SSE push event."""

    id: str | None = None
    concept: str | None = None
    content: str | None = None


@dataclass
class Push:
    """SSE push event from subscription."""

    subscription_id: str
    trigger: str
    push_number: int
    engram_id: str | None = None
    at: int | None = None
    score: float | None = None
    engram: PushEngram | None = None
    why: str | None = None


@dataclass
class EvolveResponse:
    """Response from evolving an engram."""

    id: str


@dataclass
class ConsolidateResponse:
    """Response from consolidating engrams."""

    id: str
    archived: list[str]
    warnings: list[str] | None = None


@dataclass
class DecideResponse:
    """Response from recording a decision."""

    id: str


@dataclass
class RestoreResponse:
    """Response from restoring a deleted engram."""

    id: str
    concept: str
    restored: bool
    state: str


@dataclass
class TraversalNode:
    """A node in a graph traversal result."""

    id: str
    concept: str
    hop_dist: int
    summary: str | None = None


@dataclass
class TraversalEdge:
    """An edge in a graph traversal result."""

    from_id: str
    to_id: str
    rel_type: str
    weight: float


@dataclass
class TraverseResponse:
    """Response from graph traversal."""

    nodes: list[TraversalNode]
    edges: list[TraversalEdge]
    total_reachable: int
    query_ms: float


@dataclass
class ExplainComponents:
    """Scoring components for an explain result."""

    full_text_relevance: float
    semantic_similarity: float
    decay_factor: float
    hebbian_boost: float
    access_frequency: float
    confidence: float


@dataclass
class ExplainResponse:
    """Response from explaining an engram's score."""

    engram_id: str
    concept: str
    final_score: float
    components: ExplainComponents
    fts_matches: list[str]
    assoc_path: list[str]
    would_return: bool
    threshold: float


@dataclass
class SetStateResponse:
    """Response from setting engram state."""

    id: str
    state: str
    updated: bool


@dataclass
class DeletedEngram:
    """A deleted engram in the trash."""

    id: str
    concept: str
    deleted_at: int
    recoverable_until: int
    tags: list[str] | None = None


@dataclass
class ListDeletedResponse:
    """Response from listing deleted engrams."""

    deleted: list[DeletedEngram]
    count: int


@dataclass
class RetryEnrichResponse:
    """Response from retrying enrichment."""

    engram_id: str
    plugins_queued: list[str]
    already_complete: list[str]
    note: str | None = None


@dataclass
class ContradictionItem:
    """A detected contradiction between two engrams."""

    id_a: str
    concept_a: str
    id_b: str
    concept_b: str
    detected_at: int


@dataclass
class ContradictionsResponse:
    """Response from listing contradictions."""

    contradictions: list[ContradictionItem]


@dataclass
class EngramItem:
    """An engram summary in a list response."""

    id: str
    concept: str
    content: str
    confidence: float
    tags: list[str] | None = None
    vault: str = ""
    created_at: int = 0


@dataclass
class ListEngramsResponse:
    """Response from listing engrams."""

    engrams: list[EngramItem]
    total: int
    limit: int
    offset: int


@dataclass
class AssociationItem:
    """An association/link from an engram."""

    target_id: str
    rel_type: int
    weight: float


@dataclass
class SessionEntry:
    """An entry in session activity."""

    id: str
    concept: str
    created_at: int


@dataclass
class SessionResponse:
    """Response from session activity query."""

    entries: list[SessionEntry]
    total: int
    offset: int
    limit: int
