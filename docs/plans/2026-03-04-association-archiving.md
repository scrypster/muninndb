# Association Archiving Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Archive dormant-but-strong association edges to a separate Pebble namespace and lazily restore them when their engrams are re-activated, enabling long-dormant agents to coherently recall deep historical structure.

**Architecture:** Edges that decay to the dynamic floor but exceed a consolidation score threshold (peakWeight x coActivationCount / daysSinceLastActivation > 0.05) are moved atomically from the live 0x03/0x04/0x14 namespace to a 0x25 archive namespace. On activation, a Bloom filter gates a cheap scan; hits restore the top-10 edges by consolidation score at peakWeight x 0.25, writing them back into the live index before BFS traverses them. A weekly GC pass prunes edges that were never strong enough to matter.

**Tech Stack:** Go, Pebble LSM (via internal/storage), bloom filter (bits-and-blooms/bloom or similar), existing CognitiveForwarder replication pattern

---

## Task 1: Key prefix constant + archive key builder

Add the `0x25` archive namespace constant and `ArchiveAssocKey` builder function to the keys package.

### Files

| File | Action |
|------|--------|
| `internal/storage/keys/keys.go` | Add `ArchiveAssocKey`, `ArchiveAssocPrefixForID`, `ArchiveAssocRangeStart`, `ArchiveAssocRangeEnd` |
| `internal/storage/keys/keys_test.go` | Add test for archive key builder |

### Steps

1. **Failing test.** Create `TestArchiveAssocKey_Layout` in `internal/storage/keys/keys_test.go`:

```go
func TestArchiveAssocKey_Layout(t *testing.T) {
	ws := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	src := [16]byte{0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F}
	dst := [16]byte{0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F}

	key := ArchiveAssocKey(ws, src, dst)

	// Total length: 1 (prefix) + 8 (ws) + 16 (src) + 16 (dst) = 41 bytes
	if len(key) != 41 {
		t.Fatalf("expected 41 bytes, got %d", len(key))
	}
	if key[0] != 0x25 {
		t.Errorf("prefix byte: got 0x%02X, want 0x25", key[0])
	}
	if !bytes.Equal(key[1:9], ws[:]) {
		t.Error("ws mismatch")
	}
	if !bytes.Equal(key[9:25], src[:]) {
		t.Error("src mismatch")
	}
	if !bytes.Equal(key[25:41], dst[:]) {
		t.Error("dst mismatch")
	}
}

func TestArchiveAssocPrefixForID_Length(t *testing.T) {
	ws := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	src := [16]byte{0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F}

	prefix := ArchiveAssocPrefixForID(ws, src)
	if len(prefix) != 25 {
		t.Fatalf("expected 25 bytes, got %d", len(prefix))
	}
	if prefix[0] != 0x25 {
		t.Errorf("prefix byte: got 0x%02X, want 0x25", prefix[0])
	}
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/keys/ -run "TestArchiveAssocKey|TestArchiveAssocPrefixForID" -count=1
```

3. **Implement.** Add to `internal/storage/keys/keys.go` after the `CoOccurrencePrefix` function (around line 610):

```go
// ArchiveAssocKey constructs the archived association key (0x25 prefix).
// No weight complement — archive keys are not sorted by weight.
// No reverse key — restore is one-directional (BFS always traverses outbound edges).
// Key: 0x25 | wsPrefix(8) | src(16) | dst(16) = 41 bytes
func ArchiveAssocKey(ws [8]byte, src [16]byte, dst [16]byte) []byte {
	key := make([]byte, 1+8+16+16)
	key[0] = 0x25
	copy(key[1:9], ws[:])
	copy(key[9:25], src[:])
	copy(key[25:41], dst[:])
	return key
}

// ArchiveAssocPrefixForID returns a 25-byte scan prefix covering all archived
// associations from a given source engram (0x25 | ws(8) | src(16)).
func ArchiveAssocPrefixForID(ws [8]byte, src [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x25
	copy(key[1:9], ws[:])
	copy(key[9:25], src[:])
	return key
}

// ArchiveAssocRangeStart returns the inclusive lower bound for scanning all
// archived associations within a vault (0x25 | ws(8)).
func ArchiveAssocRangeStart(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x25
	copy(key[1:9], ws[:])
	return key
}

// ArchiveAssocRangeEnd returns the exclusive upper bound for scanning all
// archived associations within a vault. Increments the ws portion.
func ArchiveAssocRangeEnd(ws [8]byte) []byte {
	end := make([]byte, 1+8)
	end[0] = 0x25
	copy(end[1:9], ws[:])
	for i := len(end) - 1; i >= 1; i-- {
		end[i]++
		if end[i] != 0 {
			break
		}
	}
	return end
}
```

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/keys/ -run "TestArchiveAssocKey|TestArchiveAssocPrefixForID" -count=1
```

5. **Commit.**

```bash
git add internal/storage/keys/keys.go internal/storage/keys/keys_test.go
git commit -m "feat(storage): add 0x25 archive assoc key prefix and builders"
```

---

## Task 2: `encodeArchiveValue` / `decodeAssocValue` 30-byte extension

Add `encodeArchiveValue` that produces 30-byte values (existing 26 bytes + `restoredAt int32`), and extend `decodeAssocValue` to handle `>= 30` byte values.

### Files

| File | Action |
|------|--------|
| `internal/storage/association.go` | Add `encodeArchiveValue`, extend `decodeAssocValue` return signature |
| `internal/storage/association_test.go` | Add round-trip test |

### Steps

1. **Failing test.** Add `TestEncodeArchiveValue_RoundTrip` in `internal/storage/association_test.go`:

```go
func TestEncodeArchiveValue_RoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	relType := RelSupports
	confidence := float32(0.85)
	createdAt := now.Add(-24 * time.Hour)
	lastActivated := int32(now.Unix())
	peakWeight := float32(0.92)
	coActivationCount := uint32(42)
	restoredAt := int32(now.Unix())

	val := encodeArchiveValue(relType, confidence, createdAt, lastActivated, peakWeight, coActivationCount, restoredAt)
	if len(val) != 30 {
		t.Fatalf("expected 30 bytes, got %d", len(val))
	}

	gotRelType, gotConf, gotCreated, gotLastAct, gotPeak, gotCoAct, gotRestored := decodeAssocValue(val[:])
	if gotRelType != relType {
		t.Errorf("relType: got %v, want %v", gotRelType, relType)
	}
	if gotConf < 0.84 || gotConf > 0.86 {
		t.Errorf("confidence: got %v, want ~0.85", gotConf)
	}
	if gotCreated.Unix() != createdAt.Unix() {
		t.Errorf("createdAt: got %v, want %v", gotCreated, createdAt)
	}
	if gotLastAct != lastActivated {
		t.Errorf("lastActivated: got %v, want %v", gotLastAct, lastActivated)
	}
	if gotPeak < 0.91 || gotPeak > 0.93 {
		t.Errorf("peakWeight: got %v, want ~0.92", gotPeak)
	}
	if gotCoAct != coActivationCount {
		t.Errorf("coActivationCount: got %v, want %v", gotCoAct, coActivationCount)
	}
	if gotRestored != restoredAt {
		t.Errorf("restoredAt: got %v, want %v", gotRestored, restoredAt)
	}
}

