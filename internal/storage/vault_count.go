package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// The vault count model.
//
// Truth is the number of a vault's 0x01 records (soft-deleted ones included).
// The in-memory vaultCounter is the live value of that truth for this
// process, and the 9-byte 0x15 key is a cache of it that is trusted only after
// a clean shutdown. Four rules keep the three in step:
//
//  1. Only the coalescer's sweep writes 0x15 in steady state, and it saves the
//     counter's value at save time (counter_coalescer.go).
//  2. A count change is atomic with its commit per vault: every incremental
//     writer holds the vault's count lock shared from before its seed until
//     after its Add (holdCounts). It is the outermost lock a writer takes.
//  3. Bulk operations (ClearVault, clone, merge, import) never compute a
//     count. When their data is in place they reset the vault's count under
//     the lock held exclusively (resetVaultCount), and the next access seeds
//     from a scan.
//  4. A start without a valid clean-shutdown marker recounts every vault from
//     its records before anything reads a count (reconcileCountsAtOpen).

// countLock returns ws's count lock. One mutex per vault, so a long bulk
// operation on one vault never blocks writes to another.
func (ps *PebbleStore) countLock(ws [8]byte) *sync.RWMutex {
	if v, ok := ps.countLocks.Load(ws); ok {
		return v.(*sync.RWMutex)
	}
	v, _ := ps.countLocks.LoadOrStore(ws, &sync.RWMutex{})
	return v.(*sync.RWMutex)
}

// countHold is a writer's shared hold on the count locks of the vaults it is
// about to change, with their counters seeded.
type countHold struct {
	ps       *PebbleStore
	locks    []*sync.RWMutex
	counters map[[8]byte]*vaultCounter
}

// holdCounts takes the count locks of wss shared, in byte order so two
// writers spanning the same vaults cannot deadlock against a pending reset,
// and seeds each counter. It must be called before the writer commits and
// before it takes any other lock. Seeding before the commit is what keeps
// the seed scan from seeing, and so double-counting, the writer's own records.
func (ps *PebbleStore) holdCounts(ctx context.Context, wss ...[8]byte) *countHold {
	uniq := make([][8]byte, 0, len(wss))
	seen := make(map[[8]byte]struct{}, len(wss))
	for _, ws := range wss {
		if _, ok := seen[ws]; !ok {
			seen[ws] = struct{}{}
			uniq = append(uniq, ws)
		}
	}
	sort.Slice(uniq, func(i, j int) bool { return string(uniq[i][:]) < string(uniq[j][:]) })
	h := &countHold{ps: ps, counters: make(map[[8]byte]*vaultCounter, len(uniq))}
	for _, ws := range uniq {
		l := ps.countLock(ws)
		l.RLock()
		h.locks = append(h.locks, l)
		h.counters[ws] = ps.getOrInitCounter(ctx, ws)
	}
	return h
}

// add applies a committed change to ws's counter and marks the vault dirty.
// Called only after the commit and while the hold is in place, so the counter
// is still the live one.
func (h *countHold) add(ws [8]byte, delta int64) {
	vc := h.counters[ws]
	if vc == nil {
		return
	}
	if n := vc.count.Add(delta); n < 0 {
		vc.count.Store(0)
	}
	if h.ps.counterFlush != nil {
		h.ps.counterFlush.Submit(ws)
	}
}

func (h *countHold) release() {
	for i := len(h.locks) - 1; i >= 0; i-- {
		h.locks[i].RUnlock()
	}
	h.locks = nil
}

// liveVaultCount is the sweep's view of ws: the counter's current value, or
// false when there is no counter or its seed has not finished.
func (ps *PebbleStore) liveVaultCount(ws [8]byte) (int64, bool) {
	v, ok := ps.vaultCounters.Load(ws)
	if !ok {
		return 0, false
	}
	vc := v.(*vaultCounter)
	if !vc.ready.Load() {
		return 0, false
	}
	return vc.count.Load(), true
}

// resetVaultCount forgets everything known about ws's count once a bulk
// operation has finished changing its records. Under the exclusive count lock
// no incremental writer is between its commit and its Add, so the scan the
// next access runs is exact. Delete waits for a sweep that may have read the
// old count, and the key is deleted after it.
func (ps *PebbleStore) resetVaultCount(ws [8]byte) error {
	l := ps.countLock(ws)
	l.Lock()
	defer l.Unlock()
	// Order matters. Readers (GetVaultCount: every recall, metrics, stats,
	// PruneVault) take no count lock, and one that finds no counter seeds a
	// new one from the saved key if there is one. So the key goes first and
	// the counter last: until the counter is dropped a reader sees the old
	// live counter and seeds nothing. Dropping the counter first let a reader
	// rebuild it from the key about to be deleted, and the next sweep saved
	// that stale count back (44 for 45 after a merge, 51 for 1 after a clear).
	if ps.counterFlush != nil {
		ps.counterFlush.Delete(ws) // waits for a sweep in flight
	}
	if err := ps.db.Delete(keys.VaultCountKey(ws), pebble.Sync); err != nil {
		return fmt.Errorf("reset vault count: %w", err)
	}
	ps.vaultCounters.Delete(ws)
	return nil
}

