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

// Prune deletes log entries with seq <= untilSeq.
// Used to clean up old entries once all replicas have acknowledged them.
//
// The DeleteRange below is safe BY CONSTRUCTION since #726: both bounds carry
// the 0x2F|0x01 entry discriminator, so the range contains only log entries.
// It used to be `0x19|be64(1) .. 0x19|be64(untilSeq+1)`, which swept every
// idempotency receipt (`0x19|siphash(op_id)`) whose hash landed below the
// watermark. Do NOT reintroduce a bound that starts at a bare prefix byte.
//
// CALLER RESPONSIBILITY: Prune must only be called after verifying that every
// connected replica has applied all entries up to untilSeq (check
// ClusterCoordinator.ReplicaLag or equivalent). Pruning entries that a lagging
// Lobe has not yet applied will cause a permanent replication gap — the Lobe
// must rejoin via snapshot if it falls behind a pruned point.
func (l *ReplicationLog) Prune(untilSeq uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.ensureSeqInit(); err != nil {
		return err
	}

	if untilSeq >= l.seq {
		return nil // nothing to prune
	}

	batch := l.db.NewBatch()
	defer batch.Close()

	// Delete all entries from seq=1 up to and including untilSeq.
	// DeleteRange is [start, end) so end key is untilSeq+1.
	startKey := replicationEntryKey(1)
	endKey := replicationEntryKey(untilSeq + 1)
	if err := batch.DeleteRange(startKey, endKey, nil); err != nil {
		return err
	}

	return batch.Commit(nil)
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