func TestDecodeAssocValue_26Bytes_RestoredAtZero(t *testing.T) {
	val := encodeAssocValue(RelSupports, 0.9, time.Now(), 100, 0.8, 5)
	_, _, _, _, _, _, restoredAt := decodeAssocValue(val[:])
	if restoredAt != 0 {
		t.Errorf("restoredAt from 26-byte value: got %v, want 0", restoredAt)
	}
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestEncodeArchiveValue_RoundTrip|TestDecodeAssocValue_26Bytes_RestoredAtZero" -count=1
```

3. **Implement.** In `internal/storage/association.go`:

   a. Update `decodeAssocValue` signature to return 7 values (add `restoredAt int32` as the 7th return):

```go
func decodeAssocValue(val []byte) (relType RelType, confidence float32, createdAt time.Time, lastActivated int32, peakWeight float32, coActivationCount uint32, restoredAt int32) {
```

   Add at the end before `return`:

```go
	if len(val) >= 30 {
		restoredAt = int32(binary.BigEndian.Uint32(val[26:30]))
	}
```

   b. Add `encodeArchiveValue` after `encodeAssocValue`:

```go
// encodeArchiveValue serializes association metadata into the 30-byte value
// stored under 0x25 archive keys.
// Layout: relType(2) | confidence(4) | createdAt(8) | lastActivated(4) | peakWeight(4) | coActivationCount(4) | restoredAt(4) = 30 bytes
func encodeArchiveValue(relType RelType, confidence float32, createdAt time.Time, lastActivated int32, peakWeight float32, coActivationCount uint32, restoredAt int32) [30]byte {
	var val [30]byte
	binary.BigEndian.PutUint16(val[0:2], uint16(relType))
	binary.BigEndian.PutUint32(val[2:6], math.Float32bits(confidence))
	var nanos int64
	if !createdAt.IsZero() {
		nanos = createdAt.UnixNano()
	}
	binary.BigEndian.PutUint64(val[6:14], uint64(nanos))
	binary.BigEndian.PutUint32(val[14:18], uint32(lastActivated))
	binary.BigEndian.PutUint32(val[18:22], math.Float32bits(peakWeight))
	binary.BigEndian.PutUint32(val[22:26], coActivationCount)
	binary.BigEndian.PutUint32(val[26:30], uint32(restoredAt))
	return val
}
```

   c. Run `grep -rn "decodeAssocValue" internal/storage/` to find all callers in the package. Update each to destructure the new 7th return value `restoredAt`. Unused sites should assign `_ = restoredAt`.

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestEncodeArchiveValue_RoundTrip|TestDecodeAssocValue_26Bytes_RestoredAtZero" -count=1
```

5. **Commit.**

```bash
git add internal/storage/association.go internal/storage/association_test.go
git commit -m "feat(storage): add encodeArchiveValue (30-byte) and extend decodeAssocValue with restoredAt"
```

---

## Task 3: `PlasticityConfig` -- add `ArchiveThreshold`

Add `ArchiveThreshold *float64` to `PlasticityConfig` and wire it through `ResolvedPlasticity` with default `0.05`.

### Files

| File | Action |
|------|--------|
| `internal/auth/plasticity.go` | Add field to `PlasticityConfig`, `ResolvedPlasticity`, `plasticityPreset`, all presets, `ResolvePlasticity` |
| `internal/auth/plasticity_test.go` | Add test for default and override |

### Steps

1. **Failing test.** Add in `internal/auth/plasticity_test.go`:

```go
func TestPlasticityConfig_ArchiveThreshold_Default(t *testing.T) {
	r := ResolvePlasticity(&PlasticityConfig{Preset: "default"})
	if r.ArchiveThreshold != 0.05 {
		t.Errorf("default ArchiveThreshold: got %v, want 0.05", r.ArchiveThreshold)
	}
}

func TestPlasticityConfig_ArchiveThreshold_Override(t *testing.T) {
	val := 0.10
	r := ResolvePlasticity(&PlasticityConfig{ArchiveThreshold: &val})
	if r.ArchiveThreshold != 0.10 {
		t.Errorf("overridden ArchiveThreshold: got %v, want 0.10", r.ArchiveThreshold)
	}
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/auth/ -run "TestPlasticityConfig_ArchiveThreshold" -count=1
```

3. **Implement.** In `internal/auth/plasticity.go`:

   a. Add to `PlasticityConfig` struct (after `AssocMinWeight`):

```go
	ArchiveThreshold *float64 `json:"archive_threshold,omitempty"` // consolidation score threshold for archiving (default 0.05)
```

   b. Add to `ResolvedPlasticity` struct (after `AssocMinWeight`):

```go
	ArchiveThreshold float64 `json:"archive_threshold"` // consolidation score threshold for archiving
```

   c. Add to `plasticityPreset` struct (after `AssocMinWeight`):

```go
	ArchiveThreshold float64
```

   d. Set `ArchiveThreshold: 0.05` in all four preset maps (`default`, `reference`, `scratchpad`, `knowledge-graph`).

   e. In `ResolvePlasticity`, add to the initial copy block:

```go
	ArchiveThreshold: p.ArchiveThreshold,
```

   f. Add override block after the `AssocMinWeight` override (around line 387):

```go
	if cfg.ArchiveThreshold != nil {
		v := *cfg.ArchiveThreshold
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		r.ArchiveThreshold = v
	}
```

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/auth/ -run "TestPlasticityConfig_ArchiveThreshold" -count=1
```

5. **Commit.**

```bash
git add internal/auth/plasticity.go internal/auth/plasticity_test.go
git commit -m "feat(auth): add ArchiveThreshold to PlasticityConfig (default 0.05)"
```

---

## Task 4: Archive trigger in `DecayAssocWeights`

When an edge hits the dynamic floor AND its consolidation score exceeds `archiveThreshold`, move it to `0x25` instead of clamping.

### Files

| File | Action |
|------|--------|
| `internal/storage/association.go` | Modify `DecayAssocWeights` signature to accept `archiveThreshold float64`, add archive branch in `flushChunk` |
| `internal/storage/association_test.go` | Add `TestDecayAssocWeights_ArchivesStrongEdge` |
| `internal/storage/association_durability_test.go` | Modify: 4 calls to `DecayAssocWeights` need a 5th arg (pass `0.0` to disable archiving in tests) |
| `internal/storage/assoc_weight_index_test.go` | Modify: 1 call to `DecayAssocWeights` needs a 5th arg (pass `0.0`) |
| `internal/storage/store.go` | Modify: `EngineStore` interface — update `DecayAssocWeights` signature to include the new `archiveThreshold float64` parameter |
| `internal/cognitive/hebbian.go` | Update `HebbianStore` interface to match new signature |
| `internal/cognitive/store_adapters.go` | Update adapter to pass through new param |
| `internal/engine/engine.go` | Pass `resolved.ArchiveThreshold` to `DecayAssocWeights` |
| `internal/engine/engine_pruning_test.go` | Modify: update the call to `store.DecayAssocWeights(ctx, ws, 0.95, 0.05)` to pass the new `archiveThreshold` argument |
| `cmd/bench/adapters.go` | Update bench adapter |
| `internal/cognitive/hebbian_test.go` | Update mock |
| `internal/cognitive/mechanics_proof_test.go` | Update mock |

### Steps

1. **Failing test.** Add in `internal/storage/association_test.go`:

```go
func TestDecayAssocWeights_ArchivesStrongEdge(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("decay-archive")

	src := NewULID()
	dst := NewULID()

	// Write edge: weight=0.8, peakWeight will be 0.8, coActivationCount=1 (seed).
	// After many decays to floor (0.8*0.05=0.04), consolidation score:
	// peakWeight(0.8) * coActivationCount(1) / max(daysSinceLastActivation,1) > 0.05 threshold?
	// 0.8 * 1 / 1 = 0.8 > 0.05 => YES, should archive.
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID:      dst,
		Weight:        0.8,
		RelType:       RelSupports,
		LastActivated: int32(time.Now().Add(-48 * time.Hour).Unix()), // 2 days ago (outside grace window)
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Decay aggressively to force below minWeight, with archiveThreshold=0.05.
	removed, err := store.DecayAssocWeights(ctx, ws, 0.01, 0.3, 0.05)
	if err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}
	// Edge was archived, not "removed" (removed=0 for archival).
	_ = removed

	// Verify edge is gone from live index (0x14).
	w, _ := store.GetAssocWeight(ctx, ws, src, dst)
	if w > 0 {
		t.Errorf("live weight should be 0 after archive, got %v", w)
	}

	// Verify edge exists in archive (0x25).
	archiveKey := keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst))
	val, err := Get(store.db, archiveKey)
	if err != nil || val == nil {
		t.Fatal("archived edge not found in 0x25 namespace")
	}
	if len(val) != 30 {
		t.Fatalf("archive value should be 30 bytes, got %d", len(val))
	}
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestDecayAssocWeights_ArchivesStrongEdge" -count=1
```

3. **Implement.**

   a. Change `DecayAssocWeights` signature in `internal/storage/association.go` to add `archiveThreshold float64` parameter:

```go
func (ps *PebbleStore) DecayAssocWeights(ctx context.Context, wsPrefix [8]byte, decayFactor float64, minWeight float32, archiveThreshold float64) (int, error) {
```

   b. Add `archive bool` field to the `assocEntry` struct inside `DecayAssocWeights`.

   c. In the `if newW < minWeight` branch (around line 530), replace the existing logic with:

```go
		if newW < minWeight {
			dynamicFloor := peakWeight * 0.05
			if dynamicFloor > 0 {
				// Compute consolidation score for archive eligibility.
				daysSinceLastAct := float64(1)
				if lastActivated > 0 {
					daysSinceLastAct = time.Since(time.Unix(int64(lastActivated), 0)).Hours() / 24
					if daysSinceLastAct < 1 {
						daysSinceLastAct = 1
					}
				}
				consolidationScore := (float64(peakWeight) * float64(coActivationCount)) / daysSinceLastAct
				if archiveThreshold > 0 && consolidationScore > archiveThreshold {
					// Archive: strong enough to remember, too dormant to keep live.
					e.archive = true
					e.newW = dynamicFloor // value used for archive encoding only
				} else {
					// Clamp to floor (existing behavior).
					e.newW = dynamicFloor
				}
			} else {
				e.remove = true
				removed++
			}
		}
```

   d. In `flushChunk`, add archive handling after the `if e.remove` block. When `e.archive` is true:

```go
			if e.archive {
				// Write to 0x25 archive namespace.
				archiveKey := keys.ArchiveAssocKey(wsPrefix, e.src, e.dst)
				archiveVal := encodeArchiveValue(e.relType, e.confidence, e.createdAt, e.lastActivated, e.peakWeight, e.coActivationCount, 0)
				_ = batch.Set(archiveKey, archiveVal[:], nil)
				// Delete from live indexes (0x03, 0x04, 0x14).
				_ = batch.Delete(keys.AssocWeightIndexKey(wsPrefix, e.src, e.dst), nil)
			} else if !e.remove {
```

   (The existing `batch.Delete` for 0x03/0x04 already runs for all entries at the top of the loop.)

   e. Update `EngineStore` interface in `internal/storage/store.go` to match the new `DecayAssocWeights` signature (add `archiveThreshold float64` parameter).

   f. Update the interface in `internal/cognitive/hebbian.go`:

```go
	DecayAssocWeights(ctx context.Context, ws [8]byte, decayFactor float64, minWeight float32, archiveThreshold float64) (int, error)
```

   g. Update adapter in `internal/cognitive/store_adapters.go`:

```go
func (a *hebbianStoreAdapter) DecayAssocWeights(ctx context.Context, ws [8]byte, decayFactor float64, minWeight float32, archiveThreshold float64) (int, error) {
	return a.store.DecayAssocWeights(ctx, ws, decayFactor, minWeight, archiveThreshold)
}
```

   g. Update call in `internal/engine/engine.go` (around line 2535):

```go
					removed, err := e.store.DecayAssocWeights(e.stopCtx, ws,
						float64(resolved.AssocDecayFactor), resolved.AssocMinWeight, resolved.ArchiveThreshold)
```

   h. Update bench adapter in `cmd/bench/adapters.go`:

```go
func (a *benchHebbianAdapter) DecayAssocWeights(ctx context.Context, ws [8]byte, factor float64, min float32, archiveThreshold float64) (int, error) {
	return a.store.DecayAssocWeights(ctx, ws, factor, min, archiveThreshold)
}
```

   i. Update mocks in `internal/cognitive/hebbian_test.go` and `internal/cognitive/mechanics_proof_test.go` to accept the new parameter.

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestDecayAssocWeights_ArchivesStrongEdge" -count=1 && go test ./internal/cognitive/ -count=1 && go test ./internal/engine/ -count=1 -short
```

5. **Commit.**

```bash
git add internal/storage/association.go internal/storage/association_test.go internal/storage/store.go internal/cognitive/hebbian.go internal/cognitive/store_adapters.go internal/engine/engine.go internal/engine/engine_pruning_test.go cmd/bench/adapters.go internal/cognitive/hebbian_test.go internal/cognitive/mechanics_proof_test.go
git commit -m "feat(storage): archive trigger in DecayAssocWeights for strong dormant edges"
```

---

## Task 5: Bloom filter over archived source engram IDs

Add an in-memory Bloom filter that tracks which source engram IDs have archived edges. Built on startup by scanning `0x25`, updated on archive (add).

> **Note:** Standard `bits-and-blooms/bloom` does not support removal. False positives from non-removed entries are acceptable — they trigger a cheap 0x25 prefix scan that finds nothing. The weekly GC pass (Task 11) can call `RebuildArchiveBloom` to periodically compact the filter.

### Files

| File | Action |
|------|--------|
| `go.mod` / `go.sum` | Add `github.com/bits-and-blooms/bloom/v3` dependency |
| `internal/storage/archive_bloom.go` | New file: `ArchiveBloom` struct with `Init`, `Add`, `MayContain` |
| `internal/storage/archive_bloom_test.go` | New file: test Bloom filter ops |
| `internal/storage/impl.go` | Add `archiveBloom *ArchiveBloom` field to `PebbleStore`, init in `NewPebbleStore` |

### Steps

1. **Failing test.** Create `internal/storage/archive_bloom_test.go`:

```go
package storage

import (
	"testing"
)

func TestArchiveBloom_AddAndMayContain(t *testing.T) {
	bloom := NewArchiveBloom(10000, 0.01)

	id := NewULID()
	other := NewULID()

	bloom.Add(id)

	if !bloom.MayContain(id) {
		t.Error("Bloom filter should report MayContain=true for added ID")
	}
	// other was never added — false positives are possible but unlikely with low cardinality.
	// We don't assert MayContain==false because Bloom filters have FP; just verify no panic.
	_ = bloom.MayContain(other)
}

func TestArchiveBloom_Rebuild(t *testing.T) {
	store := newTestStore(t)

	// Write an archive key directly to simulate startup scan.
	ws := store.VaultPrefix("bloom-rebuild")
	src := NewULID()
	dst := NewULID()

	archiveKey := keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst))
	archiveVal := encodeArchiveValue(RelSupports, 0.9, time.Now(), 100, 0.8, 5, 0)
	if err := store.db.Set(archiveKey, archiveVal[:], nil); err != nil {
		t.Fatal(err)
	}

	// Rebuild Bloom from 0x25 scan.
	store.RebuildArchiveBloom()

	if !store.archiveBloom.MayContain(src) {
		t.Error("after rebuild, Bloom should contain the archived src ID")
	}
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestArchiveBloom" -count=1
```

3. **Implement.**

   a. Add dependency:

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go get github.com/bits-and-blooms/bloom/v3
```

   b. Create `internal/storage/archive_bloom.go`:

```go
package storage

import (
	"sync"

	bloomfilter "github.com/bits-and-blooms/bloom/v3"
	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// ArchiveBloom is a thread-safe Bloom filter over archived source engram IDs.
// Used as a fast gate before scanning 0x25 for restore candidates.
type ArchiveBloom struct {
	mu sync.RWMutex
	bf *bloomfilter.BloomFilter
}

// NewArchiveBloom creates a new Bloom filter sized for n expected items at fp false-positive rate.
func NewArchiveBloom(n uint, fp float64) *ArchiveBloom {
	return &ArchiveBloom{
		bf: bloomfilter.NewWithEstimates(n, fp),
	}
}

// Add inserts a source engram ID into the Bloom filter.
func (ab *ArchiveBloom) Add(id ULID) {
	ab.mu.Lock()
	ab.bf.Add(id[:])
	ab.mu.Unlock()
}

// MayContain returns true if the ID might be in the archive (may false-positive).
func (ab *ArchiveBloom) MayContain(id ULID) bool {
	ab.mu.RLock()
	defer ab.mu.RUnlock()
	return ab.bf.Test(id[:])
}

// RebuildArchiveBloom rebuilds the Bloom filter by scanning all 0x25 keys.
// Called at startup.
func (ps *PebbleStore) RebuildArchiveBloom() {
	bloom := NewArchiveBloom(100_000, 0.01)

	// Scan all 0x25 keys across all vaults.
	lower := []byte{0x25}
	upper := []byte{0x26}
	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
	})
	if err != nil {
		ps.archiveBloom = bloom
		return
	}
	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		k := iter.Key()
		// Key: 0x25 | ws(8) | src(16) | dst(16) = 41 bytes
		if len(k) < 25 {
			continue
		}
		var src ULID
		copy(src[:], k[9:25])
		bloom.Add(src)
	}

	ps.archiveBloom = bloom
}
```

   c. In `internal/storage/impl.go`, add `archiveBloom *ArchiveBloom` field to `PebbleStore` struct and initialize it in `NewPebbleStore`:

```go
	archiveBloom: NewArchiveBloom(100_000, 0.01),
