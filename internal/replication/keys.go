package replication

import (
	"encoding/binary"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// The replication keyspace (#726).
//
// Until #726 every key in this package was inlined as a raw 0x19 byte, which
// is `prefix.Idempotency`. Log entries were `0x19|seq_be64(8)` and idempotency
// receipts are `0x19|siphash(op_id)(8)` — same prefix, same length, same
// database, no discriminator at all. `ReplicationLog.Prune`'s DeleteRange over
// `[0x19|be64(1), 0x19|be64(untilSeq+1))` therefore covered every receipt whose
// SipHash landed below the watermark.
//
// Everything now hangs off `prefix.Replication` with a second discriminator
// byte, so the entry range is a closed interval that provably contains nothing
// but entries:
//
//	0x2F 0x01 | seq_be64(8)   log entry (msgpack ReplicationEntry)
//	0x2F 0x02 | name...       replication metadata (seq counter, watermarks, epoch)
//
// Entries sort ahead of every metadata key (0x01 < 0x02), so the prune's
// exclusive upper bound can never reach metadata either.
const (
	// subEntry discriminates log entries within the replication prefix.
	subEntry byte = 0x01
	// subMeta discriminates replication metadata within the replication prefix.
	subMeta byte = 0x02
)

// entryRangeLower is the inclusive lower bound of the log-entry sub-range.
func entryRangeLower() []byte { return []byte{prefix.Replication, subEntry} }

// entryRangeUpper is the exclusive upper bound of the log-entry sub-range.
// Derived from keys.PrefixUpperBound rather than hand-rolled: three copies of a
// hand-rolled bound have been wrong in this repo (STO-11, #816/#819), and this
// one bounds a DeleteRange.
func entryRangeUpper() []byte { return keys.PrefixUpperBound(entryRangeLower()) }

// replicationEntryKey constructs the key for a replication log entry.
// Key: 0x2F | 0x01 | seq_be64(8) = 10 bytes.
func replicationEntryKey(seq uint64) []byte {
	key := make([]byte, 10)
	key[0] = prefix.Replication
	key[1] = subEntry
	binary.BigEndian.PutUint64(key[2:10], seq)
	return key
}

// entrySeqFromKey extracts the sequence number from a log-entry key, reporting
// false for anything that is not one.
func entrySeqFromKey(k []byte) (uint64, bool) {
	if len(k) != 10 || k[0] != prefix.Replication || k[1] != subEntry {
		return 0, false
	}
	return binary.BigEndian.Uint64(k[2:10]), true
}

// metaKey constructs a replication metadata key: 0x2F | 0x02 | name.
func metaKey(name string) []byte {
	key := make([]byte, 0, 2+len(name))
	key = append(key, prefix.Replication, subMeta)
	return append(key, name...)
}

// seqCounterKey stores the current head sequence number of the log.
// Formerly 0x19|0xFF*8 — i.e. it lived INSIDE the entry range, at seq
// MaxUint64. It is metadata now, so no sequence value can address it.
func seqCounterKey() []byte { return metaKey("seq_counter") }

// lastAppliedKey persists the applier's watermark (formerly 0x19 0x02 "last_app").
func lastAppliedKey() []byte { return metaKey("last_applied") }

// schemaVersionKey persists the on-disk schema version (formerly 0x19 0x03 "schema_v").
func schemaVersionKey() []byte { return metaKey("schema_version") }

// clusterEpochKey persists the cluster election epoch (formerly 0x19 0x03 "cluster_epoch").
func clusterEpochKey() []byte { return metaKey("cluster_epoch") }

// nodeRoleKey persists the last claimed node role (formerly 0x19 0x03 "node_role").
func nodeRoleKey() []byte { return metaKey("node_role") }

// snapCompleteKey is the clean-snapshot sentinel (formerly 0x19 0x10 "snap_complete").
var snapCompleteKey = metaKey("snap_complete")
