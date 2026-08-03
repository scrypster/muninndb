package migrate

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// v5 (#726) — relocate the replication keyspace off prefix.Idempotency (0x19).
//
// internal/replication inlined a raw 0x19 for every one of its keys, so
// replication log entries (`0x19|seq_be64(8)`) and idempotency receipts
// (`0x19|siphash(op_id)(8)`) were byte-identical in shape and shared one
// keyspace. `ReplicationLog.Prune` range-deletes `[0x19|be64(1),
// 0x19|be64(untilSeq+1))`, which covers every receipt whose SipHash lands under
// the watermark — negligible at seq ~10^5, linear in seq, and made live the
// moment the prune gets a production caller (#724/#737).
//
// Replication now owns prefix.Replication (0x2F) with a sub-namespace byte; see
// internal/replication/keys.go. This migration moves existing vaults.
//
// The METADATA keys are relocated (value-preserving). The LOG ENTRIES are
// DELETED rather than copied, for two reasons:
//
//  1. Cost. On a production Cortex the log was 116,248 entries / ~76 GB of
//     values, because each entry carries the full key and value of the write it
//     replicates. Copying that at startup is not a migration anyone can run.
//  2. Semantics. Dropping the log is exactly what a prune does, and its
//     consequence is already documented on `ReplicationLog.Prune`: a Lobe left
//     behind the retained window rejoins by snapshot. An upgrade restarts the
//     Cortex anyway, so followers reconnect regardless.
//
// The seq counter is preserved, so the head sequence never moves backwards and
// no future entry can reuse a sequence a follower has already acked.
//
// Deleting is done key-by-key behind a positive identification, never with a
// DeleteRange: the whole point of the migration is that this range is shared
// with idempotency receipts. A receipt is left byte-for-byte alone.
//
// Idempotent. A crash part-way leaves the migration version unstamped and the
// whole thing re-runs: relocation skips a destination that already exists,
// deletion finds fewer entries, both converge.
//
// DOWNGRADE: a vault migrated to v5 cannot be read by a binary that predates it
// — the older binary would find no seq counter, restart the log at seq 1, and
// re-issue sequence numbers its followers have already applied. That is caught
// structurally by Runner.Run's refuse-newer guard (stored 5 > the older
// binary's MaxRegisteredVersion 4 → refuse to start), not by anything here.
func RelocateReplicationPrefix(db *pebble.DB) error {
	if err := relocateReplicationMetadata(db); err != nil {
		return err
	}
	deleted, err := deleteLegacyReplicationEntries(db)
	if err != nil {
		return err
	}
	if deleted > 0 {
		// Range tombstones alone do not give the disk back. Compacting the old
		// range is the difference between "the keys are gone" and "the store
		// shrank", which on the deployments that motivated this is tens of GB.
		lo := []byte{prefix.Idempotency}
		hi := keys.PrefixUpperBound(lo)
		if err := db.Compact(lo, hi, true); err != nil {
			return fmt.Errorf("relocate replication prefix: compact legacy range: %w", err)
		}
	}
	return nil
}

// legacyReplicationKeys maps each old replication metadata key to its new
// address. The new keys are byte-pinned against internal/replication's own
// constructors by TestMigrationV5KeysMatchReplicationPackage — this package
// cannot import internal/replication (which imports internal/storage), so the
// pinning test lives on the replication side.
func legacyReplicationKeys() []struct{ old, new []byte } {
	meta := func(name string) []byte {
		k := []byte{prefix.Replication, 0x02}
		return append(k, name...)
	}
	legacySeqCounter := []byte{prefix.Idempotency, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	return []struct{ old, new []byte }{
		{legacySeqCounter, meta("seq_counter")},
		{append([]byte{prefix.Idempotency, 0x02}, "last_app"...), meta("last_applied")},
		{append([]byte{prefix.Idempotency, 0x03}, "schema_v"...), meta("schema_version")},
		{append([]byte{prefix.Idempotency, 0x03}, "cluster_epoch"...), meta("cluster_epoch")},
		{append([]byte{prefix.Idempotency, 0x03}, "node_role"...), meta("node_role")},
		{append([]byte{prefix.Idempotency, 0x10}, "snap_complete"...), meta("snap_complete")},
	}
}

// relocateReplicationMetadata copies each legacy metadata key to its new
// address and removes the old one. A destination that already exists is left
// alone (a re-run after a partial crash must not overwrite newer state with the
// stale legacy copy), but the legacy key is still cleared.
func relocateReplicationMetadata(db *pebble.DB) error {
	batch := db.NewBatch()
	defer batch.Close()

	moved := 0
	for _, m := range legacyReplicationKeys() {
		oldVal, closer, err := db.Get(m.old)
		if err == pebble.ErrNotFound {
			continue
		}
		if err != nil {
			return fmt.Errorf("relocate replication prefix: read %x: %w", m.old, err)
		}
		val := append([]byte(nil), oldVal...)
		closer.Close()

		_, nCloser, nErr := db.Get(m.new)
		if nErr == nil {
			nCloser.Close()
		} else if nErr != pebble.ErrNotFound {
			return fmt.Errorf("relocate replication prefix: read %x: %w", m.new, nErr)
		} else {
			if err := batch.Set(m.new, val, nil); err != nil {
				return fmt.Errorf("relocate replication prefix: set %x: %w", m.new, err)
			}
			moved++
		}
		if err := batch.Delete(m.old, nil); err != nil {
			return fmt.Errorf("relocate replication prefix: delete %x: %w", m.old, err)
		}
	}

	if batch.Empty() {
		return nil
	}
	// Sync: the new keys must be durable before the entry sweep starts deleting
	// anything, so a crash can never leave a store with neither copy.
	if err := batch.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("relocate replication prefix: commit metadata: %w", err)
	}
	slog.Info("relocate replication prefix: metadata moved", "keys", moved)
	return nil
}

