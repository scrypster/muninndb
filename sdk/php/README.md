# MuninnDB PHP SDK

Official PHP client for [MuninnDB](https://github.com/scrypster/muninndb) — the cognitive memory database.

Framework-agnostic. Works in WordPress, Laravel, CodeIgniter, or plain PHP scripts with zero framework dependencies.

## Features

- All 24 MuninnDB REST API operations
- Typed response objects with readonly properties (PHP 8.1+)
- Clean named-argument API
- Automatic retry with exponential backoff on 5xx / connection errors
- Server-Sent Events (SSE) streaming via iterable `SseStream`
- PSR-4 autoloading, Composer-ready
- Native `curl` only — no Guzzle, no framework coupling

## Requirements

- PHP 8.1+
- `ext-curl`
- `ext-json`

## Installation

```bash
composer require muninndb/client
```

## Quick Start

```php
use MuninnDB\MuninnClient;

$client = new MuninnClient(
    baseUrl: 'http://127.0.0.1:8476',
    token: 'your-api-token',
);

// Write a memory
$result = $client->write(
    content: 'PHP 8.1 introduced readonly properties and fibers.',
    concept: 'PHP 8.1 Features',
    tags: ['php', 'language'],
);
echo $result->id;

// Recall by context
$activated = $client->activate(context: ['What changed in PHP 8?']);
foreach ($activated->activations as $item) {
    echo "{$item->concept}: {$item->score}\n";
}

// Read back
$engram = $client->read($result->id);
echo $engram->content;
```

## Framework Examples

### WordPress

```php
// In your plugin file
require_once __DIR__ . '/vendor/autoload.php';
use MuninnDB\MuninnClient;

$client = new MuninnClient(token: get_option('muninndb_token'));
$result = $client->write(content: get_the_content(), concept: get_the_title());
update_post_meta(get_the_ID(), '_muninn_id', $result->id);
```

### Laravel

```php
// In a service provider, controller, or anywhere
$client = new \MuninnDB\MuninnClient(
    baseUrl: config('muninndb.url'),
    token: config('muninndb.token'),
);

// In a controller
public function search(Request $request)
{
    $result = $this->client->activate(
        context: [$request->input('query')],
        limit: 20,
    );
    return response()->json($result->activations);
}
```

### CodeIgniter

```php
// In a controller or library
$client = new \MuninnDB\MuninnClient(
    baseUrl: config('MuninnDB')->url,
    token: config('MuninnDB')->token,
);

$stats = $client->stats();
echo "Total engrams: {$stats->totalEngrams}";
```

### Vanilla PHP

```php
require_once __DIR__ . '/vendor/autoload.php';

$client = new \MuninnDB\MuninnClient(token: 'my-secret-token');

$client->write(
    content: 'Meeting notes from Monday standup.',
    concept: 'Standup Notes',
    vault: 'work',
    tags: ['meetings', 'standup'],
);
```

## Full API Reference

### Constructor

```php
$client = new MuninnClient(
    baseUrl: 'http://127.0.0.1:8476', // Server URL
    token: '',                        // Bearer token
    timeout: 5.0,                     // Request timeout in seconds
    maxRetries: 3,                    // Retry count on 5xx / connection errors
    retryBackoff: 0.5,               // Base backoff delay (doubles each retry)
);
```

### Core CRUD

#### `write(...)` — Write a single engram

```php
$response = $client->write(
    content: 'The actual memory content.',
    concept: 'A short label',
    vault: 'default',
    tags: ['tag1', 'tag2'],
    confidence: 0.8,
    stability: 0.5,
    memoryType: 'episodic',
    typeLabel: 'note',
    summary: 'Optional summary.',
    entities: ['entity1'],
    relationships: [['source' => 'id1', 'target' => 'id2', 'rel_type' => 'related']],
);
// Returns: WriteResponse { id, createdAt, hint }
```

#### `writeBatch(...)` — Write up to 50 engrams

```php
$response = $client->writeBatch([
    ['content' => 'First memory', 'concept' => 'Concept A'],
    ['content' => 'Second memory', 'concept' => 'Concept B'],
]);
// Returns: BatchWriteResponse { results: BatchWriteResult[] }
```

#### `read(id, vault)` — Read a single engram

```php
$engram = $client->read('engram-id-here');
// Returns: Engram { id, vault, concept, content, tags, confidence, ... }
```

#### `forget(id, vault, hard)` — Soft or hard delete

```php
$client->forget('engram-id', hard: true);
```

#### `activate(...)` — Recall by context

```php
$response = $client->activate(
    context: ['search terms', 'or phrases'],
    vault: 'default',
    threshold: 0.3,
    limit: 10,
    maxHops: 2,
    profile: 'creative',
    includeWhy: true,
    briefMode: true,
);
// Returns: ActivateResponse { queryId, totalFound, activations, latencyMs, brief }
```

#### `link(...)` — Associate two engrams

```php
$client->link(
    sourceId: 'id-a',
    targetId: 'id-b',
    relType: 'supports',
    weight: 0.9,
);
```

### Extended Operations

#### `evolve(id, newContent, reason, vault)` — Update content

```php
$response = $client->evolve('engram-id', 'Updated content here.', 'Corrected typo');
```

#### `consolidate(ids, mergedContent, vault)` — Merge engrams

```php
$response = $client->consolidate(
    ids: ['id-1', 'id-2', 'id-3'],
    mergedContent: 'Combined knowledge from all three.',
);
// Returns: ConsolidateResponse { id, archived, warnings }
```

#### `decide(decision, rationale, alternatives, evidenceIds, vault)` — Record a decision

```php
$response = $client->decide(
    decision: 'Use PostgreSQL for the project',
    rationale: 'Better JSON support and mature ecosystem',
    alternatives: ['MySQL', 'SQLite'],
    evidenceIds: ['research-id-1', 'benchmark-id-2'],
);
```

#### `restore(id, vault)` — Recover a deleted engram

```php
$response = $client->restore('deleted-engram-id');
// Returns: RestoreResponse { id, concept, restored, state }
```

#### `traverse(startId, maxHops, maxNodes, relTypes, vault)` — Graph walk

```php
$response = $client->traverse(
    startId: 'root-engram-id',
    maxHops: 3,
    maxNodes: 50,
    relTypes: ['supports', 'contradicts'],
);
// Returns: TraverseResponse { nodes, edges, totalReachable, queryMs }
```

#### `explain(engramId, query, vault)` — Score breakdown

```php
$response = $client->explain('engram-id', query: ['why this result?']);
// Returns: ExplainResponse { engramId, finalScore, components, profile }
```

#### `setState(id, state, reason, vault)` — Change workflow state

Valid states: `planning`, `active`, `paused`, `blocked`, `completed`, `cancelled`, `archived`

```php
$response = $client->setState('engram-id', 'completed', reason: 'Task finished');
```

#### `listDeleted(vault, limit)` — List soft-deleted engrams

```php
$response = $client->listDeleted(limit: 50);
// Returns: ListDeletedResponse { engrams: DeletedEngram[] }
```

#### `retryEnrich(id, vault)` — Re-run enrichment

```php
$response = $client->retryEnrich('engram-id');
```

#### `contradictions(vault)` — Detected contradictions

```php
$response = $client->contradictions();
// Returns: ContradictionsResponse { contradictions: ContradictionItem[] }
```

#### `guide(vault)` — Human-readable vault guide

```php
$text = $client->guide();
```

### Query & List

#### `stats(vault)` — Vault statistics

```php
$stats = $client->stats();
echo "Engrams: {$stats->totalEngrams}, Links: {$stats->totalLinks}";
```

#### `listEngrams(vault, limit, offset)` — Paginated engram list

```php
$response = $client->listEngrams(limit: 50, offset: 100);
foreach ($response->engrams as $item) {
    echo "{$item->id}: {$item->concept}\n";
}
```

#### `getLinks(id, vault)` — Get an engram's associations

```php
$links = $client->getLinks('engram-id');
// Returns: AssociationItem[]
```

#### `listVaults()` — List all vaults

```php
$vaults = $client->listVaults();
// Returns: string[]
```

#### `session(vault, since, limit, offset)` — Session activity log

```php
$response = $client->session(since: '2025-01-01T00:00:00Z');
foreach ($response->entries as $entry) {
    echo "{$entry->action}: {$entry->concept}\n";
}
```

### Streaming & Health

#### `subscribe(vault, pushOnWrite)` — SSE subscription

```php
foreach ($client->subscribe('default') as $event) {
    echo "Event: {$event->event}, Engram: {$event->engramId}\n";
    // Break when you've had enough
}
```

#### `health()` — Health check

```php
$h = $client->health();
echo $h->status; // "ok"
```

## Error Handling

All errors throw exceptions extending `MuninnDB\Exceptions\MuninnException`:

| Exception | HTTP Status | When |
|---|---|---|
| `AuthException` | 401 | Invalid or missing token |
| `NotFoundException` | 404 | Engram / resource not found |
| `ValidationException` | 400, 422 | Invalid request body |
| `ConflictException` | 409 | Conflicting operation |
| `ServerException` | 5xx | Server-side error (after retries) |
| `ConnectionException` | — | curl connection failure |
| `TimeoutException` | — | Request exceeded timeout |

```php
use MuninnDB\Exceptions\NotFoundException;
use MuninnDB\Exceptions\AuthException;

try {
    $engram = $client->read('nonexistent-id');
} catch (NotFoundException $e) {
    echo "Not found: {$e->getMessage()}";
} catch (AuthException $e) {
    echo "Auth failed — check your token";
}
```

## Configuration

All configuration is done through the constructor — no config files, no environment variable magic:

```php
$client = new MuninnClient(
    baseUrl: getenv('MUNINN_URL') ?: 'http://127.0.0.1:8476',
    token: getenv('MUNINN_TOKEN') ?: '',
    timeout: 10.0,
    maxRetries: 5,
    retryBackoff: 1.0,
);
```

## License

MIT
