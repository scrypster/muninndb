# MuninnDB Python SDK

An async-first Python client for **MuninnDB**, a cognitive memory database with semantic search, graph traversal, and real-time subscriptions.

## Features

- **Async/await throughout** — Built on `httpx` for concurrent, non-blocking operations
- **Semantic memory activation** — Query memories by meaning, not keywords
- **Graph associations** — Link engrams and traverse relationships
- **Real-time subscriptions** — Server-Sent Events (SSE) with auto-reconnect
- **Automatic retry logic** — Exponential backoff with jitter for transient failures
- **Type-safe** — Full type hints for IDE support and runtime validation
- **Connection pooling** — Configurable keepalive and concurrent connections

## Installation

### Requirements
- Python 3.11+

### Setup

```bash
pip install -r requirements.txt
```

## Quick Start

```python
import asyncio
from muninn import MuninnClient

async def main():
    async with MuninnClient("http://127.0.0.1:8476") as client:
        # Write a memory
        engram_id = await client.write(
            vault="default",
            concept="neural plasticity",
            content="The brain's ability to reorganize neural connections.",
            tags=["neuroscience", "learning"]
        )
        print(f"Created: {engram_id}")

        # Activate memory (semantic search)
        results = await client.activate(
            vault="default",
            context=["how does learning work?"],
            max_results=10
        )

        for item in results.activations:
            print(f"[{item.score:.2f}] {item.concept}")

asyncio.run(main())
```

## API Reference

### Core Methods

#### `write(vault, concept, content, tags=None, confidence=0.9, stability=0.5) → str`

Write an engram (memory) to the database.

```python
engram_id = await client.write(
    vault="default",
    concept="example",
    content="Long-form content",
    tags=["tag1", "tag2"],
    confidence=0.95,
    stability=0.8
)
```

**Returns:** ULID string ID of created engram

---

#### `activate(vault, context, max_results=10, threshold=0.1, brief_mode="auto") → ActivateResponse`

Activate memory using semantic search and optional graph traversal.

```python
result = await client.activate(
    vault="default",
    context=["query", "terms"],
    max_results=10,
    threshold=0.1,
    brief_mode="extractive"  # "auto", "extractive", "abstractive"
)

for item in result.activations:
    print(f"Score: {item.score}, Concept: {item.concept}")

for sentence in result.brief or []:
    print(f"Brief: {sentence.text}")
```

**Returns:** `ActivateResponse` with:
- `query_id` — Query identifier
- `total_found` — Total matching engrams
- `activations` — List of `ActivationItem` (id, concept, content, score, confidence, why, hop_path, dormant)
- `latency_ms` — Query latency
- `brief` — Optional extractive/abstractive summary

---

#### `read(id, vault="default") → ReadResponse`

Read a specific engram by ID.

```python
engram = await client.read("01JM2345...", vault="default")
print(engram.concept, engram.confidence)
```

**Returns:** `ReadResponse` with full engram details

---

#### `forget(id, vault="default", hard=False) → bool`

Delete an engram (soft or hard).

```python
# Soft delete (recoverable)
await client.forget(engram_id, vault="default")

# Hard delete (permanent)
await client.forget(engram_id, vault="default", hard=True)
```

**Returns:** `True` on success

---

#### `link(source_id, target_id, vault="default", rel_type=5, weight=1.0) → bool`

Create an association between two engrams.

```python
await client.link(
    source_id="01JM...",
    target_id="01JM...",
    vault="default",
    rel_type=5,
    weight=0.9
)
```

**Returns:** `True` on success

---

#### `stats() → StatResponse`

Get database statistics and coherence metrics.

```python
stats = await client.stats()
print(f"Engrams: {stats.engram_count}")
print(f"Storage: {stats.storage_bytes} bytes")

if stats.coherence:
    for vault_name, coherence in stats.coherence.items():
        print(f"Vault {vault_name} coherence: {coherence.score:.2f}")
```

**Returns:** `StatResponse` with engram_count, vault_count, storage_bytes, and coherence dict

---

