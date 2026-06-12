package replication

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

const (
	// defaultChunkSize is the target size in bytes for each snapshot chunk.
	defaultChunkSize = 1 << 20 // 1 MB

	// defaultRateLimit is the default transfer rate in bytes/sec.
	defaultRateLimit = 100 * 1024 * 1024 // 100 MB/s

	// maxBatchSize is the maximum Pebble write batch size on the receiver.
	maxBatchSize = 10 * 1024 * 1024 // 10 MB

	// ackTimeout is the maximum time to wait for a TypeSnapAck from the receiver.
	ackTimeout = 30 * time.Second
)

// snapCompleteKey is the sentinel key written after a successful full snapshot.
// Its presence indicates a clean, complete snapshot. If absent on startup, the
// Lobe should re-join from scratch on the next Cortex connection.
var snapCompleteKey = append([]byte{0x19, 0x10}, []byte("snap_complete")...)

var (
	ErrSnapshotInProgress = errors.New("snapshot: transfer already in progress")
)

// SnapshotSender streams a Pebble snapshot to a remote peer (Cortex side).
type SnapshotSender struct {
	db        *pebble.DB
	repLog    *ReplicationLog
	rateLimit float64 // bytes/sec
	mu        sync.Mutex
	sending   bool
}

// NewSnapshotSender creates a SnapshotSender with default rate limiting.
func NewSnapshotSender(db *pebble.DB, repLog *ReplicationLog) *SnapshotSender {
	return &SnapshotSender{
		db:        db,
		repLog:    repLog,
		rateLimit: defaultRateLimit,
	}
}

// Send takes a Pebble snapshot and streams all KV pairs to conn.
// Returns the SnapshotSeq (the repLog seq at snapshot time).
// Returns ErrSnapshotInProgress if a transfer is already in progress.
func (s *SnapshotSender) Send(ctx context.Context, conn *PeerConn) (uint64, error) {
	s.mu.Lock()
	if s.sending {
		s.mu.Unlock()
		return 0, ErrSnapshotInProgress
	}
	s.sending = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.sending = false
		s.mu.Unlock()
	}()

	// Capture seq BEFORE snapshot — guarantees all entries up to snapshotSeq
	// exist in the snapshot.
	snapshotSeq := s.repLog.CurrentSeq()

	snap := s.db.NewSnapshot()
	defer snap.Close()

	// Count approximate keys for progress reporting.
	totalKeys, err := s.countKeys(snap)
	if err != nil {
		return 0, fmt.Errorf("snapshot: count keys: %w", err)
	}

	// Send header.
	header := mbp.SnapHeader{
		SnapshotSeq: snapshotSeq,
		NodeID:      "", // filled by caller if needed
		TotalKeys:   totalKeys,
		Timestamp:   time.Now().UnixNano(),
	}
	headerPayload, err := msgpack.Marshal(&header)
	if err != nil {
		return 0, fmt.Errorf("snapshot: marshal header: %w", err)
	}
	if err := conn.Send(mbp.TypeSnapHeader, headerPayload); err != nil {
		return 0, fmt.Errorf("snapshot: send header: %w", err)
	}

	// Wait for SnapAck.
	if err := s.waitForAck(ctx, conn); err != nil {
		return 0, fmt.Errorf("snapshot: wait for ack: %w", err)
	}

	// Stream chunks.
	if err := s.streamChunks(ctx, conn, snap); err != nil {
		return 0, fmt.Errorf("snapshot: stream chunks: %w", err)
	}

	// Send complete.
	if err := conn.Send(mbp.TypeSnapComplete, nil); err != nil {
		return 0, fmt.Errorf("snapshot: send complete: %w", err)
	}

	return snapshotSeq, nil
}

// countKeys counts all keys in the snapshot. This iterates all keys which is
// O(n) but provides an accurate count for progress display.
func (s *SnapshotSender) countKeys(snap *pebble.Snapshot) (uint64, error) {
	iter, err := snap.NewIter(nil)
	if err != nil {
		return 0, err
	}
	defer iter.Close()

	var count uint64
	for valid := iter.First(); valid; valid = iter.Next() {
		count++
	}
	return count, nil
}

// waitForAck reads a TypeSnapAck from the connection, respecting ctx cancellation.
// A read deadline is set on the underlying connection so that the receive
// goroutine exits when the deadline passes, preventing a goroutine leak if ctx
// is cancelled before the ack arrives.
func (s *SnapshotSender) waitForAck(ctx context.Context, conn *PeerConn) error {
	if tc, ok := conn.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = tc.SetReadDeadline(time.Now().Add(ackTimeout))
		defer tc.SetReadDeadline(time.Time{}) // clear deadline after
	}

	type result struct {
		frameType uint8
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		ft, _, err := conn.Receive()
		ch <- result{ft, err}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if r.frameType != mbp.TypeSnapAck {
			return fmt.Errorf("expected TypeSnapAck (0x%02x), got 0x%02x", mbp.TypeSnapAck, r.frameType)
		}
		return nil
	}
}