```

   d. Also add the import for `keys` in `archive_bloom.go` (even though it's used in tests; keep it for `ArchiveAssocKey` references if needed, or remove if the linter complains).

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestArchiveBloom" -count=1
```

5. **Commit.**

```bash
git add go.mod go.sum internal/storage/archive_bloom.go internal/storage/archive_bloom_test.go internal/storage/impl.go
git commit -m "feat(storage): add Bloom filter for archived source engram IDs"
```

---

## Task 6: Restore function

Add `RestoreArchivedEdges` to `PebbleStore`: scans `0x25` for a given source ID, sorts by consolidation score, restores top-10 at `peakWeight * 0.25`, writes to `0x03`/`0x04`/`0x14`, invalidates `assocCache`, stamps `restoredAt`.

### Files

| File | Action |
|------|--------|
| `internal/storage/archive_restore.go` | New file: `RestoreArchivedEdges` method |
| `internal/storage/archive_restore_test.go` | New file: test restore round-trip |

### Steps

1. **Failing test.** Create `internal/storage/archive_restore_test.go`:

```go
package storage

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

func TestRestoreArchivedEdges_RestoresTopByConsolidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("restore-test")

	src := NewULID()
	dst1 := NewULID()
	dst2 := NewULID()

	// Manually write two archived edges with different consolidation scores.
	// dst1: peakWeight=0.9, coActivationCount=10, lastActivated=1 day ago => score = 0.9*10/1 = 9.0
	// dst2: peakWeight=0.2, coActivationCount=1, lastActivated=1 day ago  => score = 0.2*1/1 = 0.2
	now := time.Now()
	lastAct := int32(now.Add(-24 * time.Hour).Unix())

	arc1 := encodeArchiveValue(RelSupports, 0.9, now.Add(-72*time.Hour), lastAct, 0.9, 10, 0)
	if err := store.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst1)), arc1[:], nil); err != nil {
		t.Fatal(err)
	}
	arc2 := encodeArchiveValue(RelRelatesTo, 0.5, now.Add(-72*time.Hour), lastAct, 0.2, 1, 0)
	if err := store.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst2)), arc2[:], nil); err != nil {
		t.Fatal(err)
	}

	// Add src to Bloom filter.
	store.archiveBloom.Add(src)

	restored, err := store.RestoreArchivedEdges(ctx, ws, src, 10)
	if err != nil {
		t.Fatalf("RestoreArchivedEdges: %v", err)
	}
	if len(restored) != 2 {
		t.Fatalf("expected 2 restored edges, got %d", len(restored))
	}

	// Verify dst1 is restored with weight = peakWeight * 0.25 = 0.225.
	w1, _ := store.GetAssocWeight(ctx, ws, src, dst1)
	wantW1 := float32(0.9 * 0.25)
	if w1 < wantW1-0.01 || w1 > wantW1+0.01 {
		t.Errorf("restored weight for dst1: got %v, want ~%v", w1, wantW1)
	}

	// Verify archive key is deleted.
	val, _ := Get(store.db, keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst1)))
	if val != nil {
		t.Error("archive key should be deleted after restore")
	}
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestRestoreArchivedEdges" -count=1
```

