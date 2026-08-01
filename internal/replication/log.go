package replication

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"
)

var (
	ErrEmptyLog = errors.New("replication: empty log")
)

// replicationLogPrefix is the 0x19 prefix used for replication log keys.
const replicationLogPrefix = 0x19

// seqCounterKey is the key used to store the current sequence counter.
// Key: 0x19 | 0xFF | 0xFF | 0xFF | 0xFF | 0xFF | 0xFF | 0xFF | 0xFF = 9 bytes
func seqCounterKey() []byte {
	key := make([]byte, 9)
	key[0] = replicationLogPrefix
	for i := 1; i < 9; i++ {
		key[i] = 0xFF
	}
	return key
}

// replicationEntryKey constructs the key for a replication log entry.
// Key: 0x19 | seq_be64(8) = 9 bytes
func replicationEntryKey(seq uint64) []byte {
	key := make([]byte, 9)
	key[0] = replicationLogPrefix
	binary.BigEndian.PutUint64(key[1:9], seq)
	return key
}

// ReplicationLog manages the append-only replication log stored in Pebble.
type ReplicationLog struct {
	db     *pebble.DB
	mu     sync.Mutex
	seq    uint64 // current sequence number
	init   bool   // whether seq has been initialized from Pebble
	subs   []chan struct{}
	subsMu sync.Mutex
}

// NewReplicationLog creates a new ReplicationLog backed by a Pebble database.
func NewReplicationLog(db *pebble.DB) *ReplicationLog {
	return &ReplicationLog{
		db: db,
	}
}

// ensureSeqInit loads the current sequence counter from Pebble on first access.
func (l *ReplicationLog) ensureSeqInit() error {
	if l.init {
		return nil
	}

	val, closer, err := l.db.Get(seqCounterKey())
	if err != nil && err != pebble.ErrNotFound {
		return err
	}
	if closer != nil {
		defer closer.Close()
	}

	if err == pebble.ErrNotFound || len(val) == 0 {
		l.seq = 0
	} else {
		if len(val) >= 8 {
			l.seq = binary.BigEndian.Uint64(val)
		}
	}

	l.init = true
	return nil
}

// persistSeq writes the current sequence counter to Pebble.
func (l *ReplicationLog) persistSeq() error {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, l.seq)
	return l.db.Set(seqCounterKey(), buf, nil)
}

// Append writes a new entry to the replication log and returns its sequence number.
// The entry is serialized using msgpack and stored under key 0x19 | seq_be64.
// Thread-safe.
func (l *ReplicationLog) Append(op WALOp, key, value []byte) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.ensureSeqInit(); err != nil {
		return 0, err
	}

	l.seq++

	entry := ReplicationEntry{
		Seq:         l.seq,
		Op:          op,
		Key:         key,
		Value:       value,
		TimestampNS: timeNowNanos(),
	}

	data, err := msgpack.Marshal(&entry)
	if err != nil {
		l.seq-- // rollback
		return 0, err
	}

	batch := l.db.NewBatch()
	defer batch.Close()

	if err := batch.Set(replicationEntryKey(l.seq), data, nil); err != nil {
		l.seq--
		return 0, err
	}

	seqBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(seqBuf, l.seq)
	if err := batch.Set(seqCounterKey(), seqBuf, nil); err != nil {
		l.seq--
		return 0, err
	}

	if err := batch.Commit(pebble.Sync); err != nil {
		l.seq--
		return 0, err
	}

	seq := l.seq
	l.notifySubscribers()
	return seq, nil
}