// streamChunks iterates the snapshot and sends KV pairs in chunks.
func (s *SnapshotSender) streamChunks(ctx context.Context, conn *PeerConn, snap *pebble.Snapshot) error {
	iter, err := snap.NewIter(nil)
	if err != nil {
		return err
	}
	defer iter.Close()

	var (
		chunkIdx  uint32
		pairs     []mbp.KVPair
		chunkSize int
	)

	sendChunk := func(done bool) error {
		chunk := mbp.SnapChunk{
			ChunkNum:  chunkIdx,
			LastChunk: done,
			Pairs:     pairs,
		}
		payload, err := msgpack.Marshal(&chunk)
		if err != nil {
			return fmt.Errorf("marshal chunk %d: %w", chunkIdx, err)
		}
		if err := conn.Send(mbp.TypeSnapChunk, payload); err != nil {
			return fmt.Errorf("send chunk %d: %w", chunkIdx, err)
		}
		return nil
	}

	for valid := iter.First(); valid; valid = iter.Next() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Copy key and value since iterator reuses buffers.
		key := make([]byte, len(iter.Key()))
		copy(key, iter.Key())
		val := make([]byte, len(iter.Value()))
		copy(val, iter.Value())

		pairs = append(pairs, mbp.KVPair{Key: key, Value: val})
		chunkSize += len(key) + len(val)

		if chunkSize >= defaultChunkSize {
			chunkStart := time.Now()
			if err := sendChunk(false); err != nil {
				return err
			}
			s.rateWait(ctx, chunkSize, chunkStart)
			chunkIdx++
			pairs = nil
			chunkSize = 0
		}
	}

	// Send final chunk (may have remaining pairs or be empty).
	if err := sendChunk(true); err != nil {
		return err
	}

	return nil
}

