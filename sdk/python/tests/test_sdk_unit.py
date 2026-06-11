"""SDK unit tests using respx to mock HTTP — no live server required.

Run:
    pip install -e .[dev]
    pytest tests/test_sdk_unit.py -v
"""

from __future__ import annotations

import httpx
import pytest
import respx

from muninn.client import MuninnClient

BASE_URL = "http://muninn-mock"


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def mock_client() -> MuninnClient:
    """Create a MuninnClient whose HTTP will be intercepted by respx."""
    return MuninnClient(BASE_URL, token="test-token")


# ---------------------------------------------------------------------------
# write / batch
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
@respx.mock
async def test_write_returns_id():
    respx.post(f"{BASE_URL}/api/engrams").mock(
        return_value=httpx.Response(201, json={"id": "01ARZ3", "created_at": 1700000000})
    )
    async with mock_client() as c:
        resp = await c.write(vault="default", concept="test", content="body")
    assert resp.id == "01ARZ3"
    assert resp.created_at == 1700000000


@pytest.mark.asyncio
@respx.mock
async def test_write_batch_returns_results():
    respx.post(f"{BASE_URL}/api/engrams/batch").mock(
        return_value=httpx.Response(200, json={
            "results": [
                {"index": 0, "id": "id-1", "status": "created"},
                {"index": 1, "status": "duplicate"},  # no id — id is optional
            ]
        })
    )
    async with mock_client() as c:
        resp = await c.write_batch(
            vault="default",
            engrams=[
                {"concept": "a", "content": "aa"},
                {"concept": "b", "content": "bb"},
            ],
        )
    assert len(resp.results) == 2
    assert resp.results[0].id == "id-1"
    assert resp.results[1].id is None  # optional field absent


# ---------------------------------------------------------------------------
# read — coherence must be included (regression: was silently dropped)
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
@respx.mock
async def test_read_includes_coherence():
    """read() must pass coherence from the server response through to ReadResponse."""
    respx.get(f"{BASE_URL}/api/engrams/test-id").mock(
        return_value=httpx.Response(200, json={
            "id": "test-id",
            "concept": "coherent memory",
            "content": "body",
            "confidence": 0.8,
            "relevance": 0.7,
            "stability": 0.9,
            "access_count": 2,
            "tags": [],
            "state": "active",
            "created_at": 1700000000,
            "updated_at": 1700000001,
            "coherence": {
                "score": 0.95,
                "orphan_ratio": 0.01,
                "contradiction_density": 0.0,
                "duplication_pressure": 0.02,
                "temporal_variance": 0.1,
                "total_engrams": 10,
            },
        })
    )
    async with mock_client() as c:
        result = await c.read("test-id", vault="default")

    assert result.coherence is not None, "coherence must not be None"
    assert result.coherence["score"] == pytest.approx(0.95)


@pytest.mark.asyncio
@respx.mock
async def test_read_without_coherence():
    """read() without coherence field should have coherence=None."""
    respx.get(f"{BASE_URL}/api/engrams/no-coh").mock(
        return_value=httpx.Response(200, json={
            "id": "no-coh",
            "concept": "test",
            "content": "body",
            "confidence": 0.8,
            "relevance": 0.7,
            "stability": 0.9,
            "access_count": 0,
            "tags": [],
            "state": "active",
            "created_at": 1700000000,
            "updated_at": 1700000000,
        })
    )
    async with mock_client() as c:
        result = await c.read("no-coh", vault="default")

    assert result.coherence is None


# ---------------------------------------------------------------------------
# forget
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
@respx.mock
async def test_forget_soft_delete():
    route = respx.delete(f"{BASE_URL}/api/engrams/del-id").mock(
        return_value=httpx.Response(200, json={"ok": True})
    )
    async with mock_client() as c:
        ok = await c.forget("del-id", vault="default", hard=False)
    assert ok is True
    assert route.called


@pytest.mark.asyncio
@respx.mock
async def test_forget_hard_delete():
    route = respx.post(f"{BASE_URL}/api/engrams/hard-id/forget").mock(
        return_value=httpx.Response(200, json={"ok": True})
    )
    async with mock_client() as c:
        ok = await c.forget("hard-id", vault="default", hard=True)
    assert ok is True
    assert route.called


# ---------------------------------------------------------------------------
# subscribe — threshold=0.0 must be sent (regression: was silently dropped)
# ---------------------------------------------------------------------------

def test_subscribe_threshold_zero_is_sent():
    """subscribe() with threshold=0.0 must include threshold in the URL params."""
    client = MuninnClient(BASE_URL, token="tok")
    # We don't start the SSE stream — just inspect the SSEStream params.
    stream = client.subscribe(vault="default", push_on_write=True, threshold=0.0)
    params = stream._params  # type: ignore[attr-defined]
    assert "threshold" in params, "threshold=0.0 must not be dropped by falsy check"
    assert params["threshold"] == "0.0"


def test_subscribe_threshold_none_not_sent():
    """subscribe() without threshold should not include the threshold param."""
    client = MuninnClient(BASE_URL, token="tok")
    stream = client.subscribe(vault="default")
    params = stream._params  # type: ignore[attr-defined]
    assert "threshold" not in params


# ---------------------------------------------------------------------------
# activate
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
@respx.mock
async def test_activate_returns_activations():
    respx.post(f"{BASE_URL}/api/activate").mock(
        return_value=httpx.Response(200, json={
            "query_id": "q1",
            "total_found": 2,
            "activations": [
                {"id": "a1", "concept": "hit", "content": "body", "score": 0.9, "confidence": 0.8},
                {"id": "a2", "concept": "miss", "content": "body", "score": 0.3, "confidence": 0.5},
            ],
            "latency_ms": 12.5,
        })
    )
    async with mock_client() as c:
        resp = await c.activate(vault="default", context=["query"])
    assert resp.total_found == 2
    assert resp.activations[0].id == "a1"
    assert resp.latency_ms == pytest.approx(12.5)


