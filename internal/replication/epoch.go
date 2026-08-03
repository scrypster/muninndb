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

// ForceSet unconditionally sets the epoch to the given value.
// Used when accepting a CortexClaim with a higher epoch from another node.
// Only updates if newEpoch > current (never go backwards).
func (s *EpochStore) ForceSet(newEpoch uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if newEpoch <= s.current {
		slog.Debug("epoch store: ignoring stale ForceSet", "current", s.current, "provided", newEpoch)
		return nil // never go backwards
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
