package replication

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cockroachdb/pebble"
)

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
	// invalidate, when non-nil, is called with every user key an applied entry
	// touched, AFTER the write is committed to Pebble (#869). The applier holds
	// a bare *pebble.DB, so a replicated mutation lands on disk underneath the
	// storage layer's in-memory caches; a follower that had already cached an
	// engram kept serving the stale copy — soft-deletes, evolve supersession,
	// trust changes, every in-place mutation — until the process restarted.
	// The callback is how the storage layer hears about applied keys without
	// the applier importing storage (which would be an import cycle); it is
	// wired at server construction, mirroring how the storage layer's
	// RepLogAppend hook points the other way.
	//
	// Invalidation deliberately runs after Commit: invalidating first would
	// let a concurrent read re-load the OLD value from Pebble and re-cache it
	// just before the commit lands, resurrecting exactly the staleness this
	// removes. After Commit, a racing read caches the new state, which is fine.
	invalidate func(key []byte)
	mu         sync.Mutex
}

// SetInvalidate installs the per-key cache-invalidation callback (#869).
// It is a setter rather than a NewApplier parameter because of construction
// order in cmd/muninn/server.go: the applier is built with the coordinator
// before the storage layer exists, and the storage layer is what owns the
// caches to invalidate. Call once during server wiring, before replication
// traffic flows; the callback must be safe for concurrent use.
func (a *Applier) SetInvalidate(fn func(key []byte)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.invalidate = fn
}

// invalidateBatchKeys decodes an applied batch repr and feeds every user key
// it touched to the invalidation callback. Called with a.mu held, after the
// repr has been committed. A decode failure is logged and abandoned rather
// than returned: the batch itself was applied successfully, and failing the
// Apply here would make the follower re-request an entry it already holds.
func (a *Applier) invalidateBatchKeys(repr []byte, seq uint64) {
	if a.invalidate == nil {
		return
	}
	r, _ := pebble.ReadBatch(repr)
	for {
		_, ukey, _, ok, err := r.Next()
		if err != nil {
			slog.Warn("applier: batch repr decode for cache invalidation failed — "+
				"follower caches may serve stale entries for this batch",
				"seq", seq, "err", err)
			return
		}
		if !ok {
			return
		}
		a.invalidate(ukey)
	}
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
		// Apply the repr atomically, then persist lastApplied in a separate batch.
		// SetRepr replaces the batch contents entirely; we must commit it as-is
		// (adding ops after SetRepr causes a batch-count inconsistency in Pebble).
		// The outer `batch` (created before the switch) is NOT used in this path —
		// it is simply closed by its deferred Close(). A dedicated `reprBatch` applies
		// the data, and a separate `markerBatch` writes the lastApplied sequence marker.
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
		// #869: the repr is committed — tell the storage layer which keys
		// changed so its caches drop any entries the batch just mutated.
		a.invalidateBatchKeys(entry.Value, entry.Seq)
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

	// #869: single-key ops mutate exactly entry.Key — same invalidation
	// contract as the OpBatch path, same after-commit ordering.
	if a.invalidate != nil && len(entry.Key) > 0 {
		a.invalidate(entry.Key)
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

// ResetTo rebases the apply cursor onto seq, in memory and in Pebble, and is
// the ONLY way the cursor moves other than applying an entry.
//
// It exists for exactly one caller: the join path, immediately after a full
// snapshot has replaced this node's local state. The snapshot's SnapshotSeq is
// the new authoritative baseline, and the cursor must follow it — including
// DOWNWARDS, when the Cortex has been rebuilt or restored and its head is now
// behind where this node had got to.
//
// Without this, the #631 failure is silent and total: WipeForResnapshot clears
// the persisted cursor but Applier.lastApplied is in memory, so it survives at
// its pre-rebuild value; Apply() then skips every entry with Seq <= that value
// (the idempotence check), and ReplicationLag() returns 0 because it clamps at
// cortexSeq <= lastApplied. The node reports a healthy lag of 0 and applies
// nothing, forever.
func (a *Applier) ResetTo(seq uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.lastApplied == seq {
		return nil
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, seq)
	if err := a.db.Set(lastAppliedKey(), buf, pebble.Sync); err != nil {
		return fmt.Errorf("applier: persist reset cursor %d: %w", seq, err)
	}
	slog.Info("applier: apply cursor rebased onto snapshot baseline",
		"previous", a.lastApplied, "baseline", seq)
	a.lastApplied = seq
	a.appliedSince = 0
	return nil
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