#### `subscribe(vault="default", push_on_write=True, threshold=0.0) → SSEStream`

Subscribe to real-time vault events via Server-Sent Events.

```python
stream = client.subscribe(vault="default", push_on_write=True)
async for push in stream:
    print(f"New engram: {push.engram_id}")
    if condition:
        await stream.close()
```

**Returns:** Async iterable yielding `Push` events with:
- `subscription_id` — Subscription ID
- `trigger` — Event type ("new_write", etc.)
- `push_number` — Push sequence number
- `engram_id` — ID of written engram
- `at` — Unix timestamp

---

#### `health() → bool`

Check if MuninnDB server is reachable and healthy.

```python
if await client.health():
    print("Server OK")
```

**Returns:** `True` if server responds with 200 OK

---

### Configuration

Create a client with custom settings:

```python
client = MuninnClient(
    base_url="http://127.0.0.1:8476",      # Server address
    token="your-bearer-token",              # Optional auth token
    timeout=5.0,                            # Request timeout (seconds)
    max_retries=3,                          # Max retry attempts
    retry_backoff=0.5,                      # Initial backoff multiplier
    max_connections=20,                     # Max concurrent connections
    keepalive_connections=10                # Max keepalive pool size
)
```

## Error Handling

The SDK raises semantic error types:

```python
from muninn import (
    MuninnError,                 # Base error
    MuninnAuthError,             # 401 Unauthorized
    MuninnNotFound,              # 404 Not Found
    MuninnConflict,              # 409 Conflict
    MuninnServerError,           # 5xx Server errors
    MuninnConnectionError,       # Network errors
    MuninnTimeoutError           # Request timeout
)

async with MuninnClient() as client:
    try:
        result = await client.activate(vault="default", context=["query"])
    except MuninnAuthError:
        print("Invalid token")
    except MuninnNotFound:
        print("Vault not found")
    except MuninnConnectionError:
        print("Network error - will retry automatically")
    except MuninnError as e:
        print(f"Error: {e.status_code} - {e}")
```

## Examples

### Example 1: Write and Activate

```bash
python examples/write_activate.py
```

Writes multiple neuroscience engrams and activates them with a query.

### Example 2: Real-Time Subscriptions

```bash
python examples/subscribe.py
```

Subscribes to a vault and writes an engram, demonstrating SSE push events.

### Example 3: Cognitive Loop

```bash
python examples/cognitive_loop.py
```

Full workflow: write → activate → link → inspect coherence.

## Retry Logic

The client automatically retries transient failures:

- **Retried errors:** 502, 503, 504, network errors, timeouts
- **Not retried:** 4xx client errors
- **Backoff:** Exponential with jitter: `retry_backoff * (2^attempt) + random(0, 0.1)`
- **Max attempts:** Configured via `max_retries` (default: 3)

Example with custom retry settings:

```python
async with MuninnClient(
    base_url="http://127.0.0.1:8476",
    max_retries=5,
    retry_backoff=1.0
) as client:
    # This will retry up to 5 times with longer delays
    result = await client.activate(vault="default", context=["query"])
```

## Type System

Full type hints enable IDE autocomplete:

```python
from muninn import (
    MuninnClient,
    ActivateResponse,
    ActivationItem,
    ReadResponse,
    StatResponse,
    Push
)

async with MuninnClient() as client:
    result: ActivateResponse = await client.activate(vault="default", context=[])
    for item: ActivationItem in result.activations:
        print(item.score, item.concept)
```

## Performance Tips

1. **Reuse client:** Create one `MuninnClient` and reuse it for multiple operations
2. **Batch operations:** Use concurrent tasks for parallel writes/activations
3. **Connection pooling:** Adjust `max_connections` and `keepalive_connections` based on workload
4. **Timeout tuning:** Increase `timeout` for large activation queries

```python
import asyncio

async with MuninnClient() as client:
    # Parallel writes
    tasks = [
        client.write(vault="default", concept=f"item-{i}", content="...")
        for i in range(100)
    ]
    ids = await asyncio.gather(*tasks)
```

## License

MIT
