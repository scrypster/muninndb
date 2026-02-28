package coherence

import (
	"math"
	"sync"
	"sync/atomic"
)

// VaultCounters holds incremental statistics for a single vault.
// All fields use atomic operations for lock-free concurrent updates.
type VaultCounters struct {
	TotalEngrams   atomic.Int64 // total engrams written to vault
	OrphanCount    atomic.Int64 // engrams with 0 associations
	Contradictions atomic.Int64 // active contradiction markers
	RefinesCount   atomic.Int64 // engrams with >=1 REFINES link (as source)
	// Welford online variance for confidence values
	// Fixed-point: store actual*1e6 to preserve precision
	ConfidenceN     atomic.Int64 // count of confidence updates
	ConfidenceSum   atomic.Int64 // sum(confidence * 1e6)
	ConfidenceSumSq atomic.Int64 // sum(confidence^2 * 1e6)
}

// Serialize returns the current counter values as a [7]int64 array for persistence.
// The order is: TotalEngrams, OrphanCount, Contradictions, RefinesCount,
// ConfidenceN, ConfidenceSum, ConfidenceSumSq.
func (vc *VaultCounters) Serialize() [7]int64 {
	return [7]int64{
		vc.TotalEngrams.Load(),
		vc.OrphanCount.Load(),
		vc.Contradictions.Load(),
		vc.RefinesCount.Load(),
		vc.ConfidenceN.Load(),
		vc.ConfidenceSum.Load(),
		vc.ConfidenceSumSq.Load(),
	}
}

// Restore sets counter values from a previously serialized [7]int64 array.
func (vc *VaultCounters) Restore(data [7]int64) {
	vc.TotalEngrams.Store(data[0])
	vc.OrphanCount.Store(data[1])
	vc.Contradictions.Store(data[2])
	vc.RefinesCount.Store(data[3])
	vc.ConfidenceN.Store(data[4])
	vc.ConfidenceSum.Store(data[5])
	vc.ConfidenceSumSq.Store(data[6])
}

// RecordWrite records a new engram being written.
// New engrams start as orphans (0 associations).
// confidence is in [0, 1].
func (c *VaultCounters) RecordWrite(confidence float32) {
	c.TotalEngrams.Add(1)
	c.OrphanCount.Add(1)
	c.recordConfidence(confidence)
}

// RecordLinkCreated records a new association being created from sourceID.
// isFirstLink: true if sourceID previously had 0 associations (was an orphan).
// isRefines: true if the link type is REFINES.
func (c *VaultCounters) RecordLinkCreated(isFirstLink, isRefines bool) {
	if isFirstLink {
		c.OrphanCount.Add(-1)
	}
	if isRefines {
		c.RefinesCount.Add(1)
	}
}

// RecordLinkDeleted records an association being removed.
// wasLastLink: true if the source now has 0 associations (became an orphan).
// isRefines: true if the link type was REFINES.
func (c *VaultCounters) RecordLinkDeleted(wasLastLink, isRefines bool) {
	if wasLastLink {
		c.OrphanCount.Add(1)
	}
	if isRefines {
		c.RefinesCount.Add(-1)
	}
}

// RecordContradictionSet records a new contradiction marker being set.
func (c *VaultCounters) RecordContradictionSet() {
	c.Contradictions.Add(1)
}

// RecordContradictionResolved records a contradiction being resolved.
func (c *VaultCounters) RecordContradictionResolved() {
	c.Contradictions.Add(-1)
}

// RecordConfidenceUpdate updates the running variance with a new confidence value.
// Call when an engram's confidence changes (decay, Hebbian boost).
func (c *VaultCounters) RecordConfidenceUpdate(newConfidence float32) {
	c.recordConfidence(newConfidence)
}

func (c *VaultCounters) recordConfidence(confidence float32) {
	v := int64(float64(confidence) * 1e6)
	vSq := int64(float64(confidence) * float64(confidence) * 1e6)
	c.ConfidenceN.Add(1)
	c.ConfidenceSum.Add(v)
	c.ConfidenceSumSq.Add(vSq)
}

// Variance computes the population variance of confidence values.
// Returns 0 if fewer than 2 samples.
func (c *VaultCounters) Variance() float64 {
	n := c.ConfidenceN.Load()
	if n < 2 {
		return 0
	}
	sum := float64(c.ConfidenceSum.Load()) / 1e6
	sumSq := float64(c.ConfidenceSumSq.Load()) / 1e6
	mean := sum / float64(n)
	variance := sumSq/float64(n) - mean*mean
	if variance < 0 {
		variance = 0 // floating point rounding protection
	}
	return variance
}

