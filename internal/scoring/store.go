package scoring

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// lastUpdateLRUSize is the maximum number of (vault, engram) pairs tracked for
// RecordFeedback throttling. Entries are evicted in LRU order once the cache
// is full, preventing unbounded memory growth.
const lastUpdateLRUSize = 10_000

// Store persists VaultWeights to Pebble using the 0x18 key prefix.
type Store struct {
	db *pebble.DB
	mu sync.RWMutex
	// in-memory cache: avoid Pebble round-trip on every scoring call
	cache map[[8]byte]*VaultWeights
	// lastUpdateLRU tracks last update time per (vault, engram) pair to throttle
	// RecordFeedback. Bounded to lastUpdateLRUSize entries; thread-safe internally.
	lastUpdateLRU *lru.Cache[[24]byte, time.Time]
}

// NewStore creates a new scoring weight store.
func NewStore(db *pebble.DB) *Store {
	lruCache, _ := lru.New[[24]byte, time.Time](lastUpdateLRUSize)
	return &Store{
		db:            db,
		cache:         make(map[[8]byte]*VaultWeights),
		lastUpdateLRU: lruCache,
	}
}

// Get returns weights for a vault prefix. Returns DefaultWeights if not found.
func (s *Store) Get(ctx context.Context, ws [8]byte) (*VaultWeights, error) {
	s.mu.RLock()
	if cached, ok := s.cache[ws]; ok {
		s.mu.RUnlock()
		return cached, nil
	}
	s.mu.RUnlock()

	key := keys.VaultWeightsKey(ws)
	val, closer, err := s.db.Get(key)
	if err != nil {
		if err == pebble.ErrNotFound {
			// Return default weights
			s.mu.Lock()
			vw := &VaultWeights{
				VaultPrefix:  ws,
				Weights:      DefaultWeights(),
				LearningRate: 0.1,
				UpdatedAt:    time.Now(),
			}
			s.cache[ws] = vw
			s.mu.Unlock()
			return vw, nil
		}
		return nil, err
	}
	defer closer.Close()

	var vw VaultWeights
	if err := json.Unmarshal(val, &vw); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.cache[ws] = &vw
	s.mu.Unlock()

	return &vw, nil
}

// Save persists weights to Pebble.
func (s *Store) Save(ctx context.Context, vw *VaultWeights) error {
	data, err := json.Marshal(vw)
	if err != nil {
		return err
	}

	key := keys.VaultWeightsKey(vw.VaultPrefix)
	if err := s.db.Set(key, data, pebble.Sync); err != nil {
		return err
	}

	s.mu.Lock()
	s.cache[vw.VaultPrefix] = vw
	s.mu.Unlock()

	return nil
}

// RecordFeedback applies a feedback signal to the vault's weights and saves.
// Enforces max update frequency: skips if last update was < 30 minutes ago for this engram.
// Uses a best-effort approach — errors are logged but not returned.
//
// Pebble panics (rather than returning an error) when the DB is closed. Since
// Engine.Read fires this in a goroutine, teardown can race with the write.
// We recover and silently swallow closed-DB panics — the DB is already in an
// unrecoverable state and the feedback write is best-effort anyway.
func (s *Store) RecordFeedback(ctx context.Context, ws [8]byte, signal FeedbackSignal) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok && errors.Is(err, pebble.ErrClosed) {
				// Defense-in-depth: Engine.spawnFireAndForget + WaitGroup is the
				// primary guard. This recover() handles the 5s timeout edge case
				// and future call sites that bypass the spawn helper.
				return
			}
			panic(r) // unexpected — propagate
		}
	}()

	// Build tracking key: vault_prefix(8) + engram_id(16)
	var trackKey [24]byte
	copy(trackKey[:8], ws[:])
	copy(trackKey[8:24], signal.EngramID[:])

	now := signal.Timestamp
	lastUpdate, ok := s.lastUpdateLRU.Peek(trackKey)
	if ok && now.Sub(lastUpdate) < 30*time.Minute {
		return // throttle: skip this update
	}
	s.lastUpdateLRU.Add(trackKey, now)

	// Retrieve current weights (async best-effort)
	vw, err := s.Get(ctx, ws)
	if err != nil {
		return // best effort
	}

	// Copy before mutating. Get returns the cached pointer; concurrent
	// RecordFeedback calls from parallel Engine.Read goroutines would race
	// on the same VaultWeights struct without this copy.
	vwCopy := *vw
	vwCopy.Update(signal)

	// Persist and replace cache entry with the updated copy (async best-effort).
	if err := s.Save(ctx, &vwCopy); err != nil {
		slog.Warn("failed to persist feedback", "err", err)
	}
}

// InvalidateCache clears all cached weights (useful for testing or cache invalidation).
func (s *Store) InvalidateCache() {
	s.mu.Lock()
	s.cache = make(map[[8]byte]*VaultWeights)
	s.mu.Unlock()
	s.lastUpdateLRU.Purge()
}
