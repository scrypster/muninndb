package replication

import (
	"encoding/binary"
	"log/slog"
	"sync"

	"github.com/cockroachdb/pebble"
)

// EpochStore persists the cluster election epoch to Pebble.
// Every time this node participates in an election, the epoch is incremented
// and persisted before any votes are sent. This ensures a restarted node
// never proposes an epoch it has already seen.
type EpochStore struct {
	db      *pebble.DB
	mu      sync.Mutex
	current uint64
}

// NewEpochStore creates an EpochStore, loading the current epoch from Pebble.
// If no epoch is stored (first run), starts at 0.
func NewEpochStore(db *pebble.DB) (*EpochStore, error) {
	s := &EpochStore{db: db}

	val, closer, err := db.Get(clusterEpochKey())
	if err != nil && err != pebble.ErrNotFound {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}
	if err == nil && len(val) >= 8 {
		s.current = binary.BigEndian.Uint64(val)
	}

	return s, nil
}

// Load returns the current epoch (in-memory cached value).
func (s *EpochStore) Load() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// CompareAndSet atomically sets the epoch to newEpoch if the current epoch
// equals expected. Returns true if the update succeeded, false if the current
// epoch no longer matches expected (concurrent update).
// Persists to Pebble on success.
func (s *EpochStore) CompareAndSet(expected, newEpoch uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.current != expected {
		return false, nil
	}

	if err := s.persist(newEpoch); err != nil {
		return false, err
	}

	s.current = newEpoch
	return true, nil
}

// Advance moves the epoch forward and reports whether it actually moved.
//
// The epoch is monotonic by construction: a value less than or equal to the
// current one is refused, never persisted, and reported as advanced=false.
//
// The bool return is load-bearing and is why this method replaced the former
// ForceSet (#631). ForceSet also refused to go backwards, but it signalled the
// refusal only by returning nil — indistinguishable from success — so the join
// path adopted a rebuilt Cortex's lower epoch as if it had worked and diverged
// silently. A caller that must not continue past a regression cannot ignore
// this bool without saying so in code (principle #3: make the bad state
// unrepresentable rather than documented).
//
// A backwards epoch is NOT always illegitimate: rebuilding or restoring the
// Cortex from a backup genuinely lowers the cluster's epoch. That case is
// legal only through AdoptForSnapshot, which names its own precondition.
func (s *EpochStore) Advance(newEpoch uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if newEpoch <= s.current {
		slog.Debug("epoch store: refusing non-advancing epoch", "current", s.current, "provided", newEpoch)
		return false, nil
	}

	if err := s.persist(newEpoch); err != nil {
		return false, err
	}

	s.current = newEpoch
	return true, nil
}

// AdoptForSnapshot sets the epoch to newEpoch even when that moves it BACKWARDS.
// It is the only backwards path in the type, and its name is its precondition:
// the caller must have just replaced this node's entire local state with a full
// snapshot from the node whose epoch this is.
//
// This exists because WipeForResnapshot deliberately preserves cluster_epoch
// (#531 PR3) — correct when re-snapshotting from a Cortex that is AHEAD, wrong
// when the Cortex was rebuilt from scratch, which leaves the node holding a
// fencing token from a cluster history that no longer exists (#631).
//
// Every other caller uses Advance. Splitting the two means a future edit cannot
// regress an epoch by accident; it has to call a method whose name it would have
// to justify in review.
func (s *EpochStore) AdoptForSnapshot(newEpoch uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if newEpoch == s.current {
		return nil
	}
	if newEpoch < s.current {
		slog.Warn("epoch store: adopting a LOWER epoch after a full resnapshot — the Cortex was rebuilt or restored",
			"previous", s.current, "adopted", newEpoch)
	}
	if err := s.persist(newEpoch); err != nil {
		return err
	}
	s.current = newEpoch
	return nil
}

// persist writes the epoch to Pebble with Sync for crash safety.
func (s *EpochStore) persist(epoch uint64) error {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, epoch)
	return s.db.Set(clusterEpochKey(), buf, pebble.Sync)
}

// PersistRole writes the node role to Pebble with Sync for crash safety.
// Call this BEFORE broadcasting CortexClaim during handoff promotion so that a
// crash between broadcasting and completing in-memory promotion is recoverable.
// The only meaningful value is "cortex"; write "" to clear.
func (s *EpochStore) PersistRole(role string) error {
	return s.db.Set(nodeRoleKey(), []byte(role), pebble.Sync)
}

// LoadRole reads the last persisted node role from Pebble.
// Returns "" if no role has been persisted (fresh start or cleared).
func (s *EpochStore) LoadRole() (string, error) {
	val, closer, err := s.db.Get(nodeRoleKey())
	if err != nil {
		if err == pebble.ErrNotFound {
			return "", nil
		}
		return "", err
	}
	defer closer.Close()
	return string(val), nil
}
