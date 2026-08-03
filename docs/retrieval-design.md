# MuninnDB Retrieval Design

## What Gets Returned (and Why It Matters)

MuninnDB doesn't return a single memory. It returns a **ranked list** — typically the top 3-5 memories that are most relevant to your query right now. This is intentional.

A single note is rarely the full picture. The note you wrote last week about Redis caching is most relevant, but the note from three months ago explaining *why* you chose Redis over Memcached provides supporting context. Both belong in the response.

---

## How the Score Works

Every result has a final activation score:

```
final = ContentMatch × softplus(BaseLevel + HebbianBoost + TransitionBoost) × Confidence
```

**ContentMatch** — how well the note's text matches the query:
```
ContentMatch = 0.6 × vectorScore + 0.4 × normalizedFTS
```
- Vector score: cosine similarity via HNSW approximate nearest neighbors
- FTS score: BM25 keyword match via full-text index
- If ContentMatch = 0 (note not retrieved by either index), ACT-R score = 0

**BaseLevel (ACT-R)** — the cognitive weight of the memory:
```
B(M) = ln(n+1) - 0.5 × ln(ageDays / (n+1))
```
Where `n` = access count and `ageDays` = time since last access.

A fresh note (accessed 10 days ago, 13 times): softplus ≈ 2.6
A stale note (accessed 1400 days ago, 1 time): softplus ≈ 0.07
Ratio: **~37x temporal advantage** for the fresh note.

**HebbianBoost** — associative reinforcement. Notes frequently co-accessed with related notes receive a boost. This surfaces memory *clusters* — not just the single most relevant note but the constellation of notes that tend to come up together.

**TransitionBoost** — predictive activation. When PAS is enabled, notes that historically follow the previous activation's results receive a boost. This surfaces procedurally connected memories — the next step in a workflow, the follow-up to a question, the resolution to a complaint. PAS injects candidates that may have zero semantic similarity to the query but are the learned "next thing" in the sequence.

---

## Retrieval Modes

The `Weights` field on `ActivateRequest` controls the scoring mix. Pre-built modes:

| Mode | Temporal | Semantic | Hebbian | When to Use |
|------|----------|----------|---------|-------------|
| `balanced` (default) | ACT-R | HNSW + FTS | yes | General memory retrieval |
| `semantic` | off | HNSW + FTS | off | "What do I know about X?" regardless of age |
| `recent` | temporal-dominant | reduced | off | "What am I actively working on?" |

The AI consumer (an LLM calling the activation API) can specify mode directly. This means the model can adapt retrieval strategy to the question being asked:

- User asks "what was my decision about the auth system?" → `balanced`, k=3 (surface recent decision)
- User asks "summarize everything you know about distributed systems" → `semantic`, k=10
- User asks "what have I been thinking about lately?" → `recent`, k=5

---

## Recommended `k` Values

| Use Case | k | Rationale |
|----------|---|-----------|
| Single-turn AI response augmentation | 3 | Enough context, doesn't blow context window |
| Building a response about a complex topic | 5 | First result + supporting cluster |
| Full knowledge dump for a domain | 10 | Comprehensive, use with `semantic` mode |
| "What am I working on?" | 3 | Recency dominates, more isn't better |

**Default recommendation: k=3, mode=balanced.**

This is the sweet spot for AI memory injection. The top result is the primary memory; positions 2-3 provide context and supporting detail. Position 4+ is usually diminishing returns for a focused question.

---

## Needle in the Haystack

The eval results confirm retrieval quality:

- **d2 NDCG@10 = 0.56** — with a paraphrased query (different words, same concept), the right note typically lands in the top 5. No keyword match required.
- **Disambiguation win rate = 80-100%** — when multiple notes match a query, the recently-accessed one surfaces above the stale one 8-10 times out of 10.
- **Temporal lift avg delta = +11.2 rank positions** — the cognitive advantage of a fresh note over a stale semantic twin is substantial, not marginal.

The practical implication: you don't need to craft precise queries. Natural-language questions work. "How do I handle rate limiting?" finds your note on API rate limiting strategies even if the note never uses the word "rate limiting" — because ACT-R ensures the note you *recently thought about* scores high enough to clear the ContentMatch gate and then temporal priority carries it.

---

## What a Flat Vector Database Can't Do

A standard vector database returns the *k most similar embeddings*. It has no concept of recency, access patterns, or associative reinforcement.

Ask it "how do I handle caching?" in a vault with 20 caching-related notes — it returns all 20 equally likely candidates, weighted only by embedding similarity. The decision you made last week ranks the same as the decision you made three years ago and later abandoned.

MuninnDB returns the 20-year-old note you've been actively revisiting and annotating over the past week, ranked above the historically similar but now-irrelevant note from three years ago. The ranking tracks your cognitive relationship with the material, not just the material itself.

That's the core property. The eval proves it works.

---

## API Surface (Current)

```go
type ActivateRequest struct {
    Context    []string   // query strings
    MaxResults int        // k — number of results
    Vault      string     // namespace isolation
    Weights    *Weights   // scoring mix (nil = balanced/actr default)
    BriefMode  string     // "off" | "on" — truncate content in results
}

type ActivationItem struct {
    ID         string
    Concept    string
    Content    string
    Score      float64
    Tags       []string
}
```

The `Weights` struct controls the temporal/semantic/hebbian balance. ACT-R is the only production scoring path. CGDN is nominally available as an experimental alternative when `experimental_cgdn: true` is set in the vault's plasticity config, but it is currently inert at any positive activation threshold — see `docs/internals/decision-record.md` (#768) — so enabling it returns nothing on the surface default threshold.

---

## Future: Declarative Mode Shortcuts

Planned addition — let AI consumers specify intent directly without constructing weights:

```go
// Shorthand modes the AI can specify in plain language
Mode: "recent"    // temporal-dominant, k=3 default
Mode: "semantic"  // embedding-only, k=5 default
Mode: "balanced"  // current ACT-R default
Mode: "deep"      // semantic + all tiers, k=10 default
```

This allows the model to adapt retrieval strategy to the question type without needing
to understand the weight system.

---

**See also:** [Architecture](architecture.md) · [Cognitive Primitives](cognitive-primitives.md) · [Feature Reference](feature-reference.md)