3. **Implement.** Create `internal/storage/archive_restore.go`:

```go
package storage

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// restoredEdge is an edge restored from the archive.
type restoredEdge struct {
	Dst               ULID
	RelType           RelType
	Confidence        float32
	CreatedAt         time.Time
	LastActivated     int32
	PeakWeight        float32
	CoActivationCount uint32
	ConsolidationScore float64
}

// RestoreArchivedEdges scans the 0x25 archive for edges from src, sorts by
// consolidation score, and restores the top-N into the live 0x03/0x04/0x14
// indexes. Returns the list of restored dst IDs.
func (ps *PebbleStore) RestoreArchivedEdges(ctx context.Context, wsPrefix [8]byte, src ULID, maxRestore int) ([]ULID, error) {
	prefix := keys.ArchiveAssocPrefixForID(wsPrefix, [16]byte(src))
	iter, err := PrefixIterator(ps.db, prefix)
	if err != nil {
		return nil, fmt.Errorf("archive prefix iterator: %w", err)
	}
	defer iter.Close()

	var candidates []restoredEdge
	for iter.First(); iter.Valid(); iter.Next() {
		k := iter.Key()
		if len(k) < 41 {
			continue
		}
		var dst ULID
		copy(dst[:], k[25:41])

		relType, confidence, createdAt, lastActivated, peakWeight, coActivationCount, _ := decodeAssocValue(iter.Value())

		daysSinceLastAct := float64(1)
		if lastActivated > 0 {
			daysSinceLastAct = time.Since(time.Unix(int64(lastActivated), 0)).Hours() / 24
			if daysSinceLastAct < 1 {
				daysSinceLastAct = 1
			}
		}
		score := (float64(peakWeight) * float64(coActivationCount)) / daysSinceLastAct

		candidates = append(candidates, restoredEdge{
			Dst:                dst,
			RelType:            relType,
			Confidence:         confidence,
			CreatedAt:          createdAt,
			LastActivated:      lastActivated,
			PeakWeight:         peakWeight,
			CoActivationCount:  coActivationCount,
			ConsolidationScore: score,
		})
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("archive scan: %w", err)
	}

	// Sort by consolidation score descending.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ConsolidationScore > candidates[j].ConsolidationScore
	})
	if len(candidates) > maxRestore {
		candidates = candidates[:maxRestore]
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	now := int32(time.Now().Unix())
	var restored []ULID

	for _, c := range candidates {
		restoreWeight := c.PeakWeight * 0.25

		// Write live association.
		val := encodeArchiveValue(c.RelType, c.Confidence, c.CreatedAt, c.LastActivated, c.PeakWeight, c.CoActivationCount, now)
		fwdKey := keys.AssocFwdKey(wsPrefix, [16]byte(src), restoreWeight, [16]byte(c.Dst))
		batch.Set(fwdKey, val[:], nil)
		revKey := keys.AssocRevKey(wsPrefix, [16]byte(c.Dst), restoreWeight, [16]byte(src))
		batch.Set(revKey, val[:], nil)

		// Weight index.
		var wiBuf [4]byte
		binary.BigEndian.PutUint32(wiBuf[:], math.Float32bits(restoreWeight))
		batch.Set(keys.AssocWeightIndexKey(wsPrefix, [16]byte(src), [16]byte(c.Dst)), wiBuf[:], nil)

		// Delete archive key.
		batch.Delete(keys.ArchiveAssocKey(wsPrefix, [16]byte(src), [16]byte(c.Dst)), nil)

		restored = append(restored, c.Dst)
	}

	if err := batch.Commit(pebble.NoSync); err != nil {
		return nil, fmt.Errorf("restore batch commit: %w", err)
	}

	// Invalidate assoc cache for src and all restored dst nodes.
	ps.assocCache.Remove(assocCacheKey(wsPrefix, src))
	for _, dst := range restored {
		ps.assocCache.Remove(assocCacheKey(wsPrefix, dst))
	}

	return restored, nil
}
```

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestRestoreArchivedEdges" -count=1
```

5. **Commit.**

```bash
git add internal/storage/archive_restore.go internal/storage/archive_restore_test.go
git commit -m "feat(storage): RestoreArchivedEdges scans 0x25 and writes back to live indexes"
```

---

## Task 7: Lazy transitive restore (depth-2)

For each directly restored neighbor, also restore their top-5 archived edges (one extra hop of lazy restoration).

### Files

| File | Action |
|------|--------|
| `internal/storage/archive_restore.go` | Add `RestoreArchivedEdgesTransitive` that calls `RestoreArchivedEdges` with depth-2 logic |
| `internal/storage/archive_restore_test.go` | Add `TestRestoreArchivedEdges_Transitive` |

### Steps

1. **Failing test.** Add in `internal/storage/archive_restore_test.go`:

```go
func TestRestoreArchivedEdges_Transitive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("restore-transitive")

	src := NewULID()
	neighbor := NewULID()
	deepNeighbor := NewULID()

	now := time.Now()
	lastAct := int32(now.Add(-24 * time.Hour).Unix())

	// Archive src -> neighbor
	arc1 := encodeArchiveValue(RelSupports, 0.9, now.Add(-72*time.Hour), lastAct, 0.9, 10, 0)
	store.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(neighbor)), arc1[:], nil)

	// Archive neighbor -> deepNeighbor
	arc2 := encodeArchiveValue(RelRelatesTo, 0.7, now.Add(-72*time.Hour), lastAct, 0.7, 5, 0)
	store.db.Set(keys.ArchiveAssocKey(ws, [16]byte(neighbor), [16]byte(deepNeighbor)), arc2[:], nil)

	store.archiveBloom.Add(src)
	store.archiveBloom.Add(neighbor)

	restored, err := store.RestoreArchivedEdgesTransitive(ctx, ws, src, 10, 5)
	if err != nil {
		t.Fatalf("RestoreArchivedEdgesTransitive: %v", err)
	}

	// src->neighbor should be restored.
	w1, _ := store.GetAssocWeight(ctx, ws, src, neighbor)
	if w1 == 0 {
		t.Error("src->neighbor should be restored")
	}

	// neighbor->deepNeighbor should also be restored (transitive).
	w2, _ := store.GetAssocWeight(ctx, ws, neighbor, deepNeighbor)
	if w2 == 0 {
		t.Error("neighbor->deepNeighbor should be restored (transitive)")
	}

	_ = restored
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestRestoreArchivedEdges_Transitive" -count=1
```

3. **Implement.** Add to `internal/storage/archive_restore.go`:

```go
// RestoreArchivedEdgesTransitive restores archived edges for src (top-N),
// then for each directly restored neighbor, restores their top-M archived edges
// (depth-2 lazy transitive restore).
func (ps *PebbleStore) RestoreArchivedEdgesTransitive(ctx context.Context, wsPrefix [8]byte, src ULID, maxDirect int, maxTransitive int) ([]ULID, error) {
	directRestored, err := ps.RestoreArchivedEdges(ctx, wsPrefix, src, maxDirect)
	if err != nil {
		return nil, err
	}

	var allRestored []ULID
	allRestored = append(allRestored, directRestored...)

	// Depth-2: for each directly restored neighbor, restore their top-M.
	for _, neighbor := range directRestored {
		if !ps.archiveBloom.MayContain(neighbor) {
			continue
		}
		transitiveRestored, err := ps.RestoreArchivedEdges(ctx, wsPrefix, neighbor, maxTransitive)
		if err != nil {
			continue // best-effort for transitive restore
		}
		allRestored = append(allRestored, transitiveRestored...)
	}

	return allRestored, nil
}
```

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestRestoreArchivedEdges_Transitive" -count=1
```

