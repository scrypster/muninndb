# RFC (draft) — Supersedes-aware recall

Status: Increment 1 (ranking) IMPLEMENTED on branch feat/supersedes-aware-recall (commit
1687362) — Fable's "pair-promotion" refinement of Option A; RED-first, -race clean, proven
end-to-end on labs (current runway now leads, stale sits one epsilon below, no annotate flag).
NOT merged (held per standing branch/labs-only constraint). Increment 2 (always-on
superseded_by/current_version/supersedes annotation across transports) not yet built.
The #1 finding of the Fable+Opus sentient-feel passes (see
2026-07-28-sentient-feel-findings.md §SYNTHESIS). Decision below was made by Fable
(deep-reasoning pass): Option A sharpened — soft-demote + inject the head + always-annotate.

## Problem (proven on labs)
Recall leads with facts it *knows* are superseded. Query "current runway" on the seeded vault:

    default (no annotate):   [1.15] "Runway was 8 months in May"        <- stale, leads
                             [0.92] "Bridge raise extended runway to 11 months"
    annotate:true:           [1.15] "Runway was 8 months in May"  superseded_by=01KX5YC7…  <- STILL #1
                             [0.92] "Bridge raise extended runway to 11 months"

The superseded fact outranks its replacement. For evolving facts this is worse than grep —
the project's named worst failure class (silently-wrong recall).

## Why it happens (scoped, mechanism confirmed)
- The supersession signal ALREADY EXISTS and is correct: `engine.GetAnnotations`
  (annotation.go) uses `store.GetReverseAssociations` to find "who supersedes me" and returns
  `SupersededBy`. MCP already carries `superseded_by` in recall output (mcp/types.go:100).
- But it is INERT on the default path:
  1. `handleRecall` only computes annotations when `args["annotate"]==true` (handlers.go:429)
     — OFF by default, so agents never see the signal.
  2. Annotations only DECORATE; supersession never enters ranking. The stale engram keeps its
     blended score and leads.
- NOTE: evolve() already soft-deletes the predecessor, so evolve-superseded facts are already
  excluded from recall. The gap is specifically facts superseded via a `supersedes` LINK
  (remember new + link, or manual link) where the predecessor stays `state=active`.

## Fix (reuse the existing reverse-edge lookup; no new storage)
Two parts, both cheap:
- **Always surface supersession** when it exists — not opt-in. If a recall candidate is the
  target of a `RelSupersedes` edge from an active engram, annotate `superseded_by` regardless
  of the `annotate` flag. "Degrade loudly, never silently-wrong."
- **Demote superseded candidates in ranking** so the current fact wins. The superseder should
  rank above the superseded; the superseded is still returned (never hidden), just below and
  flagged.

## THE DECISION (product-behavior; the user's call — gates the build)
How aggressive should supersession be at recall?
  A. **Soft-demote + always-annotate (RECOMMENDED).** Superseder ranks above superseded;
     superseded stays visible but flagged. Never hides a fact ("what did we used to think?"
     still works), always tells the truth about currency. Matches "never silently-wrong"
     without matching "silently-hidden."
  B. Annotate-only. Surface `superseded_by` by default but don't reorder. Weakest; the stale
     fact can still lead — only fixes the invisibility, not the wrong-answer.
  C. Suppress superseded from the default top-N (opt-in to include). Strongest currency signal
     but risks hiding a fact the user explicitly wants; cuts against "never hide data."
Recommendation: A.

## Where (transport-agnostic vs presentation)
- Engine-level (Activate ranking): transport-agnostic (REST/gRPC/MBP get it too), but touches
  COG ranking invariants — must not violate COG-5/6/11/12; RED-first + -race required.
- MCP-handler-level (reorder + annotate post-Activate): least invasive, but only MCP benefits.
- Lean engine-level for A so every surface tells the truth; keep the demotion a bounded score
  penalty (not removal) so ordering among non-superseded results is unchanged.

## Cost
One `GetReverseAssociations` per returned candidate (already done under `annotate`). For
typical limit 5–20 this is negligible; guard large limits. Measure under -race; no new index.

## Test plan (RED-first)
- Seed A superseded-by B via link (predecessor active). Recall a query matching both.
  RED (today): A ranks above B, no annotation by default.
  GREEN: B ranks above A; A carries `superseded_by=B` with no `annotate` arg.
- Invariant: a query matching neither is unaffected (ordering among non-superseded stable).
- Chain A<-B<-C: only the head (C) is un-superseded; A and B both demoted+annotated.
- -race on the recall path; COG invariant tests still green.

## Sibling recall-ranking fixes (same subsystem — sequence after this, own increments)
- threshold gates BLENDED score, not vector_score (finding #3): items with vector 0.00 pass
  0.5 via activation. Fix to gate vector_score, then lower default to ~0.3. Own increment.
- activation/recency pollution overrides relevance (finding #7): for factual/state queries,
  rank by similarity with activation as tiebreak, not dominant additive term. Bigger; RFC.

## Out of scope here (separate RFCs / issues)
- Semantic/auto contradiction detection at write (finding #2) — the killer feature; own RFC.
- entity-graph UNION tag-index routing (finding #4).
- evolve concept-regeneration (finding #8; interacts with #653).
- due:-tag deterministic routing (finding #9).
- local embedder default-ON (finding #6) — product/config decision (privacy/binary size).
