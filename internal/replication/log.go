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

// The log's key layout lives in keys.go (prefix.Replication, #726).

// ReplicationLog manages the append-only replication log stored in Pebble.
type ReplicationLog struct {
	db         *pebble.DB
	mu         sync.Mutex
	seq        uint64 // current sequence number
	init       bool   // whether seq has been initialized from Pebble
	lastPruned uint64 // highest seq already deleted by Prune, this process lifetime
	subs       []chan struct{}
	subsMu     sync.Mutex
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
// The entry is serialized using msgpack and stored under key 0x2F|0x01|seq_be64.
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
	if currentSeq == ^uint64(0) { // uint64 max — no seq+1 to address
		endKey = entryRangeUpper()
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
			// Extract the sequence number from the key so we can report exactly
			// which entry is corrupt before returning an error. Silently skipping
			// would create an invisible gap in the replication stream.
			seq, _ := entrySeqFromKey(iter.Key())
			slog.Error("replication log: malformed entry, replication may have gaps",
				"seq", seq, "err", err)
			return nil, fmt.Errorf("malformed log entry at seq %d: %w", seq, err)
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

// Prune deletes log entries with seq <= untilSeq. Used to clean up old
// entries once all replicas have acknowledged them, or once the backlog
// ceiling forces a prune (see ClusterConfig.MaxLogBacklog).
//
// The DeleteRange below is safe BY CONSTRUCTION since #726: both bounds carry
// the 0x2F|0x01 entry discriminator (keys.go), so the range contains only log
// entries — it can never reach an idempotency receipt (a different prefix
// entirely now) or replication metadata (0x2F|0x02, which sorts after every
// entry key). Before #726 this was a decode-per-key scan: the log's prefix
// was shared with idempotency receipts of the same key shape, so distinguishing
// the two required unmarshalling every value in the window. That is gone with
// the shared prefix; reintroducing a scan here would resurrect the exact
// disk-decode cost the relocation exists to remove (a first prune over a large
// unpruned log used to decode every entry).
//
// lastPruned tracks how far this process has already deleted, so a prune
// that lands on an already-pruned watermark (the periodic prune runs every
// PruneIntervalSec even when nothing new has been acknowledged) is a fast
// no-op rather than a redundant DeleteRange + Flush + Compact every cycle.
// It resets on restart, so the first prune after a process restart may
// re-delete an empty sub-range — harmless, just a wasted (cheap) tombstone
// write, not a correctness issue.
//
// CALLER RESPONSIBILITY: Prune must only be called after verifying that every
// connected replica has applied all entries up to untilSeq (check
// ClusterCoordinator.ReplicaLag or equivalent). Pruning entries that a lagging
// Lobe has not yet applied will cause a permanent replication gap — the Lobe
// must rejoin via snapshot if it falls behind a pruned point.
func (l *ReplicationLog) Prune(untilSeq uint64) error {
	l.mu.Lock()
	if err := l.ensureSeqInit(); err != nil {
		l.mu.Unlock()
		return err
	}
	seq := l.seq
	if untilSeq >= seq {
		l.mu.Unlock()
		return nil // nothing to prune
	}
	if untilSeq <= l.lastPruned {
		l.mu.Unlock()
		return nil // already pruned past this watermark
	}
	from := l.lastPruned + 1
	l.lastPruned = untilSeq
	l.mu.Unlock()

	// Everything from here on operates on [from, untilSeq], which is at or
	// below untilSeq < seq — Append only ever writes at seq+1 and beyond, so
	// this never races a concurrent Append. mu is not held across it: mu is
	// the Append mutex, Append is on the synchronous write path
	// (PebbleStore.replicateBatch), and the flush/compact below can run for
	// as long as it takes to rewrite the pruned sstables. Holding mu across
	// that would stall every write in the process.
	startKey := replicationEntryKey(from)
	endKey := replicationEntryKey(untilSeq + 1)

	batch := l.db.NewBatch()
	defer batch.Close()
	if err := batch.DeleteRange(startKey, endKey, nil); err != nil {
		return fmt.Errorf("replication log prune: delete range: %w", err)
	}
	if err := batch.Commit(nil); err != nil {
		return fmt.Errorf("replication log prune: commit: %w", err)
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
	//
	// Flush first: the tombstones are still in the memtable, and a compaction
	// of the on-disk sstables cannot drop keys whose deletes it cannot see.
	// Without this the compaction reclaims almost nothing.
	if err := l.db.Flush(); err != nil {
		slog.Warn("replication log: flush before compaction failed", "err", err)
	}
	if err := l.db.Compact(startKey, endKey, true); err != nil {
		slog.Warn("replication log: compaction after prune failed — space will be"+
			" reclaimed by a later compaction", "err", err, "from_seq", from, "until_seq", untilSeq)
	}

	return nil
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