# ---------------------------------------------------------------------------
# traverse — follow_entities must be forwarded
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
@respx.mock
async def test_traverse_follow_entities_sent():
    """When follow_entities=True, the request body must contain follow_entities."""
    captured_body: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        import json
        captured_body.update(json.loads(request.content))
        return httpx.Response(200, json={"nodes": [], "edges": [], "total_reachable": 0, "query_ms": 1.0})

    respx.post(f"{BASE_URL}/api/traverse").mock(side_effect=handler)

    async with mock_client() as c:
        await c.traverse(start_id="s1", follow_entities=True, vault="default")

    assert captured_body.get("follow_entities") is True


@pytest.mark.asyncio
@respx.mock
async def test_traverse_follow_entities_not_sent_when_false():
    """When follow_entities=False (default), it must NOT appear in the request body."""
    captured_body: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        import json
        captured_body.update(json.loads(request.content))
        return httpx.Response(200, json={"nodes": [], "edges": [], "total_reachable": 0, "query_ms": 1.0})

    respx.post(f"{BASE_URL}/api/traverse").mock(side_effect=handler)

    async with mock_client() as c:
        await c.traverse(start_id="s1", vault="default")

    assert "follow_entities" not in captured_body


# ---------------------------------------------------------------------------
# stats
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
@respx.mock
async def test_stats_returns_engram_count():
    respx.get(f"{BASE_URL}/api/stats").mock(
        return_value=httpx.Response(200, json={
            "engram_count": 42,
            "vault_count": 3,
            "storage_bytes": 102400,
        })
    )
    async with mock_client() as c:
        resp = await c.stats()
    assert resp.engram_count == 42
    assert resp.vault_count == 3
    assert resp.storage_bytes == 102400


# ---------------------------------------------------------------------------
# link
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
@respx.mock
async def test_link_sends_correct_body():
    captured_body: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        import json
        captured_body.update(json.loads(request.content))
        return httpx.Response(200, json={})

    respx.post(f"{BASE_URL}/api/link").mock(side_effect=handler)

    async with mock_client() as c:
        await c.link(
            source_id="src",
            target_id="tgt",
            vault="default",
            rel_type=5,
            weight=0.9,
        )

    assert captured_body["source_id"] == "src"
    assert captured_body["target_id"] == "tgt"
    assert captured_body["rel_type"] == 5
    assert captured_body["weight"] == pytest.approx(0.9)


# ---------------------------------------------------------------------------
# error handling
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
@respx.mock
async def test_not_found_raises_muninn_not_found():
    from muninn.errors import MuninnNotFound
    respx.get(f"{BASE_URL}/api/engrams/missing").mock(
        return_value=httpx.Response(404, json={"error": "not found"})
    )
    async with mock_client() as c:
        with pytest.raises(MuninnNotFound):
            await c.read("missing", vault="default")


@pytest.mark.asyncio
@respx.mock
async def test_auth_error_raises_muninn_auth_error():
    from muninn.errors import MuninnAuthError
    respx.get(f"{BASE_URL}/api/engrams/secret").mock(
        return_value=httpx.Response(401, json={"error": "unauthorized"})
    )
    async with mock_client() as c:
        with pytest.raises(MuninnAuthError):
            await c.read("secret", vault="default")


# ---------------------------------------------------------------------------
# SSE push parsing (issue #437) — nested engram + score/why fields
# ---------------------------------------------------------------------------

def test_subscribe_sends_push_on_write_param():
    """subscribe(push_on_write=True) must send the push_on_write query param.

    This is the param the server now accepts (alongside on_write) to fire pushes.
    """
    client = MuninnClient(BASE_URL, token="tok")
    stream = client.subscribe(vault="default", push_on_write=True)
    params = stream._params  # type: ignore[attr-defined]
    assert params.get("push_on_write") == "true"


@pytest.mark.asyncio
@respx.mock
async def test_subscribe_parses_nested_engram_push():
    """The SSE parser must read the nested engram object plus score/why that the
    server actually sends (issue #437), not the flat engram_id it used to look for.
    """
    sse_body = (
        "event: push\n"
        'data: {"subscription_id":"sub-1","trigger":"on_write","push_number":1,'
        '"score":0.31,"at":1700000000,'
        '"engram":{"id":"01ENGRAM","concept":"cats","content":"about cats"},'
        '"why":"matched context"}\n'
        "\n"
    )
    respx.get(f"{BASE_URL}/api/subscribe").mock(
        return_value=httpx.Response(
            200,
            headers={"Content-Type": "text/event-stream"},
            text=sse_body,
        )
    )

    async with mock_client() as c:
        stream = c.subscribe(vault="default", push_on_write=True)
        pushes = []
        async for push in stream:
            pushes.append(push)
            await stream.close()
            break

    assert len(pushes) == 1
    p = pushes[0]
    assert p.subscription_id == "sub-1"
    assert p.trigger == "on_write"
    assert p.push_number == 1
    assert p.score == pytest.approx(0.31)
    assert p.why == "matched context"
    assert p.engram is not None
    assert p.engram.id == "01ENGRAM"
    assert p.engram.concept == "cats"
    assert p.engram.content == "about cats"
    # engram_id is back-filled from the nested engram.id for backwards compat.
    assert p.engram_id == "01ENGRAM"
