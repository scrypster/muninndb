package cognitive

import "sync"

// maxLTPEntries caps the number of entries in the ltpState counts and potentiated
// maps. When exceeded, new entries are silently dropped rather than growing unbounded.
// Existing tracked pairs continue to accumulate counts normally; only brand-new pairs
// are rejected. 10k entries is generous for any realistic single-session workload.
const maxLTPEntries = 10000

// LTPConfig configures Long-Term Potentiation behavior for the Hebbian worker.
// When nil, LTP is disabled and all behavior is unchanged (backward compatible).
//
// Current scope: weight floor enforcement for potentiated associations.
// Decay resistance (reduced decay rate for potentiated edges) is planned for a
// follow-up change that integrates with the association decay system.
type LTPConfig struct {
	// Threshold is the co-activation count at which an association becomes potentiated.
	// 0 = disabled.
	Threshold int
	// WeightFloor is the minimum weight for potentiated associations.
	// The Hebbian worker enforces this floor during weight updates.
	// 0 = disabled (no floor enforcement).
	WeightFloor float32
}

// ltpState is session-scoped: potentiation status is not hydrated from storage
// on restart. After a restart, associations must re-earn potentiation by
// accumulating threshold co-activations in the new session. This is intentional —
// session-local tracking avoids extra storage reads in the hot path.
//
// The authoritative co-activation count is in the storage layer (CoActivationCount);
// this is a session-local cache for fast lookups during processBatch.
// The underlying association weights ARE persisted — LTP is a performance
// optimization (weight floor enforcement), not a correctness requirement.
type ltpState struct {
	mu          sync.RWMutex
	potentiated map[ltpKey]struct{} // set of potentiated pairs
	counts      map[ltpKey]uint32   // session-local co-activation count tracker
}

// ltpKey is a composite key of workspace + canonical pair.
type ltpKey struct {
	ws   [8]byte
	pair pairKey
}

func newLTPState() *ltpState {
	return &ltpState{
		potentiated: make(map[ltpKey]struct{}),
		counts:      make(map[ltpKey]uint32),
	}
}

// addCount increments the session-local count for a pair and returns whether
// the pair has become potentiated (count crossed the threshold in this call).
func (s *ltpState) addCount(ws [8]byte, pair pairKey, delta uint32, threshold int) bool {
	if threshold <= 0 {
		return false
	}
	key := ltpKey{ws: ws, pair: pair}
	s.mu.Lock()
	defer s.mu.Unlock()

	old, exists := s.counts[key]
	// Cap enforcement: if this is a brand-new pair and we are at capacity, skip it.
	// Existing pairs continue to accumulate counts normally.
	if !exists && len(s.counts) >= maxLTPEntries {
		return false
	}
	newCount := old + delta
	// Saturation
	if newCount < old {
		newCount = ^uint32(0)
	}
	s.counts[key] = newCount

	if _, already := s.potentiated[key]; already {
		return false // was already potentiated
	}
	if newCount >= uint32(threshold) {
		s.potentiated[key] = struct{}{}
		return true // newly potentiated
	}
	return false
}

// isPotentiated checks if a pair is potentiated.
func (s *ltpState) isPotentiated(ws [8]byte, pair pairKey) bool {
	key := ltpKey{ws: ws, pair: pair}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.potentiated[key]
	return ok
}