// ResetAllVaultCounts makes every vault's count come from a scan of its
// records on next access, for the rest of the process. A cluster follower
// applies the leader's batches as raw records and never moves its own counts,
// so it is called when a follower becomes the leader: only a leader prunes,
// and a stale count under max_engrams prunes real memories.
//
// It writes nothing. It runs inside the promotion, while the node holds its
// applier and election lock and no node in the cluster accepts a write, and
// it used to delete every saved count with a synced write: 5.6 s at 1,000
// vaults. Durable deletes are not needed. The saved keys are ignored for the
// rest of the process (countKeysUntrusted), and they are trusted again only
// after a clean Close writes the marker, which a store that applied foreign
// commits never does (STO-30). A crash recounts regardless.
//
// The mode is set BEFORE the counters are dropped, so a reader that seeds a
// new counter after the drop scans. Each counter is dropped under its vault's
// exclusive count lock, so no incremental writer is between its seed and its
// Add. The coalescer's entries are left alone: a pending change is saved from
// the next counter, or leaves Close without a marker. Nothing here can fail,
// so no vault is left stale by an error part way.
func (ps *PebbleStore) ResetAllVaultCounts() {
	ps.countKeysUntrusted.Store(true)
	ps.dropAllCounters()
}

func (ps *PebbleStore) dropAllCounters() {
	ps.vaultCounters.Range(func(k, _ any) bool {
		ps.dropCounter(k.([8]byte))
		return true
	})
}

// dropCounter forgets ws's in-memory count under the exclusive count lock.
// Only safe while countKeysUntrusted is set: the next access seeds from a
// scan, never from a saved key that may be stale.
func (ps *PebbleStore) dropCounter(ws [8]byte) {
	l := ps.countLock(ws)
	l.Lock()
	ps.vaultCounters.Delete(ws)
	l.Unlock()
}

// dropCountersTouchedBy runs for a batch committed past the store's write path
// (the replication applier). On a follower it does nothing. Once the store
// seeds counts from scans (after promotion), a late entry from a deposed
// leader is applied without moving any count, so the counter of every vault
// whose records the batch touches is dropped and the next read rescans. An
// unreadable batch drops every counter.
func (ps *PebbleStore) dropCountersTouchedBy(repr []byte) {
	if !ps.countKeysUntrusted.Load() || len(repr) == 0 {
		return
	}
	touched := make(map[[8]byte]struct{})
	r, _ := pebble.ReadBatch(repr)
	for {
		kind, k, _, ok, err := r.Next()
		if err != nil {
			ps.dropAllCounters()
			return
		}
		if !ok {
			break
		}
		if kind == pebble.InternalKeyKindLogData || len(k) < 9 || k[0] != prefix.Engram {
			continue
		}
		// A record key is ws|id. A range delete starts at a vault's records.
		var ws [8]byte
		copy(ws[:], k[1:9])
		touched[ws] = struct{}{}
	}
	for ws := range touched {
		ps.dropCounter(ws)
	}
}

// cleanShutdownKey marks that the 0x15 keys were saved by a clean Close. It is
// a global singleton in the 0xFF namespace, like the migration version key,
// so no prefix scan of any binary reaches it. Its value is an FNV-64 of every
// 9-byte 0x15 key and value at Close: an older binary never deletes the
// marker, so a run of one that changed any count is caught by the mismatch.
var cleanShutdownKey = []byte{0xFF, 'v', 'c', 'o', 'u', 'n', 't', '_', 'c', 'l', 'e', 'a', 'n'}

// countKeysFingerprint hashes every 9-byte 0x15 key and its value.
func countKeysFingerprint(db *pebble.DB) (uint64, error) {
	iter, err := db.NewIter(&pebble.IterOptions{LowerBound: []byte{prefix.VaultCount}, UpperBound: []byte{prefix.VaultCount + 1}})
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	h := fnv.New64a()
	for valid := iter.First(); valid; valid = iter.Next() {
		if len(iter.Key()) != 9 {
			continue
		}
		h.Write(iter.Key())
		h.Write(iter.Value())
	}
	return h.Sum64(), iter.Error()
}

// writeCleanShutdownMarker is called by Close after the final sweep, and only
// when nothing was left unsaved.
func (ps *PebbleStore) writeCleanShutdownMarker() {
	fp, err := countKeysFingerprint(ps.db)
	if err != nil {
		slog.Warn("storage: vault count marker not written, the next start recounts", "err", err)
		return
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], fp)
	if err := ps.db.Set(cleanShutdownKey, buf[:], pebble.Sync); err != nil {
		slog.Warn("storage: vault count marker not written, the next start recounts", "err", err)
	}
}