// rateWait sleeps to enforce the rate limit, checking ctx.Done() to avoid
// blocking indefinitely on cancellation.
func (s *SnapshotSender) rateWait(ctx context.Context, chunkBytes int, chunkStart time.Time) {
	if s.rateLimit <= 0 {
		return
	}
	elapsed := time.Since(chunkStart)
	targetDuration := time.Duration(float64(chunkBytes) / s.rateLimit * float64(time.Second))
	sleepDur := targetDuration - elapsed
	if sleepDur <= 0 {
		return
	}

	timer := time.NewTimer(sleepDur)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// SnapshotReceiver reads a snapshot stream and writes it to local Pebble (Lobe side).
type SnapshotReceiver struct {
	db *pebble.DB
}

// NewSnapshotReceiver creates a SnapshotReceiver.
func NewSnapshotReceiver(db *pebble.DB) *SnapshotReceiver {
	return &SnapshotReceiver{db: db}
}

// WipeForResnapshot deletes all keys from the local Pebble DB to prepare for
// a fresh snapshot, preventing orphaned keys from a prior partial snapshot
// from surviving into the new state. Each batch is committed with Sync.
func (r *SnapshotReceiver) WipeForResnapshot() error {
	iter, err := r.db.NewIter(nil)
	if err != nil {
		return fmt.Errorf("snapshot wipe: new iter: %w", err)
	}
	defer iter.Close()

	batch := r.db.NewBatch()
	batchSize := 0

	epochKey := clusterEpochKey()
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		// Preserve ONLY the exact cluster_epoch key — it is fencing state, not
		// snapshot data, and wiping it would make a re-snapshotting node (e.g. a
		// deferring ex-primary) restart at epoch 0 (#531 PR3). Must be an exact-key
		// match: the 0x19 prefix is overloaded (idempotency receipts are
		// 0x19|siphash, ~0.4% of which begin 0x19 0x03), so a prefix skip would
		// leave stale data keys behind after the snapshot.
		if bytes.Equal(k, epochKey) {
			continue
		}
		key := make([]byte, len(k))
		copy(key, k)
		if err := batch.Delete(key, nil); err != nil {
			batch.Close()
			return fmt.Errorf("snapshot wipe: delete: %w", err)
		}
		batchSize += len(key)
		if batchSize >= maxBatchSize {
			if err := batch.Commit(pebble.Sync); err != nil {
				batch.Close()
				return fmt.Errorf("snapshot wipe: commit batch: %w", err)
			}
			batch.Close()
			batch = r.db.NewBatch()
			batchSize = 0
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		batch.Close()
		return fmt.Errorf("snapshot wipe: commit final: %w", err)
	}
	batch.Close()
	return nil
}

// Receive reads a snapshot stream from conn and writes all KV pairs to local Pebble.
// Returns the SnapshotSeq from the SnapHeader (Lobe catches up from snapshotSeq+1).
func (r *SnapshotReceiver) Receive(ctx context.Context, conn *PeerConn) (uint64, error) {
	// Defensive pre-wipe: ensure no stale keys from any previous partial snapshot.
	if err := r.WipeForResnapshot(); err != nil {
		return 0, fmt.Errorf("snapshot recv: pre-wipe: %w", err)
	}
	// Read header.
	headerType, headerPayload, err := conn.Receive()
	if err != nil {
		return 0, fmt.Errorf("snapshot recv: read header: %w", err)
	}
	if headerType != mbp.TypeSnapHeader {
		return 0, fmt.Errorf("snapshot recv: expected TypeSnapHeader (0x%02x), got 0x%02x", mbp.TypeSnapHeader, headerType)
	}

	var header mbp.SnapHeader
	if err := msgpack.Unmarshal(headerPayload, &header); err != nil {
		return 0, fmt.Errorf("snapshot recv: unmarshal header: %w", err)
	}

	// Send ack.
	if err := conn.Send(mbp.TypeSnapAck, nil); err != nil {
		return 0, fmt.Errorf("snapshot recv: send ack: %w", err)
	}

	// Read chunks until Done.
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}

		frameType, payload, err := conn.Receive()
		if err != nil {
			return 0, fmt.Errorf("snapshot recv: read chunk: %w", err)
		}

		if frameType == mbp.TypeSnapComplete {
			break
		}

		if frameType != mbp.TypeSnapChunk {
			return 0, fmt.Errorf("snapshot recv: expected TypeSnapChunk (0x%02x), got 0x%02x", mbp.TypeSnapChunk, frameType)
		}

		var chunk mbp.SnapChunk
		if err := msgpack.Unmarshal(payload, &chunk); err != nil {
			return 0, fmt.Errorf("snapshot recv: unmarshal chunk: %w", err)
		}

		if err := r.applyChunk(chunk.Pairs, header.SnapshotSeq, chunk.LastChunk); err != nil {
			return 0, fmt.Errorf("snapshot recv: apply chunk %d: %w", chunk.ChunkNum, err)
		}

		// LastChunk is informational only; the loop exits on TypeSnapComplete above.
		// Do not read an extra frame here — the sender sends TypeSnapComplete exactly once.
	}

	return header.SnapshotSeq, nil
}

// applyChunk writes KV pairs to Pebble in batches, splitting at maxBatchSize.
// When final is true, the last batch is committed with pebble.Sync and the
// snap_complete sentinel key is written in the same batch, so a crash before
// fsync leaves the sentinel absent — signalling an incomplete snapshot on restart.
func (r *SnapshotReceiver) applyChunk(pairs []mbp.KVPair, snapshotSeq uint64, final bool) error {
	batch := r.db.NewBatch()
	batchSize := 0

	for _, kv := range pairs {
		if err := batch.Set(kv.Key, kv.Value, nil); err != nil {
			batch.Close()
			return err
		}
		batchSize += len(kv.Key) + len(kv.Value)

		if batchSize >= maxBatchSize {
			if err := batch.Commit(pebble.NoSync); err != nil {
				batch.Close()
				return err
			}
			batch.Close()
			batch = r.db.NewBatch()
			batchSize = 0
		}
	}

	if final {
		// Write sentinel in the same batch so crash-before-fsync leaves it absent.
		seqBuf := make([]byte, 8)
		binary.BigEndian.PutUint64(seqBuf, snapshotSeq)
		if err := batch.Set(snapCompleteKey, seqBuf, nil); err != nil {
			batch.Close()
			return err
		}
		if err := batch.Commit(pebble.Sync); err != nil {
			batch.Close()
			return err
		}
		batch.Close()
	} else {
		if err := batch.Commit(pebble.NoSync); err != nil {
			batch.Close()
			return err
		}
		batch.Close()
	}
	return nil
}

// IsSnapshotComplete returns true when the snapshot sentinel key is present in
// db, indicating a complete, fsynced snapshot was received. If absent, the Lobe
// should re-join from scratch on the next connection to Cortex.
func (r *SnapshotReceiver) IsSnapshotComplete() bool {
	_, closer, err := r.db.Get(snapCompleteKey)
	if err != nil {
		return false
	}
	closer.Close()
	return true
}
