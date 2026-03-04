# ADR: Association Durability — Metadata Preservation and Peak Weight

**Date:** 2026-03-04
**Status:** Implemented
**Branch:** feat/association-durability
**Prompted by:** Architectural analysis from Claude/Morpheus (Hippoclaudus system, DeCue Technologies)

## Problem

MuninnDB's association storage had two foundational defects:

### 1. Metadata destruction bug (silent, since initial implementation)

Every association write path called `encodeAssocValue(0, 1.0, time.Time{}, 0)` — a hardcoded blank value. This destroyed `relType`, `confidence`, `createdAt`, and `lastActivated` on every Hebbian weight update and every decay pass. The `lastActivated` field was designed for recency-aware decay but had never worked. The `confidence` field was always reset to 1.0 regardless of what was written. `relType` was always zeroed after the first weight update.

### 2. No historical weight tracking

The association value stored only current weight. There was no way to distinguish an association that peaked at 0.8 and decayed from one that never exceeded 0.05. This made principled pruning decisions impossible and meant Phase 5 transitive inference could not consider historically-strong chains.

## Root Cause Analysis

The system had a total-recall promise for engrams (0.05 Ebbinghaus floor prevents complete forgetting) but no equivalent for associations. The decay model applied a time-independent multiplicative factor (`weight * 0.95` per pass) without reading `lastActivated` — because `lastActivated` was being zeroed on every write. The infrastructure for recency-aware behavior existed in the data model but was never activated.

## Decision

### Phase 0: Fix metadata destruction (no format change)

**Files:** `internal/storage/association.go`

1. `UpdateAssocWeight` and `UpdateAssocWeightBatch`: Added `getAssocValue` helper that reads the existing 0x03 forward key value before deleting it. Now carries `relType`, `confidence`, `createdAt` forward and sets `lastActivated = int32(time.Now().Unix())` on every Hebbian weight update.

2. `DecayAssocWeights`: Extended `assocEntry` struct to carry decoded metadata. Scan loop decodes `iter.Value()` and passes all fields through to `flushChunk`. `flushChunk` uses per-entry `encodeAssocValue(...)` instead of a single blank value.

3. Recency skip: Added 5-minute grace window. If `lastActivated > 0` and `time.Since(activatedAt) < 5min`, the edge is skipped entirely on the decay pass. This protects edges used moments ago from being immediately penalized by the next scheduled decay.

### Phase 1: Add PeakWeight (18→22 byte Pebble value)

**Files:** `internal/storage/types.go`, `internal/storage/association.go`, `internal/consolidation/transitive.go`

New value layout: `relType(2) | confidence(4) | createdAt(8) | lastActivated(4) | peakWeight(4) = 22 bytes`

**Backward compatibility:** Old 18-byte values decode with `peakWeight = 0`. No migration required — the system self-heals:
- On first `UpdateAssocWeight` call: `peakWeight = max(0, newWeight)` = newWeight
- On first `DecayAssocWeights` pass: `if peakWeight == 0 { peakWeight = oldW }` bootstraps from current weight

**PeakWeight semantics:**
- Set to `Weight` on `WriteAssociation` (initial write)
- Updated as `max(existingPeak, newWeight)` on every `UpdateAssocWeight`/`UpdateAssocWeightBatch`
- Never decreases
- `DecayAssocWeights` does NOT reduce `PeakWeight` (only current weight decays)

**Dynamic decay floor:** When `newW < minWeight`, compute `dynamicFloor = peakWeight * 0.05`. If `dynamicFloor > 0`, clamp `newW = dynamicFloor` instead of deleting. An association that peaked at 0.8 gets floor 0.04; one at 0.1 gets floor 0.005.

**Phase 5 transitive inference:** Changed threshold check from `ab.Weight >= minWeight` to `max(ab.Weight, ab.PeakWeight) >= minWeight`. Inferred edge weight still uses current `Weight` (not Peak) to avoid inflating new edges.

### Note on `DecayAssocWeights` return value

The `removed int` return value previously counted all edges deleted below `minWeight`. After this change, it counts only edges with no dynamic floor — unreachable in practice after the legacy bootstrap. Callers that used `removed` as a "pruned edge" metric will see 0. This is intentional: edges are no longer pruned, they clamp to their floor.

## Consequences

### Solved

- `relType`, `confidence`, `createdAt`, `lastActivated` survive Hebbian updates and decay passes
- `lastActivated` is correctly set on weight updates and enables recency-aware skip
- `PeakWeight` records the historical maximum for every association
- Earned associations (high peak) survive moderate dormancy via dynamic floor
- Phase 5 can reconstruct transitive chains through partially-decayed edges

### Deliberately Not Solved (and why)

**Association archiving (0x25 prefix):** The external collaborators proposed archiving decayed associations instead of deleting them, preserving cluster topology for wholesale restoration. This is architecturally sound but requires:
- A GC policy for the archive (not designed)
- A reactivation path on cluster re-access
- 6+ new file touchpoints
- Second-order pruning for archives that outlive their usefulness (N years dormant)

`PeakWeight` is the foundation the archiving proposal requires. With peak weight persisted, archiving can be added without re-examining the storage model. We chose to ship the foundation first and defer archiving until the need is validated by production behavior.

**Co-activation count per pair:** Useful for frequency-vs-importance discrimination (high count + high peak = truly earned; high count + low peak = noise). Deferred. `PeakWeight` enables the same decisions and is simpler. Extend to 26 bytes when co-activation count is needed.

**Phase 1 replay ceiling (top-50):** Consolidation replay is limited to the top 50 most-relevant engrams. Dormant cluster engrams fall below this window and stop getting Hebbian reinforcement. This is a tuning parameter, not a storage defect — fix by including high-peak-association engrams in the replay candidate set regardless of current relevance.

**Bridge-edge concept:** No distinction between intra-cluster and inter-cluster bridge associations. Bridge edges (connecting otherwise-independent clusters) may deserve independent preservation rules. Requires cluster detection. Deferred.

## What Should Be Done Next

1. **Co-activation count per pair** — Extend value to 26 bytes: `peakWeight(4) | coactivationCount(4)`. Thread count through `processBatch` in `hebbian.go` (currently aggregated in-memory and discarded). Enables principled archiving thresholds.

2. **Association archiving** — New 0x25 Pebble prefix for archived associations (high peak + high co-activation count, but decayed below floor). Restoration on cluster re-access. Requires GC policy.

3. **Phase 1 replay ceiling** — Change from top-50 by relevance to a composite scan that includes engrams participating in high-peak-weight associations regardless of current relevance. Ensures dormant cluster seeds stay in the replay candidate set.

4. **Recency grace window** — Currently 5 minutes (chosen to satisfy test constraints). Should be configurable per vault as a `PlasticityConfig` field. Suggested default: 1 hour for production vaults.

5. **Bridge-edge detection** — Associations connecting engrams from two otherwise-independent clusters deserve their own preservation rules. Requires cluster detection (modularity / Louvain / connected components).