// legacyLogEntry mirrors replication.ReplicationEntry's msgpack shape. It is a
// deliberate frozen copy: a migration must decode the format that is on disk,
// not whatever the live struct becomes later.
type legacyLogEntry struct {
	Seq         uint64
	Op          uint8
	Key         []byte
	Value       []byte
	TimestampNS int64
}

// legacyReceipt mirrors storage.IdempotencyReceipt for the negative check.
type legacyReceipt struct {
	EngramID  string `json:"engram_id"`
	CreatedAt int64  `json:"created_at"`
}

// IsLegacyReplicationLogEntry positively identifies a pre-#726 replication log
// entry at `0x19|seq_be64(8)`.
//
// It is deliberately a POSITIVE test on a destructive path, and it is
// conjunctive:
//
//   - the key must be exactly the 9-byte legacy entry shape;
//   - the value must NOT parse as an idempotency receipt carrying an engram id;
//   - the value MUST msgpack-decode as a log entry whose Seq is the very
//     sequence encoded in the key.
//
// The last clause is the strong one: a receipt would have to msgpack-decode AND
// carry the sequence its own SipHash happens to equal. Exported so the
// replication package can pin it against real Append output.
func IsLegacyReplicationLogEntry(key, val []byte) bool {
	if len(key) != 9 || key[0] != prefix.Idempotency {
		return false
	}
	seq := binary.BigEndian.Uint64(key[1:9])
	if seq == 0 {
		return false // Append starts at 1
	}
	var r legacyReceipt
	if err := json.Unmarshal(val, &r); err == nil && r.EngramID != "" {
		return false // an idempotency receipt — never ours to delete
	}
	var e legacyLogEntry
	if err := msgpack.Unmarshal(val, &e); err != nil {
		return false
	}
	return e.Seq == seq
}

// deleteLegacyReplicationEntries removes every positively-identified legacy log
// entry under 0x19, leaving idempotency receipts (and anything else it cannot
// prove is a log entry) untouched. Returns the number deleted.
func deleteLegacyReplicationEntries(db *pebble.DB) (int, error) {
	lower := []byte{prefix.Idempotency}
	upper := keys.PrefixUpperBound(lower)

	iter, err := db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return 0, fmt.Errorf("relocate replication prefix: new iter: %w", err)
	}
	defer iter.Close()

	const batchSize = 1000
	batch := db.NewBatch()
	defer func() { batch.Close() }()

	deleted, kept := 0, 0
	pending := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		val, err := iter.ValueAndErr()
		if err != nil {
			return deleted, fmt.Errorf("relocate replication prefix: iter value: %w", err)
		}
		if !IsLegacyReplicationLogEntry(iter.Key(), val) {
			kept++
			continue
		}
		if err := batch.Delete(append([]byte(nil), iter.Key()...), nil); err != nil {
			return deleted, fmt.Errorf("relocate replication prefix: delete: %w", err)
		}
		deleted++
		pending++
		if pending >= batchSize {
			if err := batch.Commit(pebble.Sync); err != nil {
				return deleted, fmt.Errorf("relocate replication prefix: commit deletes: %w", err)
			}
			batch.Close()
			batch = db.NewBatch()
			pending = 0
		}
	}
	if err := iter.Error(); err != nil {
		return deleted, fmt.Errorf("relocate replication prefix: iter: %w", err)
	}
	if pending > 0 {
		if err := batch.Commit(pebble.Sync); err != nil {
			return deleted, fmt.Errorf("relocate replication prefix: commit deletes: %w", err)
		}
	}

	slog.Info("relocate replication prefix: legacy log entries dropped",
		"deleted", deleted, "left_alone", kept)
	return deleted, nil
}
