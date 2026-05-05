package replication

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cockroachdb/pebble"
)

// lastAppliedKey returns the Pebble key used to persist the lastApplied sequence number.
func lastAppliedKey() []byte {
	return []byte{0x19, 0x02, 'l', 'a', 's', 't', '_', 'a', 'p', 'p'}
}

// applierSyncInterval is the number of entries applied between periodic fsyncs.
// Pebble's WAL handles crash-safety for individual writes (pebble.NoSync); this
// periodic sync bounds the amount of data that can be lost after a crash.
const applierSyncInterval = 100

// Applier applies incoming replication entries to a local Pebble database.
// Used by replicas to consume entries from the primary.
type Applier struct {
	db           *pebble.DB
	lastApplied  uint64
	appliedSince uint64 // entries since last explicit sync
	mu           sync.Mutex
}

// NewApplier creates a new Applier for a Pebble database.
// It loads lastApplied from Pebble so a restarted replica resumes from where it left off.
func NewApplier(db *pebble.DB) *Applier {
	a := &Applier{db: db}
	val, closer, err := db.Get(lastAppliedKey())
	if err == nil && len(val) >= 8 {
		a.lastApplied = binary.BigEndian.Uint64(val)
	}
	if closer != nil {
		closer.Close()
	}
	return a
}

// Apply applies a single replication entry to the local database.
// Thread-safe. Skips entries with seq <= lastApplied (idempotent).
//
// All WALOp types are handled:
//   - OpSet, OpDelete, OpBatch: standard data writes
//   - OpCognitive: Hebbian/Decay/Confidence state updates (applied as key-value writes)
//   - OpIndex: FTS/HNSW index updates (applied as key-value writes)
//   - OpMeta: cluster metadata (applied as key-value writes)
//
// The Op field is metadata for filtering/routing; all operations persist to Pebble
// as key-value writes with identical semantics.
func (a *Applier) Apply(entry ReplicationEntry) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("applier panic: %v", r)
			slog.Error("applier: panic recovered", "panic", r)
		}
	}()

	a.mu.Lock()
	defer a.mu.Unlock()

	// Idempotent: skip already-applied entries
	if entry.Seq <= a.lastApplied {
		return nil
	}

	batch := a.db.NewBatch()
	defer batch.Close()

	switch entry.Op {
	case OpSet:
		batch.Set(entry.Key, entry.Value, nil)
	case OpDelete:
		batch.Delete(entry.Key, nil)
	case OpBatch:
		// entry.Value is a Pebble batch repr (from batch.Repr() on the primary).
		// Apply the repr atomically, then persist lastApplied in the outer batch.
		// SetRepr replaces the batch contents entirely; we must commit it as-is
		// (adding ops after SetRepr causes a batch-count inconsistency in Pebble).
		// The outer `batch` (empty, just created above) is used for the lastApplied
		// marker and is closed by its own defer.
		reprBatch := a.db.NewBatch()
		defer reprBatch.Close()
		if err := reprBatch.SetRepr(entry.Value); err != nil {
			return fmt.Errorf("apply batch repr at seq %d: %w", entry.Seq, err)
		}
		if err := reprBatch.Commit(pebble.NoSync); err != nil {
			return err
		}
		seqBuf := make([]byte, 8)
		binary.BigEndian.PutUint64(seqBuf, entry.Seq)
		markerBatch := a.db.NewBatch()
		defer markerBatch.Close()
		markerBatch.Set(lastAppliedKey(), seqBuf, nil)
		if err := markerBatch.Commit(pebble.NoSync); err != nil {
			return err
		}
		a.lastApplied = entry.Seq
		a.appliedSince++
		if a.appliedSince >= applierSyncInterval {
			if err := a.db.LogData(nil, pebble.Sync); err != nil {
				return err
			}
			a.appliedSince = 0
		}
		return nil
	case OpCognitive, OpIndex, OpMeta:
		// All cognitive, index, and metadata operations are applied as key-value writes.
		// The Op field is metadata for filtering; the persistence semantics are identical.
		batch.Set(entry.Key, entry.Value, nil)
	default:
		batch.Set(entry.Key, entry.Value, nil)
	}

	// Persist lastApplied atomically with the data write.
	seqBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(seqBuf, entry.Seq)
	batch.Set(lastAppliedKey(), seqBuf, nil)

	// Use NoSync per entry — Pebble's WAL provides crash-safety without an fsync
	// on every write. Syncing every entry would serialize on disk I/O, making
	// replication throughput proportional to fsync latency (~1 IOPS per entry).
	if err := batch.Commit(pebble.NoSync); err != nil {
		return err
	}

	a.lastApplied = entry.Seq
	a.appliedSince++

	// Periodic batch-level sync: issue one fsync every applierSyncInterval entries.
	// This bounds the crash-recovery window to at most applierSyncInterval entries
	// without paying fsync cost on every individual entry.
	if a.appliedSince >= applierSyncInterval {
		if err := a.db.LogData(nil, pebble.Sync); err != nil {
			return err
		}
		a.appliedSince = 0
	}

	return nil
}

// LastApplied returns the sequence number of the most recently applied entry.
// Thread-safe.
func (a *Applier) LastApplied() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastApplied
}

// IsLagging returns true if the replica's lastApplied is more than maxLag
// entries behind the primary's currentSeq. Used to enforce BoundedStaleness mode.
func (a *Applier) IsLagging(primarySeq uint64, maxLag uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.lastApplied >= primarySeq {
		return false
	}

	lag := primarySeq - a.lastApplied
	return lag > maxLag
}
