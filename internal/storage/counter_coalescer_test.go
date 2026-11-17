package storage

import (
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// liveCounts stands in for the store's counters in coalescer unit tests.
type liveCounts struct {
	mu sync.Mutex
	m  map[[8]byte]int64
}

func newLiveCounts() *liveCounts { return &liveCounts{m: map[[8]byte]int64{}} }

func (l *liveCounts) set(ws [8]byte, n int64) {
	l.mu.Lock()
	l.m[ws] = n
	l.mu.Unlock()
}

func (l *liveCounts) get(ws [8]byte) (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.m[ws]
	return n, ok
}

// manualCoalescer has no ticker: only the test's flush calls sweep.
func manualCoalescer(db *pebble.DB, live *liveCounts) *counterCoalescer {
	return &counterCoalescer{db: db, live: live.get}
}

func savedCount(t *testing.T, db *pebble.DB, ws [8]byte) int64 {
	t.Helper()
	val, closer, err := db.Get(keys.VaultCountKey(ws))
	if err == pebble.ErrNotFound {
		return -1
	}
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	return int64(binary.BigEndian.Uint64(val))
}

func TestCounterCoalescer_FlushWritesToPebble(t *testing.T) {
	db := openTestPebble(t)
	live := newLiveCounts()
	c := manualCoalescer(db, live)
	ws := [8]byte{0xAA, 0xBB}
	live.set(ws, 5)
	c.Submit(ws)
	c.flush()
	if got := savedCount(t, db, ws); got != 5 {
		t.Fatalf("saved count = %d, want 5", got)
	}
}

// The sweep saves the counter's value when it saves, never a value handed to
// it. A hard delete overlapping a write used to hand over its older count
// after the write had handed over the newer one, and the older was saved: on
// the branch that did that, 30 of 60 delete-and-write races saved a count
// one low.
func TestCounterCoalescer_SavesLiveValueNotAHandedOne(t *testing.T) {
	db := openTestPebble(t)
	live := newLiveCounts()
	c := manualCoalescer(db, live)
	ws := [8]byte{0xCA}
	live.set(ws, 5) // a write lands: 5
	c.Submit(ws)
	live.set(ws, 4) // a delete lands after it: 4, its mark is still in flight
	c.flush()
	if got := savedCount(t, db, ws); got != 4 {
		t.Fatalf("saved count = %d, want the live 4", got)
	}
}

// A vault whose counter is not seeded yet is skipped and stays dirty.
func TestCounterCoalescer_UnseededVaultStaysDirty(t *testing.T) {
	db := openTestPebble(t)
	live := newLiveCounts()
	c := manualCoalescer(db, live)
	ws := [8]byte{0xCB}
	c.Submit(ws)
	c.flush()
	if got := savedCount(t, db, ws); got != -1 {
		t.Fatalf("saved %d for a vault with no seeded counter", got)
	}
	if c.clean() {
		t.Fatal("clean() true with an unsaved mark")
	}
	live.set(ws, 3)
	c.flush()
	if got := savedCount(t, db, ws); got != 3 || !c.clean() {
		t.Fatalf("after seeding: saved %d (want 3), clean=%v", got, c.clean())
	}
}

// A mark landing while a sweep is mid-save is saved by the next sweep.
func TestCounterCoalescer_SubmitDuringFlushIsNotLost(t *testing.T) {
	db := openTestPebble(t)
	live := newLiveCounts()
	c := manualCoalescer(db, live)
	ws := [8]byte{0xE1}
	live.set(ws, 27)
	c.Submit(ws)
	fired := false
	c.beforePersistForTest = func([8]byte) {
		if !fired {
			fired = true
			// The sweep has read 27. A write commits and marks the vault.
			live.set(ws, 28)
			c.Submit(ws)
		}
	}
	c.flush()
	c.beforePersistForTest = nil
	if !fired {
		t.Fatal("hook never ran")
	}
	if c.clean() {
		t.Fatal("clean() true while a mark from mid-sweep is unsaved")
	}
	c.flush()
	if got := savedCount(t, db, ws); got != 28 {
		t.Fatalf("saved count = %d, want 28", got)
	}
}

// Sweeps are serialised: a second sweep cannot save while the first is
// mid-flight with the value it read, and then be overwritten by it.
func TestCounterCoalescer_ConcurrentSweepsAreSerialised(t *testing.T) {
	db := openTestPebble(t)
	live := newLiveCounts()
	c := manualCoalescer(db, live)
	ws := [8]byte{0xCC}
	live.set(ws, 5)
	c.Submit(ws)
	fired := false
	second := make(chan struct{})
	c.beforePersistForTest = func([8]byte) {
		if fired {
			return
		}
		fired = true
		live.set(ws, 6)
		c.Submit(ws)
		go func() { c.flush(); close(second) }()
		select { // with flushMu the second sweep cannot finish here
		case <-second:
		case <-time.After(300 * time.Millisecond):
		}
	}
	c.flush()
	<-second
	if got := savedCount(t, db, ws); got != 6 {
		t.Fatalf("saved count = %d, want 6: an older sweep overwrote a newer one", got)
	}
}

func TestCounterCoalescer_MultiVaultFlush(t *testing.T) {
	db := openTestPebble(t)
	live := newLiveCounts()
	c := manualCoalescer(db, live)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		var ws [8]byte
		binary.BigEndian.PutUint64(ws[:], uint64(0xDEAD000000000000)+uint64(i))
		live.set(ws, int64(i))
		wg.Add(1)
		go func() { defer wg.Done(); c.Submit(ws) }()
	}
	wg.Wait()
	c.flush()
	for i := 0; i < 100; i++ {
		var ws [8]byte
		binary.BigEndian.PutUint64(ws[:], uint64(0xDEAD000000000000)+uint64(i))
		if got := savedCount(t, db, ws); got != int64(i) {
			t.Errorf("%s: saved %d, want %d", fmt.Sprintf("%x", ws), got, i)
		}
	}
}

// Guard: the sweep reads the change generation BEFORE the live count. A change
// landing between the two reads is then either in the count it saves or in a
// generation it has not saved, so the next sweep saves it. Read the other way
// round, the sweep would save the older count under the newer generation and
// never save the change.
func TestCounterCoalescer_ChangeBetweenTheSweepsReadsIsSaved(t *testing.T) {
	db := openTestPebble(t)
	live := newLiveCounts()
	c := manualCoalescer(db, live)
	ws := [8]byte{0xCD}
	live.set(ws, 5)
	c.Submit(ws)
	fired := false
	c.betweenReadsForTest = func([8]byte) {
		if !fired {
			fired = true
			live.set(ws, 6)
			c.Submit(ws)
		}
	}
	c.flush()
	c.betweenReadsForTest = nil
	c.flush()
	if !fired {
		t.Fatal("hook never ran")
	}
	if got := savedCount(t, db, ws); got != 6 {
		t.Fatalf("saved count = %d, want 6: a change between the sweep's reads was lost", got)
	}
}