// reconcileCountsAtOpen runs before the coalescer starts and before any count
// is read. A valid marker means the saved counts are what a clean Close left:
// the marker is deleted (so a crash before the next clean Close recounts) and
// nothing else is done. Otherwise every vault is recounted from its records.
// It reports whether it recounted.
func (ps *PebbleStore) reconcileCountsAtOpen() bool {
	val, closer, err := ps.db.Get(cleanShutdownKey)
	trusted := false
	present := err == nil
	if present {
		if len(val) == 8 {
			fp, ferr := countKeysFingerprint(ps.db)
			trusted = ferr == nil && fp == binary.BigEndian.Uint64(val)
		}
		closer.Close()
	}
	// A read-only open (twin-replay over a backup) can neither delete the
	// marker nor rewrite counts. Without a valid marker it seeds every count by
	// a scan instead of trusting the saved keys, and it writes nothing.
	if werr := ps.db.LogData(nil, pebble.NoSync); errors.Is(werr, pebble.ErrReadOnly) {
		ps.readOnly = true
		if !trusted {
			ps.countKeysUntrusted.Store(true)
			slog.Info("storage: read-only open without a clean-shutdown marker, vault counts are taken from a scan")
		}
		return false
	}
	if present {
		if derr := ps.db.Delete(cleanShutdownKey, pebble.Sync); derr != nil {
			slog.Warn("storage: could not clear the vault count marker", "err", derr)
		}
	}
	if trusted {
		return false
	}
	if err := ps.recountAllVaults(); err != nil {
		slog.Error("storage: vault count recount failed, saved counts may be wrong", "err", err)
	}
	return true
}

// recountAllVaults sets every vault's 0x15 key to the number of its 0x01
// records, in one pass over each range, and deletes a key whose vault has no
// records. Each correction is logged with the vault's name.
func (ps *PebbleStore) recountAllVaults() error {
	db := ps.db
	counts := make(map[[8]byte]int64)
	iter, err := db.NewIter(&pebble.IterOptions{LowerBound: []byte{prefix.Engram}, UpperBound: []byte{prefix.Engram + 1}})
	if err != nil {
		return err
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		if k := iter.Key(); len(k) >= 25 {
			var ws [8]byte
			copy(ws[:], k[1:9])
			counts[ws]++
		}
	}
	if err := iter.Error(); err != nil {
		iter.Close()
		return err
	}
	iter.Close()

	saved := make(map[[8]byte]int64)
	citer, err := db.NewIter(&pebble.IterOptions{LowerBound: []byte{prefix.VaultCount}, UpperBound: []byte{prefix.VaultCount + 1}})
	if err != nil {
		return err
	}
	for valid := citer.First(); valid; valid = citer.Next() {
		k := citer.Key()
		if len(k) != 9 {
			continue // the episode keyspace shares this byte
		}
		var ws [8]byte
		copy(ws[:], k[1:9])
		if v := citer.Value(); len(v) == 8 {
			saved[ws] = int64(binary.BigEndian.Uint64(v))
		} else {
			saved[ws] = -1
		}
	}
	if err := citer.Error(); err != nil {
		citer.Close()
		return err
	}
	citer.Close()

	batch := db.NewBatch()
	defer batch.Close()
	corrected := 0
	for ws, n := range counts {
		if old, ok := saved[ws]; ok && old == n {
			continue
		}
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(n))
		if err := batch.Set(keys.VaultCountKey(ws), buf[:], nil); err != nil {
			return err
		}
		if old, ok := saved[ws]; ok {
			corrected++
			slog.Info("storage: vault memory count corrected at start", "vault", vaultNameForLog(db, ws), "saved", old, "stored", n)
		}
	}
	for ws, old := range saved {
		if _, ok := counts[ws]; ok {
			continue
		}
		if err := batch.Delete(keys.VaultCountKey(ws), nil); err != nil {
			return err
		}
		if old != 0 { // a saved zero for an empty vault was right, only removed
			corrected++
			slog.Info("storage: vault memory count corrected at start", "vault", vaultNameForLog(db, ws), "saved", old, "stored", 0)
		}
	}
	if err := ps.commitBatch(batch, pebble.Sync); err != nil {
		return err
	}
	slog.Info("storage: vault memory counts recounted after an unclean or first start", "vaults", len(counts), "corrected", corrected)
	return nil
}

// vaultNameForLog returns ws's vault name, or the prefix in hex.
func vaultNameForLog(db *pebble.DB, ws [8]byte) string {
	val, closer, err := db.Get(keys.VaultMetaKey(ws))
	if err != nil {
		return fmt.Sprintf("%x", ws)
	}
	defer closer.Close()
	return string(val)
}
