# Semantic Triggers

> Push-based memory for AI agents. The database tells you when something matters.

---

## 1. The Problem With Pull

Every database primitive we have is pull-based. You query. You get results. You query again. You compare. You decide if anything changed.

This is fine for most use cases. It is fundamentally wrong for memory.

Think about how human attention works. You don't periodically poll your own brain for relevant memories. Your brain surfaces them — triggered by context, by time, by emotional salience, by contradiction. When you're deep in a debugging session and something about a Q3 incident suddenly becomes relevant, that memory pushes itself to your attention. You didn't ask.

AI agents built on pull-based databases have to simulate this push behavior. They poll at fixed intervals, make retrieval calls at every step, compare current results against previous results, and guess at what changed. They miss relevance shifts that happen between queries. They don't know when something they thought was settled has been contradicted. They don't know when a memory written last week just became critical to what they're doing right now.

The polling pattern is not just inefficient. It is architecturally wrong. The burden of knowing what's relevant falls on the agent, not the system that stores the knowledge. That's backwards.

---

## 2. What Semantic Triggers Are

A semantic trigger is a continuous subscription: *watch this context; tell me when anything becomes relevant above this threshold.*

You define the context you care about. You define the relevance level that matters. MuninnDB does the rest — continuously, automatically, from the storage layer. You don't poll. You don't compare. You don't guess.

Three things can fire a trigger:

**`new_write`** — A new engram is written to the vault and its activation score against your subscription context exceeds your threshold. You find out immediately — before your next query cycle, before your next LLM call.

**`threshold_crossed`** — An existing engram's relevance score changes and crosses your threshold. Time passed and a memory stabilized. A co-activation strengthened a Hebbian connection. The temporal model shifted relevance back into range. The database found something for you that you didn't know was there.

**`contradiction_detected`** — An engram within your activation set has a contradiction detected. This is highest priority. No rate limiting. Immediate delivery. You need to know that your agent is working with contradictory information.

---

## 3. Why This Is a New Primitive

No other database does this. The claim is worth defending precisely.

**Relational databases** have `LISTEN/NOTIFY` (Postgres) and row-level triggers. These are data mutation events — they fire on `INSERT`, `UPDATE`, `DELETE`. They tell you a row changed. They say nothing about whether that row is relevant to anything you care about. A Postgres trigger cannot tell you "this fact just became semantically related to your task context." It can tell you a row was updated.

**Redis pub/sub** fires on explicit `PUBLISH` operations. A publisher has to decide to emit an event. Relevance score changes in a cognitive model are not explicit publish operations — they're continuous internal state changes. Redis has no model of relevance.

**Vector databases** have no push mechanism at all. They are pure query systems. You ask, they answer. The idea of a vector database proactively notifying you when a stored vector becomes relevant to a context you declared interest in — this does not exist in any vector database today.

**Graph databases** have adjacency and traversal. They have no continuous relevance scoring model that changes over time. They cannot detect when a node's relevance to a given context crosses a threshold.

The key distinction: semantic triggers fire based on *meaning and relevance*, not on data mutations. The trigger condition is evaluated inside the cognitive model — against activation scores, temporal priority, Hebbian weights, contradiction signals — not at the storage layer. The trigger system understands what the data means in context. Storage-layer triggers understand only that data changed.

This is what makes it novel. It's not a faster version of something that already exists. It's a different thing.

---

## 4. How the Trigger System Works

### The TriggerWorker

One shared worker handles all subscriptions. Not one goroutine per subscription — that would be O(N) goroutines for N subscriptions, which breaks down at scale. The TriggerWorker maintains a shared event bus and processes events from four sources.

### Event Sources

**Write events** — After every successful write ACK, the trigger system receives a notification. It scores the new engram against all active subscriptions. This is O(S) where S is the subscription count — linear, bounded, predictable. High-write vaults with many subscriptions are the design load, not the edge case.

**Cognitive events** — The Hebbian worker and confidence worker emit events when a score delta exceeds `NegligibleDelta`. Small score changes are filtered out. Only meaningful threshold crossings propagate into the event bus. This is the mechanism that fires `threshold_crossed` — an existing engram drifts into relevance because the cognitive model updated.

**Contradiction events** — Fired immediately on contradiction detection. Highest priority queue position. No rate limiting — contradictions are urgent and rare. An agent working with contradictory information needs to know now.

**Periodic sweep** — Every 30 seconds, all subscriptions are re-scored against the current vault state. This is the backstop. It catches anything the event stream might have missed and ensures that slow-accumulating relevance changes are eventually surfaced.

### Scoring a Subscription

Each subscription has one or more context strings and a threshold between 0.0 and 1.0. Scoring means running ACTIVATE against the subscription's context and checking whether the engram in question appears above threshold. If it does, delivery is triggered.

When the Embed plugin is active, subscription context strings are embedded to vectors. The cache stores embeddings keyed by SHA256 hash of the context string. Subscriptions with identical context strings share a single embedding — no redundant model calls for duplicate contexts.

### Rate Limiting

Token bucket per subscription. Prevents a single high-activity vault from flooding a subscriber. The bucket refills at a configured rate. When empty, non-critical events are held or dropped.

Contradiction events bypass the rate limiter entirely. You always get contradiction notifications.

### Delivery

Non-blocking async channel per subscriber. If the subscriber's channel is full because the consumer is processing too slowly, the notification is dropped. A metric is incremented. This is intentional — a subscriber that cannot keep up must not slow down the rest of the system. The periodic sweep provides a backstop: even dropped events will be caught at the next 30-second pass.

---

## 5. Using Triggers

### Basic Subscribe (Go SDK)

