package wal

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	MOLMagic        uint32 = 0x4D4F4C20 // "MOL "
	EntryHeaderSize        = 32 + 4     // 32 bytes header + 4 bytes CRC32 trailer

	OpEngramWrite  uint16 = 0x0001
	OpEngramUpdate uint16 = 0x0002
	OpEngramForget uint16 = 0x0003
	OpEngramPurge  uint16 = 0x0004
	OpAssocLink    uint16 = 0x0005
	OpAssocUnlink  uint16 = 0x0006
	OpHebbianBatch uint16 = 0x0007
	OpDecayBatch   uint16 = 0x0008
	OpVaultCreate  uint16 = 0x0009
	OpVaultUpdate  uint16 = 0x000A
	OpCheckpoint   uint16 = 0x00FF

	FlagCompressed uint8 = 1 << 0
	FlagLargeBatch uint8 = 1 << 1
	FlagCheckpoint uint8 = 1 << 2

	DefaultMaxSegmentSize int64 = 256 * 1024 * 1024 // 256 MB
	DefaultMaxWait              = 2 * time.Millisecond
	DefaultMaxGroupSize         = 1000
)

var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// MOLEntry is one logical operation in the write-ahead log.
type MOLEntry struct {
	SeqNum     uint64
	Timestamp  int64 // Unix nanoseconds
	OpType     uint16
	VaultID    uint32
	PayloadLen uint32
	Flags      uint8
	Payload    []byte // msgpack-encoded operation payload
}

// MOLSegment is an append-only log file.
type MOLSegment struct {
	mu      sync.Mutex
	file    *os.File
	path    string
	size    int64
	nextSeq uint64
}

// MOL manages the MuninnDB Operation Log.
type MOL struct {
	mu            sync.Mutex
	dir           string
	active        *MOLSegment
	nextSeq       atomic.Uint64
	SealThreshold int64 // Segment rotation threshold in bytes (default: 256MB)
}

// Open opens or creates the MOL in the given directory.
// On open, the active segment is scanned to recover the highest sequence number
// so that subsequent Append calls do not produce duplicate sequence numbers.
func Open(dir string) (*MOL, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("mol: mkdir: %w", err)
	}

	mol := &MOL{
		dir:           dir,
		SealThreshold: DefaultMaxSegmentSize, // 256 MB by default
	}

	// Try to open existing active segment or create new one
	activePath := filepath.Join(dir, "mol-active.log")
	f, err := os.OpenFile(activePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("mol: open active segment: %w", err)
	}

	info, _ := f.Stat()
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	mol.active = &MOLSegment{
		file: f,
		path: activePath,
		size: size,
	}

	// Recover nextSeq from existing entries (active + sealed segments).
	maxSeq, found := mol.recoverMaxSeq()
	if found {
		mol.nextSeq.Store(maxSeq + 1)
	}

	return mol, nil
}

// recoverMaxSeq scans all segment files to find the highest sequence number.
// Returns (maxSeq, true) if any entries were found, or (0, false) for an empty MOL.
func (m *MOL) recoverMaxSeq() (uint64, bool) {
	var maxSeq uint64
	found := false

	pattern := filepath.Join(m.dir, "mol-*.log")
	matches, _ := filepath.Glob(pattern)

	allFiles := append(matches, m.active.path)
	seen := make(map[string]bool)
	for _, path := range allFiles {
		if seen[path] {
			continue
		}
		seen[path] = true

		m.scanSegmentMaxSeq(path, &maxSeq, &found)
	}

	return maxSeq, found
}

func (m *MOL) scanSegmentMaxSeq(path string, maxSeq *uint64, found *bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	headerBuf := make([]byte, 32)
	for {
		if _, err := io.ReadFull(f, headerBuf); err != nil {
			break
		}
		magic := binary.BigEndian.Uint32(headerBuf[0:4])
		if magic != MOLMagic {
			break
		}
		seqNum := binary.BigEndian.Uint64(headerBuf[4:12])
		*found = true
		if seqNum > *maxSeq {
			*maxSeq = seqNum
		}
		payloadLen := binary.BigEndian.Uint32(headerBuf[26:30])
		const maxPayloadLen = 64 << 20
		if payloadLen > maxPayloadLen {
			break
		}
		skipLen := int64(payloadLen) + 4
		if _, err := f.Seek(skipLen, io.SeekCurrent); err != nil {
			break
		}
	}
}

