"""Async MuninnDB client."""

import asyncio
import json
import random

import httpx

from .errors import (
    MuninnAuthError,
    MuninnConnectionError,
    MuninnConflict,
    MuninnError,
    MuninnNotFound,
    MuninnServerError,
    MuninnTimeoutError,
)
from .sse import SSEStream
from .types import (
    ActivateResponse,
    ActivationItem,
    AssociationItem,
    BatchWriteResponse,
    BatchWriteResult,
    BriefSentence,
    CoherenceResult,
    ConsolidateResponse,
    ContradictionItem,
    ContradictionsResponse,
    DecideResponse,
    DeletedEngram,
    EngramItem,
    EvolveResponse,
    ExplainComponents,
    ExplainResponse,
    ListDeletedResponse,
    ListEngramsResponse,
    ReadResponse,
    RestoreResponse,
    RetryEnrichResponse,
    SessionEntry,
    SessionResponse,
    SetStateResponse,
    StatResponse,
    TraversalEdge,
    TraversalNode,
    TraverseResponse,
    WriteResponse,
)


class MuninnClient:
    """Async client for MuninnDB REST API.

    The client uses httpx for async HTTP and supports automatic retry with
    exponential backoff for transient failures.

    Usage:
        async with MuninnClient("http://localhost:8476") as client:
            eng_id = await client.write(
                vault="default",
                concept="memory concept",
                content="memory content"
            )
            results = await client.activate(
                vault="default",
                context=["search query"]
            )
            async for push in client.subscribe(vault="default"):
                print(f"New engram: {push.engram_id}")
                break

    Args:
        base_url: Base URL of MuninnDB server (default: http://localhost:8476)
        token: Optional Bearer token for authentication
        timeout: Request timeout in seconds (default: 5.0)
        max_retries: Maximum retry attempts for transient errors (default: 3)
        retry_backoff: Initial backoff multiplier for retries (default: 0.5)
        max_connections: Max concurrent connections (default: 20)
        keepalive_connections: Max keepalive connections (default: 10)
    """

    def __init__(
        self,
        base_url: str = "http://localhost:8476",
        token: str | None = None,
        timeout: float = 5.0,
        max_retries: int = 3,
        retry_backoff: float = 0.5,
        max_connections: int = 20,
        keepalive_connections: int = 10,
    ):
        self._base_url = base_url.rstrip("/")
        self._token = token
        self._timeout = timeout
        self._max_retries = max_retries
        self._retry_backoff = retry_backoff
        self._max_connections = max_connections
        self._keepalive_connections = keepalive_connections
        self._http: httpx.AsyncClient | None = None

    async def __aenter__(self):
        """Enter async context."""
        self._http = httpx.AsyncClient(
            base_url=self._base_url,
            timeout=self._timeout,
            limits=httpx.Limits(
                max_connections=self._max_connections,
                max_keepalive_connections=self._keepalive_connections,
            ),
            headers=self._default_headers(),
        )
        return self

    async def __aexit__(self, *args):
        """Exit async context."""
        if self._http:
            await self._http.aclose()

    async def write(
        self,
        vault: str = "default",
        concept: str = "",
        content: str = "",
        tags: list[str] | None = None,
        confidence: float = 0.9,
        stability: float = 0.5,
        memory_type: int | None = None,
        type_label: str | None = None,
        summary: str | None = None,
        entities: list[dict] | None = None,
        relationships: list[dict] | None = None,
    ) -> WriteResponse:
        """Write an engram to the database.

        Args:
            vault: Vault name (default: "default")
            concept: Concept/title for this engram
            content: Main content/body
            tags: Optional list of tags for categorization
            confidence: Confidence score 0-1 (default: 0.9)
            stability: Stability score 0-1 (default: 0.5)
            memory_type: Memory type enum (0=unknown, 1=fact, 2=decision, etc.)
            type_label: Free-form type label (e.g. "architecture_decision")
            summary: Caller-provided summary for inline enrichment
            entities: Caller-provided entities [{"name": "...", "type": "..."}]
            relationships: Caller-provided relationships [{"target_id": "...", "relation": "...", "weight": 1.0}]

        Returns:
            WriteResponse with id, created_at, and optional hint

        Raises:
            MuninnError: If write fails
        """
        body: dict = {
            "vault": vault,
            "concept": concept,
            "content": content,
            "confidence": confidence,
            "stability": stability,
        }
        if tags:
            body["tags"] = tags
        if memory_type is not None:
            body["memory_type"] = memory_type
        if type_label is not None:
            body["type_label"] = type_label
        if summary is not None:
            body["summary"] = summary
        if entities is not None:
            body["entities"] = entities
        if relationships is not None:
            body["relationships"] = relationships

        response = await self._request("POST", "/api/engrams", json=body)
        return WriteResponse(
            id=response.get("id", ""),
            created_at=response.get("created_at", 0),
            hint=response.get("hint"),
        )

    async def write_batch(
        self,
        vault: str = "default",
        engrams: list[dict] | None = None,
    ) -> BatchWriteResponse:
        """Write multiple engrams in a single batch call.

        More efficient than calling write() repeatedly. Maximum 50 per batch.
        Each engram dict can contain: concept, content, tags, confidence,
        stability, memory_type, type_label, summary, entities, relationships.

        Args:
            vault: Default vault for engrams that don't specify one
            engrams: List of engram dicts to write

        Returns:
            BatchWriteResponse with per-item results

        Raises:
            MuninnError: If batch write fails
        """
        if not engrams:
            raise MuninnError("engrams list is required and must not be empty")
        if len(engrams) > 50:
            raise MuninnError("batch size exceeds maximum of 50")

        items = []
        for eng in engrams:
            item = dict(eng)
            if "vault" not in item:
                item["vault"] = vault
            items.append(item)

        response = await self._request(
            "POST", "/api/engrams/batch", json={"engrams": items}
        )

        results = [
            BatchWriteResult(
                index=r.get("index", i),
                id=r.get("id", ""),
                status=r.get("status", "error"),
                error=r.get("error"),
            )
            for i, r in enumerate(response.get("results", []))
        ]
        return BatchWriteResponse(results=results)

    async def activate(
        self,
        vault: str = "default",
        context: list[str] | None = None,
        max_results: int = 10,
        threshold: float = 0.1,
        max_hops: int = 0,
        include_why: bool = False,
        brief_mode: str = "auto",
    ) -> ActivateResponse:
        """Activate memory using semantic search and graph traversal.

        Args:
            vault: Vault name (default: "default")
            context: List of query terms/context
            max_results: Max results to return (default: 10)
            threshold: Min activation score threshold (default: 0.1)
            max_hops: Max graph hops to traverse (default: 0)
            include_why: Include reasoning/why field (default: False)
            brief_mode: Brief extraction mode - "auto", "extractive", "abstractive" (default: "auto")

        Returns:
            ActivateResponse with activations and optional brief

        Raises:
            MuninnError: If activation fails
        """
        if context is None:
            context = []

        body = {
            "vault": vault,
            "context": context,
            "max_results": max_results,
            "threshold": threshold,
            "max_hops": max_hops,
            "include_why": include_why,
            "brief_mode": brief_mode,
        }

        response = await self._request("POST", "/api/activate", json=body)

        activations = [
            ActivationItem(
                id=item.get("id", ""),
                concept=item.get("concept", ""),
                content=item.get("content", ""),
                score=item.get("score", 0.0),
                confidence=item.get("confidence", 0.0),
                why=item.get("why"),
                hop_path=item.get("hop_path"),
                dormant=item.get("dormant", False),
                memory_type=item.get("memory_type", 0),
                type_label=item.get("type_label", ""),
            )
            for item in response.get("activations", [])
        ]

        brief = None
        if response.get("brief"):
            brief = [
                BriefSentence(
                    engram_id=sent.get("engram_id", ""),
                    text=sent.get("text", ""),
                    score=sent.get("score", 0.0),
                )
                for sent in response["brief"]
            ]

        return ActivateResponse(
            query_id=response.get("query_id", ""),
            total_found=response.get("total_found", 0),
            activations=activations,
            latency_ms=response.get("latency_ms", 0.0),
            brief=brief,
        )

    async def read(self, id: str, vault: str = "default") -> ReadResponse:
        """Read a specific engram by ID.

        Args:
            id: Engram ULID
            vault: Vault name (default: "default")

        Returns:
            ReadResponse with engram details

        Raises:
            MuninnNotFound: If engram doesn't exist
            MuninnError: If read fails
        """
        response = await self._request("GET", f"/api/engrams/{id}", params={"vault": vault})

        coherence = response.get("coherence")
        return ReadResponse(
            id=response.get("id", ""),
            concept=response.get("concept", ""),
            content=response.get("content", ""),
            confidence=response.get("confidence", 0.0),
            relevance=response.get("relevance", 0.0),
            stability=response.get("stability", 0.0),
            access_count=response.get("access_count", 0),
            tags=response.get("tags", []),
            state=response.get("state", ""),
            created_at=response.get("created_at", 0),
            updated_at=response.get("updated_at", 0),
            last_access=response.get("last_access"),
        )

    async def forget(self, id: str, vault: str = "default", hard: bool = False) -> bool:
        """Delete an engram (soft or hard delete).

        Args:
            id: Engram ULID
            vault: Vault name (default: "default")
            hard: If True, hard delete (cannot recover). If False, soft delete (default: False)

        Returns:
            True if deletion successful

        Raises:
            MuninnNotFound: If engram doesn't exist
            MuninnError: If deletion fails
        """
        if hard:
            await self._request(
                "POST",
                f"/api/engrams/{id}/forget",
                params={"vault": vault, "hard": "true"},
            )
        else:
            await self._request(
                "DELETE",
                f"/api/engrams/{id}",
                params={"vault": vault},
            )
        return True

    async def link(
        self,
        source_id: str,
        target_id: str,
        vault: str = "default",
        rel_type: int = 5,
        weight: float = 1.0,
    ) -> bool:
        """Create an association/link between two engrams.

        Args:
            source_id: Source engram ULID
            target_id: Target engram ULID
            vault: Vault name (default: "default")
            rel_type: Relationship type code (default: 5)
            weight: Link weight/strength (default: 1.0)

        Returns:
            True if link created successfully

        Raises:
            MuninnError: If link creation fails
        """
        body = {
            "vault": vault,
            "source_id": source_id,
            "target_id": target_id,
            "rel_type": rel_type,
            "weight": weight,
        }
        await self._request("POST", "/api/link", json=body)
        return True

    async def stats(self) -> StatResponse:
        """Get database statistics including coherence scores.

        Returns:
            StatResponse with engram count, vault count, storage bytes, and coherence

        Raises:
            MuninnError: If stats request fails
        """
        response = await self._request("GET", "/api/stats")

        coherence = None
        if response.get("coherence"):
            coherence = {
                vault_name: CoherenceResult(
                    score=data.get("score", 0.0),
                    orphan_ratio=data.get("orphan_ratio", 0.0),
                    contradiction_density=data.get("contradiction_density", 0.0),
                    duplication_pressure=data.get("duplication_pressure", 0.0),
                    temporal_variance=data.get("temporal_variance", 0.0),
                    total_engrams=data.get("total_engrams", 0),
                )
                for vault_name, data in response["coherence"].items()
            }

        return StatResponse(
            engram_count=response.get("engram_count", 0),
            vault_count=response.get("vault_count", 0),
            storage_bytes=response.get("storage_bytes", 0),
            coherence=coherence,
        )

    def subscribe(
        self,
        vault: str = "default",
        push_on_write: bool = True,
        threshold: float = 0.0,
    ) -> SSEStream:
        """Subscribe to vault events via Server-Sent Events (SSE).

        This returns an async iterable that yields Push events when engrams are
        written to the vault. The stream automatically reconnects on network errors.

        Usage:
            stream = client.subscribe(vault="default")
            async for push in stream:
                print(f"New engram: {push.engram_id}")
                if condition:
                    await stream.close()

        Args:
            vault: Vault to subscribe to (default: "default")
            push_on_write: Emit push events on new writes (default: True)
            threshold: Min activation threshold for push events (default: 0.0)

        Returns:
            SSEStream async iterable

        Raises:
            MuninnError: If subscription fails
        """
        params = {
            "vault": vault,
            "push_on_write": str(push_on_write).lower(),
        }
        if threshold:
            params["threshold"] = str(threshold)

        return SSEStream(self, "/api/subscribe", params)

    async def evolve(
        self,
        id: str,
        new_content: str,
        reason: str,
        vault: str = "default",
    ) -> EvolveResponse:
        """Evolve an engram's content, creating a new version.

        Args:
            id: Engram ULID to evolve
            new_content: Updated content
            reason: Reason for the evolution
            vault: Vault name (default: "default")

        Returns:
            EvolveResponse with the new engram ID
        """
        body = {"new_content": new_content, "reason": reason, "vault": vault}
        response = await self._request("POST", f"/api/engrams/{id}/evolve", json=body)
        return EvolveResponse(id=response.get("id", ""))

    async def consolidate(
        self,
        ids: list[str],
        merged_content: str,
        vault: str = "default",
    ) -> ConsolidateResponse:
        """Consolidate multiple engrams into one.

        Args:
            ids: List of engram ULIDs to consolidate
            merged_content: Combined content for the merged engram
            vault: Vault name (default: "default")

        Returns:
            ConsolidateResponse with new ID, archived IDs, and any warnings
        """
        body = {"vault": vault, "ids": ids, "merged_content": merged_content}
        response = await self._request("POST", "/api/consolidate", json=body)
        return ConsolidateResponse(
            id=response.get("id", ""),
            archived=response.get("archived", []),
            warnings=response.get("warnings"),
        )

    async def decide(
        self,
        decision: str,
        rationale: str,
        alternatives: list[str] | None = None,
        evidence_ids: list[str] | None = None,
        vault: str = "default",
    ) -> DecideResponse:
        """Record a decision as an engram.

        Args:
            decision: The decision made
            rationale: Reasoning behind the decision
            alternatives: Alternative options considered
            evidence_ids: Engram IDs that informed the decision
            vault: Vault name (default: "default")

        Returns:
            DecideResponse with the decision engram ID
        """
        body: dict = {"vault": vault, "decision": decision, "rationale": rationale}
        if alternatives:
            body["alternatives"] = alternatives
        if evidence_ids:
            body["evidence_ids"] = evidence_ids
        response = await self._request("POST", "/api/decide", json=body)
        return DecideResponse(id=response.get("id", ""))

    async def restore(self, id: str, vault: str = "default") -> RestoreResponse:
        """Restore a soft-deleted engram.

        Args:
            id: Engram ULID to restore
            vault: Vault name (default: "default")

        Returns:
            RestoreResponse with restored engram details
        """
        response = await self._request(
            "POST", f"/api/engrams/{id}/restore", params={"vault": vault}
        )
        return RestoreResponse(
            id=response.get("id", ""),
            concept=response.get("concept", ""),
            restored=response.get("restored", False),
            state=response.get("state", ""),
        )

    async def traverse(
        self,
        start_id: str,
        max_hops: int = 2,
        max_nodes: int = 20,
        rel_types: list[str] | None = None,
        vault: str = "default",
    ) -> TraverseResponse:
        """Traverse the association graph from a starting engram.

        Args:
            start_id: Starting engram ULID
            max_hops: Maximum hops to traverse (default: 2)
            max_nodes: Maximum nodes to return (default: 20)
            rel_types: Filter by relationship types
            vault: Vault name (default: "default")

        Returns:
            TraverseResponse with nodes, edges, and stats
        """
        body: dict = {
            "vault": vault,
            "start_id": start_id,
            "max_hops": max_hops,
            "max_nodes": max_nodes,
        }
        if rel_types:
            body["rel_types"] = rel_types
        response = await self._request("POST", "/api/traverse", json=body)
        nodes = [
            TraversalNode(
                id=n.get("id", ""),
                concept=n.get("concept", ""),
                hop_dist=n.get("hop_dist", 0),
                summary=n.get("summary"),
            )
            for n in response.get("nodes", [])
        ]
        edges = [
            TraversalEdge(
                from_id=e.get("from_id", ""),
                to_id=e.get("to_id", ""),
                rel_type=e.get("rel_type", ""),
                weight=e.get("weight", 0.0),
            )
            for e in response.get("edges", [])
        ]
        return TraverseResponse(
            nodes=nodes,
            edges=edges,
            total_reachable=response.get("total_reachable", 0),
            query_ms=response.get("query_ms", 0.0),
        )

    async def explain(
        self,
        engram_id: str,
        query: list[str],
        vault: str = "default",
    ) -> ExplainResponse:
        """Explain why an engram would or wouldn't be returned for a query.

        Args:
            engram_id: Engram ULID to explain
            query: Query context terms
            vault: Vault name (default: "default")

        Returns:
            ExplainResponse with scoring breakdown
        """
        body = {"vault": vault, "engram_id": engram_id, "query": query}
        response = await self._request("POST", "/api/explain", json=body)
        comp = response.get("components", {})
        return ExplainResponse(
            engram_id=response.get("engram_id", ""),
            concept=response.get("concept", ""),
            final_score=response.get("final_score", 0.0),
            components=ExplainComponents(
                full_text_relevance=comp.get("full_text_relevance", 0.0),
                semantic_similarity=comp.get("semantic_similarity", 0.0),
                decay_factor=comp.get("decay_factor", 0.0),
                hebbian_boost=comp.get("hebbian_boost", 0.0),
                access_frequency=comp.get("access_frequency", 0.0),
                confidence=comp.get("confidence", 0.0),
            ),
            fts_matches=response.get("fts_matches", []),
            assoc_path=response.get("assoc_path", []),
            would_return=response.get("would_return", False),
            threshold=response.get("threshold", 0.0),
        )

    async def set_state(
        self,
        id: str,
        state: str,
        reason: str = "",
        vault: str = "default",
    ) -> SetStateResponse:
        """Set the state of an engram.

        Args:
            id: Engram ULID
            state: New state value
            reason: Reason for the state change
            vault: Vault name (default: "default")

        Returns:
            SetStateResponse with updated state
        """
        body: dict = {"state": state, "vault": vault}
        if reason:
            body["reason"] = reason
        response = await self._request("PUT", f"/api/engrams/{id}/state", json=body)
        return SetStateResponse(
            id=response.get("id", ""),
            state=response.get("state", ""),
            updated=response.get("updated", False),
        )

    async def list_deleted(
        self,
        vault: str = "default",
        limit: int = 20,
    ) -> ListDeletedResponse:
        """List soft-deleted engrams that can be restored.

        Args:
            vault: Vault name (default: "default")
            limit: Maximum number of results (default: 20)

        Returns:
            ListDeletedResponse with deleted engrams and count
        """
        response = await self._request(
            "GET", "/api/deleted", params={"vault": vault, "limit": str(limit)}
        )
        deleted = [
            DeletedEngram(
                id=d.get("id", ""),
                concept=d.get("concept", ""),
                deleted_at=d.get("deleted_at", 0),
                recoverable_until=d.get("recoverable_until", 0),
                tags=d.get("tags"),
            )
            for d in response.get("deleted", [])
        ]
        return ListDeletedResponse(
            deleted=deleted,
            count=response.get("count", 0),
        )

    async def retry_enrich(self, id: str, vault: str = "default") -> RetryEnrichResponse:
        """Retry enrichment plugins for an engram.

        Args:
            id: Engram ULID
            vault: Vault name (default: "default")

        Returns:
            RetryEnrichResponse with queued and completed plugins
        """
        response = await self._request(
            "POST", f"/api/engrams/{id}/retry-enrich", params={"vault": vault}
        )
        return RetryEnrichResponse(
            engram_id=response.get("engram_id", ""),
            plugins_queued=response.get("plugins_queued", []),
            already_complete=response.get("already_complete", []),
            note=response.get("note"),
        )

    async def contradictions(self, vault: str = "default") -> ContradictionsResponse:
        """List detected contradictions in a vault.

        Args:
            vault: Vault name (default: "default")

        Returns:
            ContradictionsResponse with contradiction pairs
        """
        response = await self._request(
            "GET", "/api/contradictions", params={"vault": vault}
        )
        items = [
            ContradictionItem(
                id_a=c.get("id_a", ""),
                concept_a=c.get("concept_a", ""),
                id_b=c.get("id_b", ""),
                concept_b=c.get("concept_b", ""),
                detected_at=c.get("detected_at", 0),
            )
            for c in response.get("contradictions", [])
        ]
        return ContradictionsResponse(contradictions=items)

    async def guide(self, vault: str = "default") -> str:
        """Get a natural-language guide/summary of a vault's contents.

        Args:
            vault: Vault name (default: "default")

        Returns:
            Guide text as a string
        """
        response = await self._request(
            "GET", "/api/guide", params={"vault": vault}
        )
        return response.get("guide", "")

    async def list_engrams(
        self,
        vault: str = "default",
        limit: int = 20,
        offset: int = 0,
    ) -> ListEngramsResponse:
        """List engrams with pagination.

        Args:
            vault: Vault name (default: "default")
            limit: Maximum number of results (default: 20)
            offset: Pagination offset (default: 0)

        Returns:
            ListEngramsResponse with engrams and pagination info
        """
        response = await self._request(
            "GET",
            "/api/engrams",
            params={"vault": vault, "limit": str(limit), "offset": str(offset)},
        )
        engrams = [
            EngramItem(
                id=e.get("id", ""),
                concept=e.get("concept", ""),
                content=e.get("content", ""),
                confidence=e.get("confidence", 0.0),
                tags=e.get("tags"),
                vault=e.get("vault", ""),
                created_at=e.get("created_at", 0),
            )
            for e in response.get("engrams", [])
        ]
        return ListEngramsResponse(
            engrams=engrams,
            total=response.get("total", 0),
            limit=response.get("limit", limit),
            offset=response.get("offset", offset),
        )

    async def get_links(self, id: str, vault: str = "default") -> list[AssociationItem]:
        """Get associations/links for an engram.

        Args:
            id: Engram ULID
            vault: Vault name (default: "default")

        Returns:
            List of AssociationItem
        """
        response = await self._request(
            "GET", f"/api/engrams/{id}/links", params={"vault": vault}
        )
        links = response if isinstance(response, list) else response.get("links", [])
        return [
            AssociationItem(
                target_id=link.get("targetId", link.get("target_id", "")),
                rel_type=link.get("relType", link.get("rel_type", 0)),
                weight=link.get("weight", 0.0),
            )
            for link in links
        ]

    async def list_vaults(self) -> list[str]:
        """List all available vaults.

        Returns:
            List of vault names
        """
        response = await self._request("GET", "/api/vaults")
        return response.get("vaults", [])

    async def session(
        self,
        vault: str = "default",
        since: str | None = None,
        limit: int = 50,
        offset: int = 0,
    ) -> SessionResponse:
        """Get session activity for a vault.

        Args:
            vault: Vault name (default: "default")
            since: ISO 8601 timestamp to filter from
            limit: Maximum entries (default: 50)
            offset: Pagination offset (default: 0)

        Returns:
            SessionResponse with activity entries
        """
        params: dict = {
            "vault": vault,
            "limit": str(limit),
            "offset": str(offset),
        }
        if since:
            params["since"] = since
        response = await self._request("GET", "/api/session", params=params)
        entries = [
            SessionEntry(
                id=e.get("id", ""),
                concept=e.get("concept", ""),
                created_at=e.get("created_at", e.get("createdAt", 0)),
            )
            for e in response.get("entries", [])
        ]
        return SessionResponse(
            entries=entries,
            total=response.get("total", 0),
            offset=response.get("offset", offset),
            limit=response.get("limit", limit),
        )

    async def health(self) -> bool:
        """Check if MuninnDB server is healthy.

        Returns:
            True if server responds with 200 OK

        Raises:
            MuninnError: If health check fails
        """
        try:
            response = await self._request("GET", "/api/health")
            return response.get("status") == "ok"
        except MuninnError:
            return False

    async def _request(self, method: str, path: str, **kwargs) -> dict:
        """Make an HTTP request with automatic retry logic.

        Retries on transient errors (502, 503, 504, connection/read errors).
        Does not retry on 4xx errors. Uses exponential backoff with jitter.

        Args:
            method: HTTP method (GET, POST, DELETE, etc)
            path: URL path relative to base_url
            **kwargs: Additional arguments to pass to httpx

        Returns:
            Parsed JSON response as dict

        Raises:
            MuninnAuthError: 401 Unauthorized
            MuninnNotFound: 404 Not Found
            MuninnConflict: 409 Conflict
            MuninnServerError: 5xx errors
            MuninnTimeoutError: Request timeout
            MuninnConnectionError: Connection error
            MuninnError: Other HTTP errors
        """
        if not self._http:
            raise MuninnError("Client not initialized. Use 'async with' context manager.")

        attempt = 0
        while attempt <= self._max_retries:
            try:
                response = await self._http.request(method, path, **kwargs)
                self._raise_for_status(response)
                return response.json()

            except (httpx.ConnectError, httpx.ReadError, httpx.RemoteProtocolError) as e:
                if attempt >= self._max_retries:
                    raise MuninnConnectionError(f"Connection failed: {str(e)}")
                await self._backoff(attempt)
                attempt += 1

            except httpx.ReadTimeout as e:
                if attempt >= self._max_retries:
                    raise MuninnTimeoutError(f"Request timeout: {str(e)}")
                await self._backoff(attempt)
                attempt += 1

            except httpx.HTTPStatusError as e:
                # Don't retry on 4xx (except certain ones), do retry on 5xx
                if 500 <= e.response.status_code < 600:
                    if attempt >= self._max_retries:
                        self._raise_for_status(e.response)
                    await self._backoff(attempt)
                    attempt += 1
                else:
                    self._raise_for_status(e.response)

            except MuninnError:
                raise

        raise MuninnError("Max retries exceeded")

    async def _backoff(self, attempt: int):
        """Wait with exponential backoff + jitter.

        Args:
            attempt: Attempt number (0-indexed)
        """
        delay = self._retry_backoff * (2 ** attempt) + random.uniform(0, 0.1)
        await asyncio.sleep(delay)

    def _default_headers(self) -> dict:
        """Build default request headers."""
        headers = {"Content-Type": "application/json"}
        if self._token:
            headers["Authorization"] = f"Bearer {self._token}"
        return headers

    def _raise_for_status(self, response: httpx.Response):
        """Convert httpx response to appropriate MuninnError.

        Args:
            response: httpx Response object

        Raises:
            Appropriate MuninnError subclass
        """
        if response.status_code == 401:
            raise MuninnAuthError(
                "Authentication required. Provide token= parameter to MuninnClient.",
                401,
            )
        elif response.status_code == 404:
            raise MuninnNotFound(f"Not found: {response.text}", 404)
        elif response.status_code == 409:
            raise MuninnConflict(f"Conflict: {response.text}", 409)
        elif 500 <= response.status_code < 600:
            raise MuninnServerError(
                f"Server error {response.status_code}: {response.text}",
                response.status_code,
            )
        elif response.status_code >= 400:
            raise MuninnError(
                f"Client error {response.status_code}: {response.text}",
                response.status_code,
            )