// Score computes the vault coherence score in [0.0, 1.0].
// Higher is better (more coherent memory).
//
// Formula:
//
//	coherence = 1.0 - (orphanRatio*0.3 + contradictionDensity*0.3 +
//	                   clamp01(variance)*0.2 + duplicationPressure*0.2)
//
// Returns 1.0 for empty vaults.
func (c *VaultCounters) Score() float64 {
	n := float64(c.TotalEngrams.Load())
	if n == 0 {
		return 1.0
	}
	orphanRatio := clamp01(float64(c.OrphanCount.Load()) / n)
	contradictionDensity := clamp01(float64(c.Contradictions.Load()) / n)
	duplicationPressure := clamp01(float64(c.RefinesCount.Load()) / n)
	temporalVariance := clamp01(c.Variance())

	penalty := orphanRatio*0.3 + contradictionDensity*0.3 +
		temporalVariance*0.2 + duplicationPressure*0.2
	return math.Max(0, 1.0-penalty)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// Result holds a vault's coherence score and sub-metrics.
type Result struct {
	VaultName            string
	Score                float64
	OrphanRatio          float64
	ContradictionDensity float64
	DuplicationPressure  float64
	TemporalVariance        float64
	TotalEngrams         int64
}

// Snapshot returns a Result with all current sub-metrics and the overall score.
func (c *VaultCounters) Snapshot(vaultName string) Result {
	n := float64(c.TotalEngrams.Load())
	if n == 0 {
		return Result{VaultName: vaultName, Score: 1.0, TotalEngrams: 0}
	}
	orphanRatio := clamp01(float64(c.OrphanCount.Load()) / n)
	contradictionDensity := clamp01(float64(c.Contradictions.Load()) / n)
	duplicationPressure := clamp01(float64(c.RefinesCount.Load()) / n)
	temporalVariance := clamp01(c.Variance())
	penalty := orphanRatio*0.3 + contradictionDensity*0.3 +
		temporalVariance*0.2 + duplicationPressure*0.2
	return Result{
		VaultName:            vaultName,
		Score:                math.Max(0, 1.0-penalty),
		OrphanRatio:          orphanRatio,
		ContradictionDensity: contradictionDensity,
		DuplicationPressure:  duplicationPressure,
		TemporalVariance:        temporalVariance,
		TotalEngrams:         int64(n),
	}
}

// Registry maintains VaultCounters for all vaults.
type Registry struct {
	mu     sync.RWMutex
	vaults map[string]*VaultCounters
}

// NewRegistry creates a new coherence Registry.
func NewRegistry() *Registry {
	return &Registry{
		vaults: make(map[string]*VaultCounters),
	}
}

// GetOrCreate returns the VaultCounters for the named vault, creating if needed.
func (r *Registry) GetOrCreate(vaultName string) *VaultCounters {
	r.mu.RLock()
	c, ok := r.vaults[vaultName]
	r.mu.RUnlock()
	if ok {
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok = r.vaults[vaultName]; ok {
		return c
	}
	c = &VaultCounters{}
	r.vaults[vaultName] = c
	return c
}

// DeleteVault removes the vault counter entry for the named vault.
// After this call, subsequent GetOrCreate calls will return a fresh zeroed
// counter for the same vault name. Safe for concurrent use.
func (r *Registry) DeleteVault(name string) {
	r.mu.Lock()
	delete(r.vaults, name)
	r.mu.Unlock()
}

// RenameVault moves the VaultCounters entry from oldName to newName.
// No-op if oldName is not tracked.
func (r *Registry) RenameVault(oldName, newName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.vaults[oldName]
	if !ok {
		return
	}
	r.vaults[newName] = c
	delete(r.vaults, oldName)
}

// SerializeAll returns a snapshot of all vault counters as a map of vault name → [7]int64.
func (r *Registry) SerializeAll() map[string][7]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string][7]int64, len(r.vaults))
	for name, vc := range r.vaults {
		out[name] = vc.Serialize()
	}
	return out
}

// RestoreVault sets the counter values for the named vault.
// Creates the vault entry if it does not exist.
func (r *Registry) RestoreVault(name string, data [7]int64) {
	vc := r.GetOrCreate(name)
	vc.Restore(data)
}

// Snapshots returns Result for all vaults.
func (r *Registry) Snapshots() []Result {
	r.mu.RLock()
	names := make([]string, 0, len(r.vaults))
	for name := range r.vaults {
		names = append(names, name)
	}
	r.mu.RUnlock()

	results := make([]Result, 0, len(names))
	for _, name := range names {
		r.mu.RLock()
		c := r.vaults[name]
		r.mu.RUnlock()
		results = append(results, c.Snapshot(name))
	}
	return results
}