// Append writes a MOLEntry to the active segment.
func (m *MOL) Append(entry *MOLEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry.SeqNum = m.nextSeq.Add(1) - 1
	entry.Timestamp = time.Now().UnixNano()

	data, err := marshalEntry(entry)
	if err != nil {
		return fmt.Errorf("mol: marshal entry: %w", err)
	}

	seg := m.active
	seg.mu.Lock()
	defer seg.mu.Unlock()

	if _, err := seg.file.Write(data); err != nil {
		return fmt.Errorf("mol: write: %w", err)
	}
	seg.size += int64(len(data))
	return nil
}

// Sync flushes the active segment to disk.
func (m *MOL) Sync() error {
	m.mu.Lock()
	seg := m.active
	m.mu.Unlock()

	seg.mu.Lock()
	defer seg.mu.Unlock()
	return seg.file.Sync()
}

// Close closes the MOL.
func (m *MOL) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		return m.active.file.Close()
	}
	return nil
}

// MaybeSealSegment checks if the active segment exceeds the threshold and rotates it.
// If the active segment size exceeds SealThreshold, it is renamed to mol-{seqNum}.log
// and a new active segment is created.
func (m *MOL) MaybeSealSegment() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// If active segment is below threshold, no-op
	if m.active.size < m.SealThreshold {
		return nil
	}

	// Close the current active file
	if err := m.active.file.Close(); err != nil {
		return fmt.Errorf("mol: close active segment: %w", err)
	}

	// Rename mol-active.log to mol-{seqNum}.log
	// Use nextSeq-1 since nextSeq was already incremented
	seqNum := m.nextSeq.Load() - 1
	sealedPath := filepath.Join(m.dir, fmt.Sprintf("mol-%d.log", seqNum))
	if err := os.Rename(m.active.path, sealedPath); err != nil {
		return fmt.Errorf("mol: rename segment: %w", err)
	}

	// Open new active segment
	activePath := filepath.Join(m.dir, "mol-active.log")
	f, err := os.OpenFile(activePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("mol: open new active segment: %w", err)
	}

	m.active = &MOLSegment{
		file: f,
		path: activePath,
		size: 0,
	}

	return nil
}

// SafePrune deletes sealed segments that are fully replicated.
// minReplicatedSeq is the minimum confirmed sequence number across all replicas.
// Only segments where every entry has seq <= minReplicatedSeq are deleted.
// Returns the number of segments pruned and any error.
func (m *MOL) SafePrune(minReplicatedSeq uint64) (int, error) {
	if minReplicatedSeq == 0 {
		return 0, nil
	}

	pattern := filepath.Join(m.dir, "mol-*.log")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return 0, fmt.Errorf("mol: glob sealed segments: %w", err)
	}

	pruned := 0
	for _, path := range matches {
		base := filepath.Base(path)
		if base == "mol-active.log" {
			continue
		}

		seqStr := strings.TrimPrefix(base, "mol-")
		seqStr = strings.TrimSuffix(seqStr, ".log")
		segSeq, err := strconv.ParseUint(seqStr, 10, 64)
		if err != nil {
			continue // not a valid sealed segment filename
		}

		if segSeq <= minReplicatedSeq {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return pruned, fmt.Errorf("mol: remove sealed segment %s: %w", base, err)
			}
			slog.Info("mol: pruned sealed segment", "file", base, "seg_seq", segSeq, "min_replicated", minReplicatedSeq)
			pruned++
		}
	}

	return pruned, nil
}

