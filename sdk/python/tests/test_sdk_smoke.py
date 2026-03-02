"""SDK assertion-based smoke tests.

These tests run against a LIVE MuninnDB server. They are NOT unit tests — they
require a running server and will fail with a connection error if one is not
available. This is intentional: the purpose is to catch regressions in the SDK
or server before they ship.

Configure the server address with environment variables:
    MUNINNDB_URL   — base URL (default: http://localhost:8476)
    MUNINNDB_TOKEN — bearer token (default: None / no auth)

Run:
    PYTHONPATH=. pytest tests/test_sdk_smoke.py -v
"""

import os

import pytest
import pytest_asyncio

from muninn.client import MuninnClient
from muninn.errors import MuninnAuthError, MuninnConnectionError, MuninnError

BASE_URL = os.environ.get("MUNINNDB_URL", "http://localhost:8476")
TOKEN = os.environ.get("MUNINNDB_TOKEN") or None
VAULT = "sdk-smoke-test"


@pytest.mark.asyncio
async def test_health():
    """Server must respond to /health with status=ok."""
    async with MuninnClient(BASE_URL, token=TOKEN) as client:
        ok = await client.health()
        assert ok, "health() returned False — server is not healthy"


@pytest.mark.asyncio
async def test_write_returns_valid_ulid():
    """write() must return a WriteResponse with a non-empty 26-character ULID id."""
    async with MuninnClient(BASE_URL, token=TOKEN) as client:
        resp = await client.write(
            vault=VAULT,
            concept="sdk-smoke-write",
            content="This memory was written by the SDK smoke test.",
        )
        assert resp.id, "write() returned an empty id"
        assert len(resp.id) == 26, (
            f"id should be a 26-char ULID, got {len(resp.id)}-char value: {resp.id!r}"
        )
        assert resp.id.isalnum(), (
            f"ULID should be alphanumeric (Base32 Crockford), got: {resp.id!r}"
        )
        assert isinstance(resp.created_at, int), (
            f"created_at should be an int, got: {type(resp.created_at).__name__}"
        )
        assert resp.created_at > 0, f"created_at should be a positive timestamp, got: {resp.created_at}"


@pytest.mark.asyncio
async def test_write_then_read_round_trip():
    """A written memory must be readable with correct id, concept, and content."""
    async with MuninnClient(BASE_URL, token=TOKEN) as client:
        write_resp = await client.write(
            vault=VAULT,
            concept="sdk-smoke-read-rt",
            content="Round-trip test content for SDK smoke.",
        )
        assert write_resp.id, "write() returned no id"

        read_resp = await client.read(write_resp.id, vault=VAULT)

        assert read_resp.id == write_resp.id, (
            f"read id mismatch: got {read_resp.id!r}, expected {write_resp.id!r}"
        )
        assert "Round-trip" in read_resp.content, (
            f"content mismatch — expected 'Round-trip' in content, got: {read_resp.content!r}"
        )
        assert read_resp.concept == "sdk-smoke-read-rt", (
            f"concept mismatch: got {read_resp.concept!r}"
        )
        # Structural sanity on the ReadResponse fields
        assert isinstance(read_resp.confidence, float), (
            f"confidence should be float, got {type(read_resp.confidence).__name__}"
        )
        assert isinstance(read_resp.tags, list), (
            f"tags should be a list, got {type(read_resp.tags).__name__}"
        )


@pytest.mark.asyncio
async def test_activate_returns_valid_response_structure():
    """activate() must return an ActivateResponse with required fields of correct types."""
    unique_concept = "sdk-smoke-activate-unique-xyzzy"
    async with MuninnClient(BASE_URL, token=TOKEN) as client:
        write_resp = await client.write(
            vault=VAULT,
            concept=unique_concept,
            content=f"Unique content for activate smoke test: {unique_concept}.",
        )
        assert write_resp.id, "write() returned no id before activate test"

        act_resp = await client.activate(
            vault=VAULT,
            context=[unique_concept],
            max_results=10,
            threshold=0.0,
        )

        # Response structure assertions — these catch garbage responses
        assert hasattr(act_resp, "query_id"), "ActivateResponse missing 'query_id' field"
        assert hasattr(act_resp, "total_found"), "ActivateResponse missing 'total_found' field"
        assert hasattr(act_resp, "activations"), "ActivateResponse missing 'activations' field"
        assert hasattr(act_resp, "latency_ms"), "ActivateResponse missing 'latency_ms' field"

        assert isinstance(act_resp.query_id, str), (
            f"query_id must be str, got {type(act_resp.query_id).__name__}"
        )
        assert isinstance(act_resp.total_found, int), (
            f"total_found must be int, got {type(act_resp.total_found).__name__}"
        )
        assert act_resp.total_found >= 0, (
            f"total_found must be non-negative, got {act_resp.total_found}"
        )
        assert isinstance(act_resp.activations, list), (
            f"activations must be a list, got {type(act_resp.activations).__name__}"
        )
        assert isinstance(act_resp.latency_ms, (int, float)), (
            f"latency_ms must be numeric, got {type(act_resp.latency_ms).__name__}"
        )

        # If activations are returned, each item must have the required fields
        for i, item in enumerate(act_resp.activations):
            assert item.id, f"activations[{i}].id is empty"
            assert isinstance(item.score, float), (
                f"activations[{i}].score must be float, got {type(item.score).__name__}"
            )
            assert isinstance(item.concept, str), (
                f"activations[{i}].concept must be str"
            )
            assert isinstance(item.content, str), (
                f"activations[{i}].content must be str"
            )


@pytest.mark.asyncio
async def test_no_auth_returns_error_or_gracefully_handles():
    """Requests with a deliberately invalid token must not crash the process.

    If auth is enforced: MuninnAuthError is raised — that is the correct behavior.
    If auth is not enforced (open server): the call may succeed — that is also OK.
    What is NOT OK: an unhandled exception of an unexpected type.
    """
    async with MuninnClient(BASE_URL, token="invalid-token-xyz-000") as client:
        try:
            await client.activate(
                vault="nonexistent-locked-vault",
                context=["test probe"],
            )
            # If we reach here the server is open (no auth required) — that is OK.
        except (MuninnAuthError, MuninnError, MuninnConnectionError):
            # Expected — auth enforcement or connection problem. Pass through.
            pass
        except Exception as exc:
            pytest.fail(
                f"Unexpected exception type when using invalid token: "
                f"{type(exc).__name__}: {exc}"
            )
