package storage

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
)

// isClosedPanic reports whether the recovered panic value represents a
// "pebble: closed" condition. Pebble can panic with either an error value
// (implementing the error interface with pebble.ErrClosed) or a formatted
// string message containing "pebble: closed" when the WAL writer is torn down.
func isClosedPanic(r any) bool {
	if err, ok := r.(error); ok {
		return errors.Is(err, pebble.ErrClosed)
	}
	// Pebble's applyInternal panics with a formatted string in some code paths.
	return strings.Contains(fmt.Sprintf("%v", r), pebble.ErrClosed.Error())
}

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
//	  • scoring/Store.Save — vault weight persistence (0x18 key)
//	  • provenance/Store.Append — audit trail entries
//	  • auth writes — vault config, API keys
//	  • migration writes — schema version keys
//
//	pebble.NoSync + walSyncer group-commit (≤10ms data loss window):
//	  • WriteOrdinal / DeleteOrdinal — tree ordinal keys (0x1E)
//	  • UpdateMetadata — access count, last-access, state transitions
//	  • UpdateRelevance — relevance/stability score updates
//	  • SoftDelete / DeleteEngram — lifecycle transitions
//	  • WriteEntityEngramLink — entity forward/reverse index (0x20/0x23)
//	  • UpsertEntityRecord — global entity records (0x1F prefix)
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
	db      *pebble.DB
	stop    chan struct{}
	done    chan struct{}
	stopped atomic.Bool // set true before signalling the goroutine to stop
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

	ticker := time.NewTicker(walSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.doSync()
		case <-s.stop:
			// Final sync before shutdown.
			s.doSync()
			return
		}
	}
}

// doSync calls db.LogData and handles both error returns and panics gracefully.
//
// Pebble can surface a closed-DB condition in two ways:
//   - as a return value: pebble.ErrClosed ("pebble: closed")
//   - as a panic:        any value when the WAL writer is already torn down
//
// A closed-DB panic is always silently swallowed — the db is in an
// unrecoverable closed state regardless of how it got there (orderly shutdown
// or abrupt close). Any other panic while stopped is false is unexpected and
// is re-panicked so it is not silently lost.
func (s *walSyncer) doSync() {
	defer func() {
		if r := recover(); r != nil {
			// Swallow closed-db panics unconditionally: pebble surfaces a
			// closed-db condition as a panic (not an error) when the WAL writer
			// is already torn down. This is safe to ignore in all cases because
			// the db is already in an unrecoverable state.
			if isClosedPanic(r) {
				return
			}
			if s.stopped.Load() {
				return // expected: pebble is shutting down
			}
			panic(r) // unexpected during normal operation — propagate
		}
	}()

	if err := s.db.LogData(nil, pebble.Sync); err != nil {
		if s.stopped.Load() || errors.Is(err, pebble.ErrClosed) {
			return // expected during shutdown
		}
		slog.Error("storage: WAL sync failed", "component", "wal_syncer", "err", err)
	}
}

// Close signals the syncer to stop and blocks until the final sync completes.
// Must be called before db.Close().
func (s *walSyncer) Close() {
	// Mark as stopped BEFORE signalling the goroutine. This ensures that if
	// the goroutine is mid-doSync when Close is called, the error/panic
	// recovery correctly identifies the condition as expected shutdown.
	s.stopped.Store(true)
	close(s.stop)
	<-s.done
}
