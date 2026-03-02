package storage

import (
	"errors"
	"log/slog"
	"time"

	"github.com/cockroachdb/pebble"
)

const walSyncInterval = 10 * time.Millisecond

// walSyncer periodically calls db.LogData(nil, pebble.Sync) to flush the WAL
// without triggering a memtable flush. This provides group-commit semantics:
// all batch.Commit(pebble.NoSync) writes accumulate in the WAL and are durably
// fsynced every walSyncInterval (default 10ms). Max data loss on crash: 10ms.
//
// This is the same trade-off as MySQL innodb_flush_log_at_trx_commit=2 or
// PostgreSQL synchronous_commit=off, and is safe because Pebble's own WAL
// provides crash recovery — the LogData sync covers all preceding NoSync writes.
//
// Durability contract — which paths use Sync vs NoSync:
//
//	pebble.Sync (immediate fsync, zero data loss on crash):
//	  • WriteEngram (0x01 + 0x02 keys) — primary write path; default behavior
//	  • WriteAssociation — association forward/reverse keys (0x03/0x04)
//	  • WriteOrdinal — tree ordinal keys (0x1E)
//	  • scoring/Store.Save — vault weight persistence (0x18 key)
//	  • provenance/Store.Append — audit trail entries
//	  • auth writes — vault config, API keys
//	  • migration writes — schema version keys
//
//	pebble.NoSync + walSyncer group-commit (≤10ms data loss window):
//	  • UpdateMetadata — access count, last-access, state transitions
//	  • UpdateRelevance — relevance/stability score updates
//	  • SoftDelete / DeleteEngram — lifecycle transitions
//	  • WriteEntityEngramLink — entity forward/reverse index (0x20/0x23)
//	  • UpsertEntityRecord — global entity records (0x22 prefix)
//	  • UpsertRelationshipRecord — entity relationships (0x21)
//	  • IncrementEntityCoOccurrence — co-occurrence counts (0x24)
//	  • WriteLastAccessEntry / DeleteLastAccessEntry — 0x22 last-access index
//	  • WriteIdempotency — op_id receipts
//	  • WriteVaultName — vault name forward index
//	  • episodic/Store — all episode and frame writes
//	  • FTS index updates — keyword search (eventual consistency)
//
//	Design rationale:
//	  The Sync paths cover "primary records" — writes that the caller expects
//	  to be durable when WriteEngram/WriteAssociation return. The NoSync paths
//	  cover "derived state" — metadata, indexes, and scores that can be
//	  reconstructed or tolerate a 10ms rollback without user-visible data loss.
//	  The walSyncer guarantees that all NoSync writes are durably flushed within
//	  walSyncInterval (10ms) via LogData(nil, pebble.Sync), providing a bounded
//	  durability window equivalent to MySQL innodb_flush_log_at_trx_commit=2.
type walSyncer struct {
	db   *pebble.DB
	stop chan struct{}
	done chan struct{}
}

func newWALSyncer(db *pebble.DB) *walSyncer {
	s := &walSyncer{
		db:   db,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *walSyncer) run() {
	defer close(s.done)
	// Recover from the "pebble: closed" panic that can occur if db.Close()
	// races with an in-flight ticker sync during shutdown.  Pebble panics with
	// pebble.ErrClosed (an error value), so we check via errors.Is.
	// Any other unexpected panic is re-panicked so it is not silently swallowed.
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok && errors.Is(err, pebble.ErrClosed) {
				return // expected during shutdown
			}
			panic(r) // unexpected — re-panic
		}
	}()

	ticker := time.NewTicker(walSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := s.db.LogData(nil, pebble.Sync); err != nil {
				slog.Error("storage: WAL sync failed", "component", "wal_syncer", "err", err)
			}
		case <-s.stop:
			// Final sync before shutdown.
			if err := s.db.LogData(nil, pebble.Sync); err != nil {
				slog.Error("storage: final WAL sync on shutdown failed", "component", "wal_syncer", "err", err)
			}
			return
		}
	}
}

// Close signals the syncer to stop and blocks until the final sync completes.
// Must be called before db.Close().
func (s *walSyncer) Close() {
	close(s.stop)
	<-s.done
}
