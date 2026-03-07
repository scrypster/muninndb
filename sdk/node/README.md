# @muninndb/client

Official TypeScript / Node.js SDK for **MuninnDB** — the cognitive memory database.

## Features

- Full coverage of all 24 MuninnDB REST API operations
- Zero external dependencies — uses native `fetch` (Node.js 18+)
- Strict TypeScript types for every request and response
- Automatic retry with exponential backoff and jitter
- Server-Sent Events (SSE) streaming via `AsyncIterable`
- Structured error classes mapped to HTTP status codes

## Installation

```bash
npm install @muninndb/client
```

## Quick Start

```typescript
import { MuninnClient } from "@muninndb/client";

const client = new MuninnClient({
  token: "your-api-token",
  // baseUrl defaults to http://127.0.0.1:8476
});

// Write a memory
const { id } = await client.write({
  concept: "project-architecture",
  content: "The system uses a hub-spoke model with event-driven messaging.",
  tags: ["architecture", "design"],
});

// Semantic recall
const { activations } = await client.activate({
  context: ["How is the system architected?"],
  limit: 5,
});

for (const item of activations) {
  console.log(`${item.concept} (score: ${item.score})`);
}

// Clean up
client.close();
```

## Configuration

```typescript
const client = new MuninnClient({
  baseUrl: "http://127.0.0.1:8476", // MuninnDB server URL
  token: "your-api-token",         // Bearer token (required)
  timeout: 30_000,                 // Request timeout in ms (default: 30s)
  maxRetries: 3,                   // Retry attempts (default: 3)
  retryBackoff: 500,               // Base backoff delay in ms (default: 500)
  defaultVault: "default",         // Default vault name (default: "default")
});
```

## API Reference

### Core CRUD

| Method | Description |
|--------|-------------|
| `write(options)` | Write a single engram |
| `writeBatch(vault, engrams)` | Write up to 50 engrams in a batch |
| `read(id, vault?)` | Read an engram by ID |
| `forget(id, vault?)` | Soft-delete an engram |
| `activate(options)` | Semantic recall query |
| `link(options)` | Create an association between engrams |

### Extended Operations

| Method | Description |
|--------|-------------|
| `evolve(id, newContent, reason, vault?)` | Update an engram's content |
| `consolidate(options)` | Merge multiple engrams into one |
| `decide(options)` | Record a decision with rationale |
| `restore(id, vault?)` | Recover a soft-deleted engram |
| `traverse(options)` | Walk the association graph |
| `explain(options)` | Get a scoring breakdown |
| `setState(id, state, reason?, vault?)` | Transition lifecycle state |
| `listDeleted(vault?, limit?)` | List soft-deleted engrams |
| `retryEnrich(id, vault?)` | Re-queue enrichment plugins |
| `contradictions(vault?)` | List detected contradictions |
| `guide(vault?)` | Get the vault usage guide |

### Query & List

| Method | Description |
|--------|-------------|
| `stats(vault?)` | Get vault statistics |
| `listEngrams(vault?, limit?, offset?)` | List engrams with pagination |
| `getLinks(id, vault?)` | Get associations for an engram |
| `listVaults()` | List all vaults |
| `session(vault?, since?, limit?, offset?)` | Get session activity |

### Streaming & Health

| Method | Description |
|--------|-------------|
| `subscribe(vault?, pushOnWrite?)` | SSE subscription (`AsyncIterable<SseEvent>`) |
| `health()` | Health check |

### Lifecycle

| Method | Description |
|--------|-------------|
| `close()` | Abort all in-flight requests and subscriptions |

## Error Handling

The SDK throws typed errors that map to HTTP status codes:

```typescript
import {
  MuninnError,          // Base class for all errors
  MuninnAuthError,      // 401 Unauthorized
  MuninnNotFoundError,  // 404 Not Found
  MuninnConflictError,  // 409 Conflict
  MuninnServerError,    // 5xx Server Error
  MuninnConnectionError,// Network failures
  MuninnTimeoutError,   // Request timeout
} from "@muninndb/client";

try {
  await client.read("nonexistent-id");
} catch (err) {
  if (err instanceof MuninnNotFoundError) {
    console.log("Engram not found");
  } else if (err instanceof MuninnAuthError) {
    console.log("Invalid token");
  }
}
```

All error classes extend `MuninnError`, which provides:

- `message` — Human-readable error description
- `statusCode` — HTTP status code (when applicable)
- `body` — Raw response body from the server

## SSE Streaming

```typescript
const events = client.subscribe("my-vault");

for await (const event of events) {
  console.log(event.event, event.data);
}
```

The subscription remains open until you break out of the loop or call `client.close()`.

## TypeScript Types

All request/response types are exported and can be imported directly:

```typescript
import type {
  Engram,
  WriteOptions,
  ActivateOptions,
  ActivateResponse,
  TraverseResponse,
  // ... all other types
} from "@muninndb/client";
```

## Requirements

- Node.js >= 18.0.0
- TypeScript >= 5.0 (for development)

## License

MIT
