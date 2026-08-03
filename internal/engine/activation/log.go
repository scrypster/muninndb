package activation

import (
	"sort"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

const vaultLogCap = 1000 // per-vault ring buffer capacity

// vaultLog is a per-vault ring buffer of activation events.
type vaultLog struct {
	mu      sync.RWMutex
	entries [vaultLogCap]LogEntry
	head    int
	count   int
}

// ActivationLog holds independent ring buffers per vault.
// This eliminates inter-vault pollution and removes the O(200) linear scan
// that degraded Hebbian boost from +45% at 15k to +3% at 20k memories.
type ActivationLog struct {
	vaults sync.Map // uint32 → *vaultLog
}

// LogEntry records one activation event.
type LogEntry struct {
	VaultID   uint32
	At        time.Time
	EngramIDs []storage.ULID
	Scores    []float64
}

func (l *ActivationLog) getOrCreate(vaultID uint32) *vaultLog {
	v, _ := l.vaults.LoadOrStore(vaultID, &vaultLog{})
	return v.(*vaultLog)
}

// ResetVault discards all recorded activation events for vaultID. Test-only:
// production callers never call this — phase4HebbianBoost's cross-activation
// priming ("was this candidate's associated target recently activated") is
// intentional, unbounded spreading activation across a vault's real timeline.
// A scripted test harness that fires many calls back-to-back to model
// SEPARATE, independently-dedup'd agent sessions (see prospective_harness_test.go)
// compresses what would be minutes/hours of real elapsed time into
// milliseconds, so the recency-weighted decay (recencyTau=3600s) never
// meaningfully decays between "sessions" — every prior scripted call's results stay
// maximally "recently activated" for the rest of the run, letting an armed
// intention's own engram silently accumulate Hebbian boost from unrelated
// earlier calls and outscore its own corroborator. See ActivationEngine.ResetLog.
func (l *ActivationLog) ResetVault(vaultID uint32) {
	l.vaults.Delete(vaultID)
}

// Record appends a new activation event to the vault-scoped ring buffer.
func (l *ActivationLog) Record(entry LogEntry) {
	vl := l.getOrCreate(entry.VaultID)
	vl.mu.Lock()
	vl.entries[vl.head] = entry
	vl.head = (vl.head + 1) % vaultLogCap
	if vl.count < vaultLogCap {
		vl.count++
	}
	vl.mu.Unlock()
}

// Recent returns the last n entries across all vaults, newest first.
// Used for monitoring/debugging. For Hebbian scoring use RecentForVault.
func (l *ActivationLog) Recent(n int) []LogEntry {
	var all []LogEntry
	l.vaults.Range(func(_, v any) bool {
		vl := v.(*vaultLog)
		vl.mu.RLock()
		pos := (vl.head - 1 + vaultLogCap) % vaultLogCap
		for i := 0; i < vl.count; i++ {
			all = append(all, vl.entries[pos])
			pos = (pos - 1 + vaultLogCap) % vaultLogCap
		}
		vl.mu.RUnlock()
		return true
	})
	sort.Slice(all, func(i, j int) bool {
		return all[i].At.After(all[j].At)
	})
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// RecentForVault returns the last n entries for the given vault, newest first.
// Direct ring walk with no inter-vault filtering — O(n) on vault's own ring only.
func (l *ActivationLog) RecentForVault(vaultID uint32, n int) []LogEntry {
	v, ok := l.vaults.Load(vaultID)
	if !ok {
		return nil
	}
	vl := v.(*vaultLog)
	vl.mu.RLock()
	defer vl.mu.RUnlock()
	result := make([]LogEntry, 0, n)
	pos := (vl.head - 1 + vaultLogCap) % vaultLogCap
	for i := 0; i < vl.count && len(result) < n; i++ {
		result = append(result, vl.entries[pos])
		pos = (pos - 1 + vaultLogCap) % vaultLogCap
	}
	return result
}
