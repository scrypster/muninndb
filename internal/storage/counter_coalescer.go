package storage

import (
	"encoding/binary"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

const counterFlushInterval = 100 * time.Millisecond

// counterCoalescer saves vault counts (the 9-byte 0x15 key) in the background.
//
// The model (vault_count.go has the rest): the number of a vault's 0x01
// records is the truth, the in-memory counter is its live value in this
// process, and the 0x15 key is a cache of that counter. The sweep is the only
// steady-state writer of the key, and it saves the counter's value AT SAVE
// TIME, looked up through live. Submit carries no value: it only marks the
// vault dirty by bumping a generation. Earlier designs saved a value handed
// to them, and a hand-over that lost a race (a delete's older count landing
// after a write's newer one) became the saved count.
type counterCoalescer struct {
	db   *pebble.DB
	m    sync.Map // [8]byte → *counterEntry
	stop chan struct{}
	done chan struct{}

	// live returns a vault's current in-memory count, and false when the
	// vault has no counter or its seed has not finished. A sweep skips such a
	// vault and keeps it dirty.
	live func(ws [8]byte) (int64, bool)

	// flushMu serialises sweeps with each other and with Delete.
	flushMu sync.Mutex

	// beforePersistForTest is a TEST-ONLY seam that runs inside a sweep just
	// before one vault's count is saved. Production never sets it.
	beforePersistForTest func(ws [8]byte)
	// betweenReadsForTest runs between the sweep's read of the change
	// generation and its read of the live count. TEST-ONLY.
	betweenReadsForTest func(ws [8]byte)
}

// counterEntry marks one vault dirty. gen is bumped by every Submit, and savedGen
// is the generation read by the last successful save. A sweep reads gen
// before it reads the live count, so a save covers every Submit it saw.
type counterEntry struct {
	gen      atomic.Uint64
	savedGen atomic.Uint64
	// failing is set while saves of this entry keep failing, so a retry every
	// sweep logs once per failure streak.
	failing atomic.Bool
}

func newCounterCoalescer(db *pebble.DB, live func(ws [8]byte) (int64, bool)) *counterCoalescer {
	c := &counterCoalescer{
		db:   db,
		live: live,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go c.run()
	return c
}

// Submit marks ws dirty. Lock-free and never drops.
func (c *counterCoalescer) Submit(wsPrefix [8]byte) {
	v, _ := c.m.LoadOrStore(wsPrefix, &counterEntry{})
	v.(*counterEntry).gen.Add(1)
}

func (c *counterCoalescer) run() {
	defer close(c.done)

	ticker := time.NewTicker(counterFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.flush()
		case <-c.stop:
			c.flush()
			return
		}
	}
}

func (c *counterCoalescer) flush() {
	defer func() {
		if r := recover(); r != nil {
			msg, ok := r.(string)
			if ok && (strings.Contains(msg, "pebble: closed") || strings.Contains(msg, "pebble: cleaned up")) {
				// Known teardown panic — swallow silently.
				return
			}
			slog.Warn("storage: unexpected panic in counter flush", "panic", r)
		}
	}()
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	var buf [8]byte
	c.m.Range(func(k, v any) bool {
		ws := k.([8]byte)
		e := v.(*counterEntry)
		gen := e.gen.Load()
		if gen == e.savedGen.Load() {
			return true
		}
		if h := c.betweenReadsForTest; h != nil {
			h(ws)
		}
		count, ok := c.live(ws)
		if !ok {
			return true // no seeded counter yet: stay dirty
		}
		if h := c.beforePersistForTest; h != nil {
			h(ws)
		}
		binary.BigEndian.PutUint64(buf[:], uint64(count))
		if err := c.db.Set(keys.VaultCountKey(ws), buf[:], pebble.NoSync); err != nil {
			if !e.failing.Swap(true) {
				slog.Warn("storage: counter flush failed, retrying every sweep until it succeeds", "err", err)
			}
			return true
		}
		e.failing.Store(false)
		e.savedGen.Store(gen)
		return true
	})
}

// clean reports whether every dirty mark has been saved. Close writes the
// clean-shutdown marker only when it has.
func (c *counterCoalescer) clean() bool {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	ok := true
	c.m.Range(func(_, v any) bool {
		e := v.(*counterEntry)
		if e.gen.Load() != e.savedGen.Load() {
			ok = false
			return false
		}
		return true
	})
	return ok
}

// Delete drops ws's entry. It waits for a sweep in progress, which may have
// read the vault's count already, so the caller's next step (deleting the
// key) lands after anything that sweep wrote.
func (c *counterCoalescer) Delete(ws [8]byte) {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.m.Delete(ws)
}

// Close performs a final flush and waits for the flusher goroutine to exit.
// Must be called before db.Close().
func (c *counterCoalescer) Close() {
	close(c.stop)
	<-c.done
}