5. **Commit.**

```bash
git add internal/storage/archive_restore.go internal/storage/archive_restore_test.go
git commit -m "feat(storage): lazy transitive restore (depth-2) for archived edges"
```

---

## Task 8: Restore trigger in activation engine

Insert the archive restore call between Phase 4.5 (PAS transition boost) and Phase 5 (`phase5Traverse`) in the activation engine.

### Files

| File | Action |
|------|--------|
| `internal/engine/activation/engine.go` | Add `phase4_75ArchiveRestore` method, call it between Phase 4.5 and Phase 5; add `RestoreArchivedEdgesTransitive` and `ArchiveBloomMayContain` to the `ActivationStore` interface |
| `internal/engine/activation/activation_test.go` | Add integration test; update any mocks of `ActivationStore` used in activation tests |

### Steps

1. **Failing test.** Add in `internal/engine/activation/activation_test.go` (or a new test file):

> **Note:** The test below is a skeleton. The implementer must wire up a real `PebbleStore` test harness (see `internal/engine/activation/engine_test.go` for the established pattern) and insert an archived edge directly at the `0x25` key before running activation. Flesh out the body before marking this task complete.

```go
func TestPhase4_75ArchiveRestore_RestoredEdgesVisibleToBFS(t *testing.T) {
	// 1. Create a store with an archived edge (src → dst at 0x25)
	// 2. Run activation with src as a seed engram
	// 3. Assert dst appears in the activation results
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/engine/activation/ -run "TestPhase4_75_ArchiveRestore" -count=1
```