```go
client := muninn.NewClient("http://127.0.0.1:8475", token)
pushCh, err := client.Subscribe(ctx, "my-project")
if err != nil {
    log.Fatal(err)
}

go func() {
    for push := range pushCh {
        fmt.Printf("[MEMORY] Trigger: %s, Score: %.2f\n", push.Trigger, push.Score)
        if push.Engram != nil {
            fmt.Printf("[ENGRAM] %s: %s\n", push.Engram.Concept, push.Engram.Content)
        }
    }
}()
```

The `Subscribe` call opens a long-lived SSE (Server-Sent Events) connection via GET /api/subscribe. Delivery is push-based: the database sends events as they occur. The channel closes when the context is cancelled or the connection fails.

### Subscribe with Context and Threshold

```go
// The Subscribe method connects to GET /api/subscribe with query parameters
// to define semantic matching conditions
q := url.Values{}
q.Set("vault", "my-project")
q.Set("context", "database architecture")
q.Set("threshold", "0.8")
q.Set("push_on_write", "true")

url := "http://127.0.0.1:8475/api/subscribe?" + q.Encode()
// Then use client.Subscribe(ctx, vault) which encapsulates this

pushCh, err := client.Subscribe(ctx, "my-project")
if err != nil {
    log.Fatal(err)
}

// Process pushes as they arrive
for push := range pushCh {
    if push.Engram != nil && push.Score > 0.8 {
        fmt.Printf("[MATCH] %s (score: %.2f)\n", push.Engram.Concept, push.Score)
    }
}
```

Multiple contexts are passed via repeated query params (context=value1&context=value2). The subscription fires when any context matches above the threshold.

---

## 6. The Agent Memory Pattern

The canonical integration for AI agents looks like this:

```go
func RunAgent(ctx context.Context, task string) error {
    client := muninn.NewClient("http://127.0.0.1:8475", token)

    // Subscribe at session start
    // The DB will push memories as they become relevant — no polling
    pushCh, err := client.Subscribe(ctx, "agent-session")
    if err != nil {
        return err
    }

    // Process pushes in a background goroutine
    go func() {
        for push := range pushCh {
            if push.Engram == nil {
                continue
            }
            // Inject into the agent's context window before next LLM call
            agent.InjectContext(push.Engram.Content)

            // Contradictions (push.Trigger == "contradiction_detected") get highest priority
            if push.Trigger == "contradiction_detected" {
                agent.InjectWarning("Contradiction detected: " + push.Why)
            }
        }
    }()

    // Agent works; the DB handles what's relevant and pushes to the channel
    return agent.Run(ctx, task)
}
```

### Python SDK Example

```python
import asyncio
from muninn.client import MuninnClient

async def run_agent(task: str):
    async with MuninnClient("http://127.0.0.1:8475", token=token) as client:
        # Subscribe to semantic triggers via SSE
        stream = client.subscribe(vault="agent-session", push_on_write=True, threshold=0.7)

        # Process pushes in a background task
        async def handle_pushes():
            async for push in stream:
                if push.engram:
                    # Inject into agent's context window
                    agent.inject_context(push.engram.content)
                    if push.trigger == "contradiction_detected":
                        agent.inject_warning(f"Contradiction: {push.why}")

        # Start push handler
        handler = asyncio.create_task(handle_pushes())

        try:
            # Agent works while DB pushes relevant memories
            await agent.run(task)
        finally:
            await stream.close()
            handler.cancel()
```

The session flow:

1. **Session start** — Open a Subscribe connection for the agent's task context. One call establishes the SSE stream.

2. **As the agent works** — Memories relevant to the task are pushed automatically to the stream. The agent doesn't poll. It receives.

3. **Before each LLM call** — The agent's context window already contains the most relevant memories. No explicit retrieval query needed at each step.

4. **The DB handles** — what's relevant now (temporal scoring), what connects (Hebbian associations), what's uncertain (confidence weighting), what's contradictory (contradiction detection and notification).

5. **The agent handles** — using pushed memories to make better decisions.

### This Is Not RAG

Retrieval-Augmented Generation is pull: the agent queries before it generates. It decides what to retrieve, when to retrieve it, and how much to retrieve. The retrieval step is explicit, synchronous, and agent-managed.

Semantic triggers are push: the database decides when something is relevant and sends it. The agent's context window accumulates relevant memories automatically across the session. Retrieval is continuous, async, and database-managed.

These are not competing approaches for the same problem. RAG is the right pattern for "I need specific information right now." Semantic triggers are the right pattern for "I want the database to surface what matters as the session evolves."

In practice, most agent architectures benefit from both: semantic triggers for continuous context accumulation, explicit ACTIVATE calls when the agent needs to deliberately search.

---

## 7. Operational Notes

**Subscription limits** — No hard limit per vault, but each active subscription adds O(1) work per write event. Monitor `trigger_subscriptions_active` in the metrics endpoint.

**Threshold calibration** — Start at 0.7. Lower thresholds mean more events (higher recall, lower precision). Higher thresholds mean fewer events (lower recall, higher precision). The periodic sweep catches anything the event stream misses, so a threshold that's slightly too high doesn't create permanent blind spots — just 30-second lag at most.

**Subscription lifetime** — Tied to the context lifetime. When the context is cancelled, the subscription is removed. Leaked contexts leak subscriptions. Use `defer cancel()`.

**Contradiction rate limiting** — Contradiction events bypass the token bucket. If a vault has many contradictions firing simultaneously (e.g., after a large batch write), each subscribed context receives all of them. Design contradiction handlers to be fast or to queue internally.

**Metrics** — The trigger system exposes:
- `trigger_events_total` — by event type and vault
- `trigger_deliveries_total` — successful deliveries
- `trigger_drops_total` — dropped events (consumer too slow)
- `trigger_subscriptions_active` — current active count
- `trigger_sweep_duration_seconds` — periodic sweep timing
