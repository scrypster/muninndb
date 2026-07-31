# Measuring an embedding model's noise baseline

The semantic-abstention floor (COG-26) rescales cosine by a per-model constant
`b`, held in `noiseBaselineRegistry` (`internal/plugin/embed/baseline.go`). `b`
is the point above which a cosine is evidence of relevance rather than an
artefact of the model's anisotropy.

This document is the procedure for deriving `b` for a model, so the constant is
a reproducible property of the model rather than a measurement of whichever
corpus happened to be at hand. Re-derive; do not hand-edit.

## Why a canonical corpus

A baseline derived from a deployment's own vault characterizes that vault's
subject matter as much as the model. Two consequences:

- It cannot be reproduced by anyone else, so it cannot be reviewed — only trusted.
- It travels: a constant measured on one corpus ships to every deployment,
  including those whose text looks nothing like it.

`internal/plugin/embed/anisotropy_corpus.jsonl` is the fixed alternative: 160
short general-knowledge statements across 20 unrelated domains, 8 per domain,
carrying no proper nouns tied to any organization. It ships in the repo, so a
change to a constant arrives as a diff alongside the corpus that produced it.

## The statistic

For corpus entries `e_i` with embeddings `v_i`:

1. L2-normalize every `v_i`. (Cosine is scale-invariant, so this also makes the
   result independent of whether the backend returns unit vectors — Ollama's
   `/api/embed` and `/api/embeddings` differ on exactly this for the same model
   and input.)
2. Take the cosine of every pair `(i, j)`, `i < j`, **where `domain(i) != domain(j)`**.
3. `b = μ + kσ` over that population.

`AnisotropyBaseline` in `internal/plugin/embed/anisotropy.go` implements this.

### Same-domain pairs are excluded, and that is not a detail

A corpus of *N* sentences over *D* domains does not supply *N(N−1)/2* unrelated
pairs. Within-domain pairs are related by construction and sit measurably
higher, so including them inflates `b` and makes recall abstain more than the
operator asked for. The contamination scales with the square of per-domain
depth: at 8 sentences per domain it is 4.4% of pairs; at 20 it is 9.5%.

This is why the corpus format carries a `domain` label per line. A corpus
shipped as bare text cannot be calibrated correctly, only approximately.

## k is a policy choice, not a measurement

`μ` and `σ` are properties of the model. `k` is not — it is the only part of
`μ+kσ` that encodes a product decision, and it trades false-admits against
false-abstains. It therefore belongs in one documented place with a per-vault
override (`SemanticFloor`), never as a per-model value, which would hide a
policy choice inside a measurement.

To choose or defend a `k`, report what it excludes on the corpus, e.g.:

| k | share of unrelated pairs at or above the floor |
|---|---|
| 1.0 | ~16% |
| 1.3 | ~10% |
| 2.0 | ~3% |
| 3.0 | ~0.3% |

(Indicative figures from one 1024-dim model; recompute per model.)

## What a registry entry must record

`b` alone is not reproducible. Two callers can both honestly say
`bge-small-en-v1.5` and be entitled to different constants: pooling (CLS vs
mean), whether an instruction prefix is prepended (bge v1.5 uses one for
queries; bge-m3 does not), truncation, and quantization all move the
distribution.

So a characterization should carry: model identifier, backend/runtime, pooling,
prefix convention, dimension, corpus size and pair count, `μ`, `σ`, `k`, the
resulting `b`, and a hash of the corpus used. `AnisotropyResult` holds the
numeric part of that.

## Protocol divergence to be aware of

The shipped `bge-small-en-v1.5` value (`b = 0.520`, μ=0.450 σ=0.054) was derived
from **432 nonsense-query × real-passage pairs** — a query→passage distribution.
The procedure above measures **random cross-domain sentence pairs**. Both
estimate "cosine between unrelated text", but they are different populations and
their numbers are not interchangeable.

Whichever is adopted, it should be adopted for every model in the registry;
mixing them makes the entries incomparable to each other, which defeats the
purpose of having a registry.