3. **Implement.**

   a. Add `RestoreArchivedEdgesTransitive` and `ArchiveBloomMayContain` to the `ActivationStore` interface in `internal/engine/activation/engine.go` (around the existing interface definition). Also update any mocks of `ActivationStore` used in activation tests.

   b. Add `phase4_75ArchiveRestore` method in `internal/engine/activation/engine.go`:

```go
// phase4_75ArchiveRestore checks the Bloom filter for archived edges among
// the fused candidate IDs and lazily restores them before BFS traversal.
func (e *ActivationEngine) phase4_75ArchiveRestore(ctx context.Context, ws [8]byte, candidates []fusedCandidate) {
	for _, c := range candidates {
		if !e.store.ArchiveBloomMayContain(c.id) {
			continue
		}
		// Restore top-10 direct + top-5 transitive.
		_, _ = e.store.RestoreArchivedEdgesTransitive(ctx, ws, c.id, 10, 5)
	}
}
```

   c. Insert the call in `Run()` between Phase 4.5 and Phase 5 (around line 377):

```go
	// Phase 4.75: Lazy archive restore — check Bloom filter, restore dormant edges.
	e.phase4_75ArchiveRestore(ctx, ws, fused)
```

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/engine/activation/ -count=1 -short
```

5. **Commit.**

```bash
git add internal/engine/activation/engine.go internal/engine/activation/activation_test.go
git commit -m "feat(activation): insert archive restore phase between Phase 4.5 and Phase 5"
```

---

## Task 9: `restored_at` in API response

Add `RestoredAt` to `AssociationItem` in the REST transport layer, populated from the 30-byte value when non-zero.

### Files

| File | Action |
|------|--------|
| `internal/transport/rest/types.go` | Add `RestoredAt` field to `AssociationItem` |
| `internal/transport/rest/engine_adapter.go` | Populate `RestoredAt` from decoded value |
| `internal/storage/types.go` | Add `RestoredAt` field to `Association` struct |
| `internal/storage/association.go` | Populate `RestoredAt` when decoding associations |

### Steps

1. **Failing test.** Add test that verifies `restored_at` appears in JSON when non-zero and is omitted when zero. Add in a new file or existing test:

```go
// In internal/transport/rest/ test file:
func TestAssociationItem_RestoredAt_OmitEmpty(t *testing.T) {
	item := AssociationItem{
		TargetID: "test-id",
		Weight:   0.5,
	}
	data, _ := json.Marshal(item)
	if strings.Contains(string(data), "restored_at") {
		t.Error("restored_at should be omitted when zero")
	}

	item.RestoredAt = 1709568000
	data, _ = json.Marshal(item)
	if !strings.Contains(string(data), "restored_at") {
		t.Error("restored_at should be present when non-zero")
	}
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/transport/rest/ -run "TestAssociationItem_RestoredAt" -count=1
```

3. **Implement.**

   a. In `internal/transport/rest/types.go`, add to `AssociationItem`:

```go
	RestoredAt int64 `json:"restored_at,omitempty"`
```

   b. In `internal/storage/types.go`, add to `Association`:

```go
	RestoredAt int32 // Unix seconds; 0 = never restored
```

   c. In `internal/storage/association.go`, wherever `Association` structs are built from `decodeAssocValue` results (in `GetAssociations` and `associationsForOne`), populate `RestoredAt` from the 7th return value.

   d. In `internal/transport/rest/engine_adapter.go` (around line 177), populate:

```go
	links[i] = AssociationItem{
		TargetID:          a.TargetID.String(),
		RelType:           uint16(a.RelType),
		Weight:            a.Weight,
		CoActivationCount: a.CoActivationCount,
		RestoredAt:        int64(a.RestoredAt),
	}
```

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/transport/rest/ -run "TestAssociationItem_RestoredAt" -count=1
```

5. **Commit.**

```bash
git add internal/transport/rest/types.go internal/transport/rest/engine_adapter.go internal/storage/types.go internal/storage/association.go
git commit -m "feat(api): add restored_at to AssociationItem response (omitted when zero)"
```

---

## Task 10: `restoredAt` clearing logic

Clear `restoredAt` when an edge re-establishes itself: 3+ co-activations post-restore OR weight exceeds `restoreWeight * 1.5`.

### Files

| File | Action |
|------|--------|
| `internal/storage/association.go` | Modify `UpdateAssocWeight` and `UpdateAssocWeightBatch` to clear `restoredAt` when conditions met |
| `internal/storage/association_test.go` | Add test for clearing |

### Steps

1. **Failing test.** Add in `internal/storage/association_test.go`:

```go
func TestRestoredAt_ClearedAfterReestablishment(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("restored-clear")

	src := NewULID()
	dst := NewULID()

	// Write a restored edge (simulate via archive value written to live keys).
	now := int32(time.Now().Unix())
	restoreWeight := float32(0.25)
	val := encodeArchiveValue(RelSupports, 0.9, time.Now().Add(-72*time.Hour), now, 1.0, 5, now)
	fwdKey := keys.AssocFwdKey(ws, [16]byte(src), restoreWeight, [16]byte(dst))
	store.db.Set(fwdKey, val[:], nil)
	revKey := keys.AssocRevKey(ws, [16]byte(dst), restoreWeight, [16]byte(src))
	store.db.Set(revKey, val[:], nil)
	var wiBuf [4]byte
	binary.BigEndian.PutUint32(wiBuf[:], math.Float32bits(restoreWeight))
	store.db.Set(keys.AssocWeightIndexKey(ws, [16]byte(src), [16]byte(dst)), wiBuf[:], nil)

	// Update weight 3 times (3 co-activations post-restore).
	for i := 0; i < 3; i++ {
		if err := store.UpdateAssocWeight(ctx, ws, src, dst, restoreWeight+float32(i)*0.01, 1); err != nil {
			t.Fatalf("UpdateAssocWeight[%d]: %v", i, err)
		}
	}

	// Read back and verify restoredAt is cleared.
	_, _, _, _, _, _, restoredAt := store.getAssocValueFull(ws, src, dst)
	if restoredAt != 0 {
		t.Errorf("restoredAt should be cleared after 3 co-activations, got %v", restoredAt)
	}
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestRestoredAt_ClearedAfterReestablishment" -count=1
```

3. **Implement.**

   a. Consider extending the existing unexported `getAssocValue` helper (in `association.go`) to return `restoredAt` instead of adding a new `getAssocValueFull` function, to avoid duplication. If a new function is preferred for clarity, add `getAssocValueFull` that returns all 7 decoded fields including `restoredAt`:

```go
func (ps *PebbleStore) getAssocValueFull(wsPrefix [8]byte, a, b ULID) (RelType, float32, time.Time, int32, float32, uint32, int32) {
	w, _ := ps.GetAssocWeight(context.Background(), wsPrefix, a, b)
	if w <= 0 {
		return 0, 1.0, time.Time{}, 0, 0, 0, 0
	}
	fwdKey := keys.AssocFwdKey(wsPrefix, [16]byte(a), w, [16]byte(b))
	val, err := Get(ps.db, fwdKey)
	if err != nil || val == nil {
		return 0, 1.0, time.Time{}, 0, 0, 0, 0
	}
	return decodeAssocValue(val)
}
```

   b. In `UpdateAssocWeight`, after reading existing metadata and computing `newCoAct`, add clearing logic:

```go
	// Clear restoredAt if edge has re-established itself.
	restoredAt := existingRestoredAt
	if restoredAt != 0 {
		restoreWeight := existingPeak * 0.25
		if newCoAct >= existingCoAct+3 || weight > restoreWeight*1.5 {
			restoredAt = 0
		}
	}
```

   Then use `encodeArchiveValue` (30-byte) instead of `encodeAssocValue` (26-byte) for the value. OR: only use the 30-byte encoder when `restoredAt != 0`, and use the standard 26-byte encoder otherwise. This keeps backward compatibility.

   c. Apply the same logic in `UpdateAssocWeightBatch`.

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestRestoredAt_ClearedAfterReestablishment" -count=1
```

5. **Commit.**

```bash
git add internal/storage/association.go internal/storage/association_test.go
git commit -m "feat(storage): clear restoredAt after 3+ co-activations or weight exceeds 1.5x restore weight"
```

---

## Task 11: GC pass for archived edges

Add a weekly background pass over `0x25` that true-prunes edges meeting all four conditions: `peakWeight < 0.15 AND coActivationCount < 3 AND daysSinceLastActivation > 1095 AND restoredAt == 0`.

### Files

| File | Action |
|------|--------|
| `internal/storage/archive_gc.go` | New file: `GCArchivedEdges` method |
| `internal/storage/archive_gc_test.go` | New file: test GC prunes qualifying edges |
| `internal/engine/engine.go` | Wire GC into the background worker loop (weekly) |

### Steps

1. **Failing test.** Create `internal/storage/archive_gc_test.go`:

```go
package storage

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

func TestGCArchivedEdges_PrunesQualifyingEdges(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("gc-test")

	src := NewULID()
	dstPrune := NewULID()
	dstKeep := NewULID()

	// dstPrune: qualifies for GC (peakWeight < 0.15, coAct < 3, >3 years old, never restored)
	fourYearsAgo := int32(time.Now().Add(-4 * 365 * 24 * time.Hour).Unix())
	arcPrune := encodeArchiveValue(RelSupports, 0.5, time.Now().Add(-4*365*24*time.Hour), fourYearsAgo, 0.10, 2, 0)
	store.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dstPrune)), arcPrune[:], nil)

	// dstKeep: does NOT qualify (peakWeight > 0.15)
	arcKeep := encodeArchiveValue(RelSupports, 0.9, time.Now().Add(-4*365*24*time.Hour), fourYearsAgo, 0.50, 10, 0)
	store.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dstKeep)), arcKeep[:], nil)

	store.archiveBloom.Add(src)

	pruned, err := store.GCArchivedEdges(ctx, ws)
	if err != nil {
		t.Fatalf("GCArchivedEdges: %v", err)
	}
	if pruned != 1 {
		t.Errorf("expected 1 pruned, got %d", pruned)
	}

	// dstPrune should be gone.
	val, _ := Get(store.db, keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dstPrune)))
	if val != nil {
		t.Error("dstPrune should have been GC'd")
	}

	// dstKeep should still exist.
	val, _ = Get(store.db, keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dstKeep)))
	if val == nil {
		t.Error("dstKeep should still exist")
	}
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestGCArchivedEdges" -count=1
```

3. **Implement.** Create `internal/storage/archive_gc.go`:

```go
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// GCArchivedEdges scans all archived edges for the given vault and deletes
// those that meet ALL four conditions:
//   - peakWeight < 0.15
//   - coActivationCount < 3
//   - daysSinceLastActivation > 1095 (3 years)
//   - restoredAt == 0 (never restored and reestablished)
//
// Returns the number of pruned entries.
func (ps *PebbleStore) GCArchivedEdges(ctx context.Context, wsPrefix [8]byte) (int, error) {
	lower := keys.ArchiveAssocRangeStart(wsPrefix)
	upper := keys.ArchiveAssocRangeEnd(wsPrefix)

	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
	})
	if err != nil {
		return 0, fmt.Errorf("archive GC iterator: %w", err)
	}
	defer iter.Close()

	type gcEntry struct {
		key []byte
	}
	var toDelete []gcEntry

	const daysThreshold = 1095 // 3 years

	for iter.First(); iter.Valid(); iter.Next() {
		if ctx.Err() != nil {
			break
		}
		val := iter.Value()
		_, _, _, lastActivated, peakWeight, coActivationCount, restoredAt := decodeAssocValue(val)

		if peakWeight >= 0.15 {
			continue
		}
		if coActivationCount >= 3 {
			continue
		}
		if restoredAt != 0 {
			continue
		}

		daysSinceLastAct := float64(0)
		if lastActivated > 0 {
			daysSinceLastAct = time.Since(time.Unix(int64(lastActivated), 0)).Hours() / 24
		}
		if daysSinceLastAct <= float64(daysThreshold) {
			continue
		}

		// All four conditions met — schedule for deletion.
		keyCopy := make([]byte, len(iter.Key()))
		copy(keyCopy, iter.Key())
		toDelete = append(toDelete, gcEntry{key: keyCopy})
	}
	if err := iter.Error(); err != nil {
		return 0, fmt.Errorf("archive GC scan: %w", err)
	}

	if len(toDelete) == 0 {
		return 0, nil
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	for _, e := range toDelete {
		batch.Delete(e.key, nil)
	}

	if err := batch.Commit(pebble.NoSync); err != nil {
		return 0, fmt.Errorf("archive GC commit: %w", err)
	}

	return len(toDelete), nil
}
```

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/storage/ -run "TestGCArchivedEdges" -count=1
```