// marshalEntry serializes a MOLEntry into bytes with header and CRC32.
func marshalEntry(entry *MOLEntry) ([]byte, error) {
	header := make([]byte, 32)
	binary.BigEndian.PutUint32(header[0:4], MOLMagic)
	binary.BigEndian.PutUint64(header[4:12], entry.SeqNum)
	binary.BigEndian.PutUint64(header[12:20], uint64(entry.Timestamp))
	binary.BigEndian.PutUint16(header[20:22], entry.OpType)
	binary.BigEndian.PutUint32(header[22:26], entry.VaultID)
	binary.BigEndian.PutUint32(header[26:30], uint32(len(entry.Payload)))
	header[30] = entry.Flags
	header[31] = 0 // reserved

	var buf bytes.Buffer
	buf.Write(header)
	buf.Write(entry.Payload)

	crc := crc32.Checksum(buf.Bytes(), castagnoliTable)
	crc32Bytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crc32Bytes, crc)
	buf.Write(crc32Bytes)

	return buf.Bytes(), nil
}

// ReadFrom reads all entries after the given sequence number.
func (m *MOL) ReadFrom(afterSeq uint64) ([]*MOLEntry, error) {
	m.mu.Lock()
	seg := m.active
	m.mu.Unlock()

	seg.mu.Lock()
	defer seg.mu.Unlock()

	// Re-open file for reading from start
	f, err := os.Open(seg.path)
	if err != nil {
		return nil, fmt.Errorf("mol: reopen for read: %w", err)
	}
	defer f.Close()

	var entries []*MOLEntry
	headerBuf := make([]byte, 32)

	for {
		_, err := io.ReadFull(f, headerBuf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("mol: read header: %w", err)
		}

		magic := binary.BigEndian.Uint32(headerBuf[0:4])
		if magic != MOLMagic {
			break // corrupted or end of written data
		}

		seqNum := binary.BigEndian.Uint64(headerBuf[4:12])
		timestamp := int64(binary.BigEndian.Uint64(headerBuf[12:20]))
		opType := binary.BigEndian.Uint16(headerBuf[20:22])
		vaultID := binary.BigEndian.Uint32(headerBuf[22:26])
		payloadLen := binary.BigEndian.Uint32(headerBuf[26:30])
		flags := headerBuf[30]

		// Sanity-cap payload size to prevent OOM on corrupted records.
		const maxPayloadLen = 64 << 20 // 64 MB
		if payloadLen > maxPayloadLen {
			break // corrupted record
		}

		payload := make([]byte, payloadLen)
		if payloadLen > 0 {
			if _, err := io.ReadFull(f, payload); err != nil {
				break
			}
		}

		// Read and verify CRC32/Castagnoli.
		crcBuf := make([]byte, 4)
		if _, err := io.ReadFull(f, crcBuf); err != nil {
			break
		}
		storedCRC := binary.BigEndian.Uint32(crcBuf)
		computed := crc32.Checksum(headerBuf, castagnoliTable)
		computed = crc32.Update(computed, castagnoliTable, payload)
		if computed != storedCRC {
			break // corrupted entry — stop reading here
		}

		entry := &MOLEntry{
			SeqNum:    seqNum,
			Timestamp: timestamp,
			OpType:    opType,
			VaultID:   vaultID,
			Flags:     flags,
			Payload:   payload,
		}

		if seqNum > afterSeq {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// GroupCommitter batches concurrent writes into single MOL flushes.
type GroupCommitter struct {
	pending        chan *PendingWrite
	mol            *MOL
	db             *pebble.DB
	maxGroupSize   int
	maxWait        time.Duration
	done           chan struct{}
	stopOnce       sync.Once
	droppedEntries atomic.Uint64 // count of AppendAsync entries dropped due to full queue
}

// DroppedEntries returns the number of WAL entries dropped because the queue was full.
func (gc *GroupCommitter) DroppedEntries() uint64 {
	return gc.droppedEntries.Load()
}

// PendingWrite is a write waiting to be committed.
type PendingWrite struct {
	Entry   MOLEntry
	Payload interface{} // will be msgpack-encoded
	Done    chan error
}

// NewGroupCommitter creates a new GroupCommitter.
func NewGroupCommitter(mol *MOL, db *pebble.DB) *GroupCommitter {
	return &GroupCommitter{
		pending:      make(chan *PendingWrite, 4096),
		mol:          mol,
		db:           db,
		maxGroupSize: DefaultMaxGroupSize,
		maxWait:      DefaultMaxWait,
		done:         make(chan struct{}),
	}
}

// Submit adds a write to the pending queue and waits for commit.
func (gc *GroupCommitter) Submit(entry MOLEntry, payload interface{}) error {
	pw := &PendingWrite{
		Entry:   entry,
		Payload: payload,
		Done:    make(chan error, 1),
	}

	// Encode payload
	if payload != nil {
		data, err := msgpack.Marshal(payload)
		if err != nil {
			return fmt.Errorf("groupcommit: marshal payload: %w", err)
		}
		pw.Entry.Payload = data
		pw.Entry.PayloadLen = uint32(len(data))
	}

	gc.pending <- pw
	return <-pw.Done
}

// Run starts the group commit loop. Call in a goroutine.
// Returns nil when ctx is done or the committer is stopped.
func (gc *GroupCommitter) Run(ctx context.Context) error {
	var batch []*PendingWrite
	ticker := time.NewTicker(gc.maxWait)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Drain any pending items that arrived since the last batch pull.
			for {
				select {
				case pw := <-gc.pending:
					batch = append(batch, pw)
				default:
					goto ctxDrained
				}
			}
		ctxDrained:
			if len(batch) > 0 {
				gc.flush(batch)
			}
			return nil

		case <-gc.done:
			// Drain any pending items that arrived since the last batch pull.
			for {
				select {
				case pw := <-gc.pending:
					batch = append(batch, pw)
				default:
					goto doneDrained
				}
			}
		doneDrained:
			if len(batch) > 0 {
				gc.flush(batch)
			}
			return nil

		case pw := <-gc.pending:
			batch = append(batch, pw)
			// Non-blocking drain: collect any already-queued writes without
			// blocking. The labeled break exits the for loop, not just the
			// select — a bare break inside default would only break the select
			// and cause an infinite loop.
		drain:
			for len(batch) < gc.maxGroupSize {
				select {
				case pw := <-gc.pending:
					batch = append(batch, pw)
				default:
					break drain
				}
			}
			gc.flush(batch)
			batch = batch[:0]

		case <-ticker.C:
			if len(batch) > 0 {
				gc.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

// Stop signals the committer to stop after draining.
func (gc *GroupCommitter) Stop() {
	gc.stopOnce.Do(func() {
		close(gc.done)
	})
}

func (gc *GroupCommitter) flush(batch []*PendingWrite) {
	// Track which entries were successfully appended to avoid double-signaling.
	succeeded := make([]bool, len(batch))
	for i, pw := range batch {
		if err := gc.mol.Append(&pw.Entry); err != nil {
			if pw.Done != nil {
				pw.Done <- err
			}
		} else {
			succeeded[i] = true
		}
	}

	// Single fsync for the whole group
	syncErr := gc.mol.Sync()
	for i, pw := range batch {
		if !succeeded[i] {
			continue // Already signaled with Append error
		}
		if pw.Done == nil {
			continue // Fire-and-forget (AppendAsync) — no caller waiting
		}
		if syncErr != nil {
			pw.Done <- syncErr
		} else {
			pw.Done <- nil
		}
	}
}

// Recover replays all sealed segment entries by calling replayFn for each entry.
// It reads all sealed mol-*.log files in the WAL directory in sequence order
// and calls replayFn for each entry.
func (m *MOL) Recover(db *pebble.DB, replayFn func(*MOLEntry) error) error {
	// Find all sealed segments
	pattern := filepath.Join(m.dir, "mol-*.log")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("mol: glob sealed segments: %w", err)
	}

	// Sort matches by numeric sequence number (not lexicographic).
	sort.Slice(matches, func(i, j int) bool {
		return extractMOLSeq(matches[i]) < extractMOLSeq(matches[j])
	})

	// Replay each sealed segment
	for _, sealedPath := range matches {
		f, err := os.Open(sealedPath)
		if err != nil {
			return fmt.Errorf("mol: open sealed segment %s: %w", sealedPath, err)
		}

		// Read and replay all entries in this segment
		headerBuf := make([]byte, 32)
		for {
			_, err := io.ReadFull(f, headerBuf)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			if err != nil {
				f.Close()
				return fmt.Errorf("mol: read header from %s: %w", sealedPath, err)
			}

			magic := binary.BigEndian.Uint32(headerBuf[0:4])
			if magic != MOLMagic {
				break // corrupted or end of written data
			}

			seqNum := binary.BigEndian.Uint64(headerBuf[4:12])
			timestamp := int64(binary.BigEndian.Uint64(headerBuf[12:20]))
			opType := binary.BigEndian.Uint16(headerBuf[20:22])
			vaultID := binary.BigEndian.Uint32(headerBuf[22:26])
			payloadLen := binary.BigEndian.Uint32(headerBuf[26:30])
			flags := headerBuf[30]

			// Sanity-cap payload size to prevent OOM on corrupted records.
			const maxPayloadLen = 64 << 20 // 64 MB
			if payloadLen > maxPayloadLen {
				f.Close()
				return fmt.Errorf("mol: payload too large (%d bytes) in %s — segment may be corrupted", payloadLen, sealedPath)
			}

			payload := make([]byte, payloadLen)
			if payloadLen > 0 {
				if _, err := io.ReadFull(f, payload); err != nil {
					f.Close()
					return fmt.Errorf("mol: read payload from %s: %w", sealedPath, err)
				}
			}

			// Read and verify CRC32/Castagnoli.
			crcBuf := make([]byte, 4)
			if _, err := io.ReadFull(f, crcBuf); err != nil {
				f.Close()
				return fmt.Errorf("mol: read CRC from %s: %w", sealedPath, err)
			}
			storedCRC := binary.BigEndian.Uint32(crcBuf)
			computed := crc32.Checksum(headerBuf, castagnoliTable)
			computed = crc32.Update(computed, castagnoliTable, payload)
			if computed != storedCRC {
				f.Close()
				return fmt.Errorf("mol: CRC mismatch at seq %d in %s — segment is corrupted", seqNum, sealedPath)
			}

			entry := &MOLEntry{
				SeqNum:    seqNum,
				Timestamp: timestamp,
				OpType:    opType,
				VaultID:   vaultID,
				Flags:     flags,
				Payload:   payload,
			}

			// Call the replay function
			if err := replayFn(entry); err != nil {
				f.Close()
				return fmt.Errorf("mol: replay entry from %s: %w", sealedPath, err)
			}
		}

		f.Close()
	}

	return nil
}

// lastMOLSeqKey is the Pebble key for tracking the last committed MOL seq.
var lastMOLSeqKey = []byte{0xFF, 'l', 'a', 's', 't', '_', 'm', 'o', 'l', '_', 's', 'e', 'q'}

// SaveLastSeq persists the last committed MOL sequence number to Pebble.
func SaveLastSeq(db *pebble.DB, seq uint64) error {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, seq)
	return db.Set(lastMOLSeqKey, buf, pebble.NoSync)
}

// LoadLastSeq reads the last committed MOL sequence number from Pebble.
func LoadLastSeq(db *pebble.DB) uint64 {
	val, closer, err := db.Get(lastMOLSeqKey)
	if err != nil {
		return 0
	}
	defer closer.Close()
	if len(val) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(val)
}

// AppendAsync is a fire-and-forget, non-blocking function that sends entry to gc.pending.
// If the channel is full the entry is dropped, the counter is incremented, and a warning
// is logged — callers can observe gc.DroppedEntries() for back-pressure signals.
func AppendAsync(gc *GroupCommitter, entry *MOLEntry) {
	pw := &PendingWrite{
		Entry: *entry,
		Done:  nil, // fire-and-forget, no waiting for result
	}
	select {
	case gc.pending <- pw:
		// Successfully queued.
	default:
		dropped := gc.droppedEntries.Add(1)
		slog.Warn("wal: AppendAsync queue full — WAL entry dropped",
			"dropped_total", dropped,
			"op_type", entry.OpType,
			"vault_id", entry.VaultID,
		)
	}
}

// extractMOLSeq extracts the numeric sequence from a MOL segment filename
// like "/path/mol-42.log" → 42. Returns 0 on parse failure, which sorts
// unparseable filenames first (safe: they'll fail to open cleanly).
func extractMOLSeq(path string) uint64 {
	base := filepath.Base(path)             // "mol-42.log"
	base = strings.TrimPrefix(base, "mol-") // "42.log"
	base = strings.TrimSuffix(base, ".log") // "42"
	n, _ := strconv.ParseUint(base, 10, 64)
	return n
}