// ReadSince returns all entries with seq > afterSeq, up to limit entries.
// Returns entries in ascending order of sequence number.
func (l *ReplicationLog) ReadSince(afterSeq uint64, limit int) ([]ReplicationEntry, error) {
	l.mu.Lock()

	if err := l.ensureSeqInit(); err != nil {
		l.mu.Unlock()
		return nil, err
	}

	// Capture currentSeq while holding the lock to avoid a data race: another
	// goroutine could call Append() between Unlock() and the iterator creation
	// below, advancing l.seq. Missing those new entries is intentional — the
	// caller will pick them up on the next poll.
	currentSeq := l.seq

	l.mu.Unlock()

	if limit <= 0 {
		limit = 1000
	}

	// Scan from afterSeq+1 to currentSeq (snapshot taken above)
	startKey := replicationEntryKey(afterSeq + 1)
	var endKey []byte
	if currentSeq == ^uint64(0) { // uint64 max
		endKey = seqCounterKey()
	} else {
		endKey = replicationEntryKey(currentSeq + 1)
	}

	iter, err := l.db.NewIter(&pebble.IterOptions{
		LowerBound: startKey,
		UpperBound: endKey,
	})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	entries := make([]ReplicationEntry, 0, limit)
	for valid := iter.First(); valid && len(entries) < limit; valid = iter.Next() {
		var entry ReplicationEntry
		if err := msgpack.Unmarshal(iter.Value(), &entry); err != nil {
			// Extract the sequence number from the key (0x19 | seq_be64) so we can
			// report exactly which entry is corrupt before returning an error.
			// Silently skipping would create an invisible gap in the replication stream.
			var seq uint64
			if key := iter.Key(); len(key) >= 9 {
				seq = binary.BigEndian.Uint64(key[1:9])
			}
			slog.Error("replication log: malformed entry, replication may have gaps",
				"seq", seq, "err", err)
			return nil, fmt.Errorf("malformed log entry at seq %d: %w", seq, err)
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

// Prune deletes log entries with seq <= untilSeq and returns how many were
// removed. Used to clean up old entries once all replicas have acknowledged
// them, or once the backlog ceiling forces a prune (see ClusterConfig.
// MaxLogBacklog).
//
// CALLER RESPONSIBILITY: Prune must only be called after verifying that every
// connected replica has applied all entries up to untilSeq (check
// ClusterCoordinator.ReplicaLag or equivalent). Pruning entries that a lagging
// Lobe has not yet applied will cause a permanent replication gap — the Lobe
// must rejoin via snapshot if it falls behind a pruned point.
func (l *ReplicationLog) Prune(untilSeq uint64) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.ensureSeqInit(); err != nil {
		return 0, err
	}

	if untilSeq >= l.seq {
		return 0, nil // nothing to prune
	}

	// Deliberately NOT a DeleteRange. Prefix 0x19 is shared with
	// prefix.Idempotency, whose keys are 0x19|siphash(op_id) — byte-identical
	// in shape to 0x19|seq_be64. A range delete would take any receipt whose
	// siphash happens to fall below untilSeq. The two are trivially separable
	// by encoding: log entries are msgpack, receipts are JSON. So decode, and
	// delete only what is provably a log entry at or below the watermark.
	iter, err := l.db.NewIter(&pebble.IterOptions{
		LowerBound: replicationEntryKey(1),
		UpperBound: replicationEntryKey(untilSeq + 1),
	})
	if err != nil {
		return 0, fmt.Errorf("replication log prune: new iter: %w", err)
	}

	var toDelete [][]byte
	for iter.First(); iter.Valid(); iter.Next() {
		var entry ReplicationEntry
		if err := msgpack.Unmarshal(iter.Value(), &entry); err != nil {
			continue // not a log entry (idempotency receipt) — leave it alone
		}
		if entry.Seq > untilSeq {
			continue
		}
		k := make([]byte, len(iter.Key()))
		copy(k, iter.Key())
		toDelete = append(toDelete, k)
	}
	if err := iter.Error(); err != nil {
		_ = iter.Close()
		return 0, fmt.Errorf("replication log prune: iter: %w", err)
	}
	if err := iter.Close(); err != nil {
		return 0, fmt.Errorf("replication log prune: close iter: %w", err)
	}

	// Batch the deletes so a large first prune (a log that has never been
	// pruned can hold hundreds of thousands of entries) does not build one
	// enormous batch.
	const deleteBatchSize = 1000
	deleted := 0
	for start := 0; start < len(toDelete); start += deleteBatchSize {
		end := start + deleteBatchSize
		if end > len(toDelete) {
			end = len(toDelete)
		}
		batch := l.db.NewBatch()
		for _, k := range toDelete[start:end] {
			if err := batch.Delete(k, nil); err != nil {
				batch.Close()
				return deleted, fmt.Errorf("replication log prune: delete: %w", err)
			}
		}
		if err := batch.Commit(nil); err != nil {
			batch.Close()
			return deleted, fmt.Errorf("replication log prune: commit: %w", err)
		}
		batch.Close()
		deleted += end - start
	}

	// Reclaim the space now. Pebble deletes are tombstones — the bytes come
	// back only when a compaction rewrites the sstables holding them, and with
	// the default single compaction slot a large backlog can sit on disk for
	// hours or days. In production, pruning 104k entries reclaimed nothing
	// until a compaction was forced by hand: the store stayed at 20 GB.
	//
	// Compact only the range that was just pruned, not the whole keyspace, so
	// this stays proportional to the work done and does not disturb unrelated
	// key ranges. Pebble's Compact is an online operation — the database keeps
	// serving.
	//
	// A compaction failure is not a prune failure: the entries are gone either
	// way and the next cycle will try again, so this logs rather than returns.
	if deleted > 0 {
		// Flush first: the tombstones are still in the memtable, and a
		// compaction of the on-disk sstables cannot drop keys whose deletes it
		// cannot see. Without this the compaction reclaims almost nothing.
		if err := l.db.Flush(); err != nil {
			slog.Warn("replication log: flush before compaction failed", "err", err)
		}
		if err := l.db.Compact(replicationEntryKey(1), replicationEntryKey(untilSeq+1), true); err != nil {
			slog.Warn("replication log: compaction after prune failed — space will be"+
				" reclaimed by a later compaction", "err", err, "pruned", deleted)
		}
	}

	return deleted, nil
}

// CurrentSeq returns the latest committed sequence number.
// Thread-safe.
func (l *ReplicationLog) CurrentSeq() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.ensureSeqInit(); err != nil {
		return 0
	}

	return l.seq
}

// Subscribe registers a notification channel that receives a signal whenever a
// new entry is appended to the log. The returned unsubscribe function removes
// the subscription and closes the channel. It is safe to call from multiple
// goroutines concurrently.
func (l *ReplicationLog) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	l.subsMu.Lock()
	l.subs = append(l.subs, ch)
	l.subsMu.Unlock()

	unsubscribe := func() {
		l.subsMu.Lock()
		defer l.subsMu.Unlock()
		for i, s := range l.subs {
			if s == ch {
				l.subs = append(l.subs[:i], l.subs[i+1:]...)
				close(ch)
				return
			}
		}
	}
	return ch, unsubscribe
}

// notifySubscribers sends a non-blocking signal to all registered subscriber
// channels. If a channel already has a pending notification it is skipped.
// Must never block.
func (l *ReplicationLog) notifySubscribers() {
	l.subsMu.Lock()
	defer l.subsMu.Unlock()
	for _, ch := range l.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// timeNowNanos returns the current time in nanoseconds since epoch.
func timeNowNanos() int64 {
	return time.Now().UnixNano()
}