5. **Commit.**

```bash
git add internal/storage/archive_gc.go internal/storage/archive_gc_test.go
git commit -m "feat(storage): weekly GC pass for archived edges (4-condition true-prune)"
```

---

## Task 12: Replication -- verify/extend CognitiveForwarder for archive/restore

Verify that the existing `CognitiveForwarder` and `CognitiveSideEffect` MBP protocol supports archive/restore operations. Extend if needed.

### Files

| File | Action |
|------|--------|
| `internal/transport/mbp/cluster_frames.go` | Add `ArchivedEdges` and `RestoredEdges` fields to `CognitiveSideEffect` (if not already present) |
| `internal/transport/mbp/cluster_frames_test.go` | Add round-trip test for new fields |
| `internal/engine/engine.go` | Forward archive/restore effects via `CognitiveForwarder` |

### Steps

1. **Failing test.** Add in `internal/transport/mbp/cluster_frames_test.go`. Add `"github.com/vmihailenco/msgpack/v5"` to the test file's imports if not already present.

```go
func TestCognitiveSideEffect_ArchiveRestore_RoundTrip(t *testing.T) {
	orig := CognitiveSideEffect{
		QueryID:      "test-query",
		OriginNodeID: "node-1",
		Timestamp:    time.Now().UnixNano(),
		ArchivedEdges: []EdgeRef{
			{Src: [16]byte{1}, Dst: [16]byte{2}},
		},
		RestoredEdges: []EdgeRef{
			{Src: [16]byte{3}, Dst: [16]byte{4}},
		},
	}

	data, err := msgpack.Marshal(&orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got CognitiveSideEffect
	if err = msgpack.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got.ArchivedEdges) != 1 {
		t.Errorf("ArchivedEdges: got %d, want 1", len(got.ArchivedEdges))
	}
	if len(got.RestoredEdges) != 1 {
		t.Errorf("RestoredEdges: got %d, want 1", len(got.RestoredEdges))
	}
}
```

2. **Run to fail.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/transport/mbp/ -run "TestCognitiveSideEffect_ArchiveRestore" -count=1
```

3. **Implement.**

   a. Add `EdgeRef` type and new fields to `CognitiveSideEffect` in `internal/transport/mbp/cluster_frames.go`:

```go
// EdgeRef is a (src, dst) pair identifying an association edge.
type EdgeRef struct {
	Src [16]byte `msgpack:"src"`
	Dst [16]byte `msgpack:"dst"`
}

// Add to CognitiveSideEffect:
	ArchivedEdges []EdgeRef `msgpack:"archived_edges,omitempty"`
	RestoredEdges []EdgeRef `msgpack:"restored_edges,omitempty"`
```

   b. Existing msgpack serialization should handle the new fields automatically (the codec is struct-tag-driven). Verify `MarshalMsgpack`/`UnmarshalMsgpack` methods work with the additional fields.

   c. In `internal/engine/engine.go`, at the points where archive/restore writes happen, forward the effects via the coordinator if non-nil. This may be deferred to a follow-up if the archive/restore writes happen in the storage layer (not the engine layer). Add a TODO comment if the forwarding cannot be wired yet.

4. **Run to pass.**

```bash
cd /Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/feat-association-archiving && go test ./internal/transport/mbp/ -run "TestCognitiveSideEffect_ArchiveRestore" -count=1
```

5. **Commit.**

```bash
git add internal/transport/mbp/cluster_frames.go internal/transport/mbp/cluster_frames_test.go
git commit -m "feat(mbp): extend CognitiveSideEffect with ArchivedEdges and RestoredEdges fields"
```
