package storage

import "sync/atomic"

// rmwHook is a TEST-ONLY seam inside every read-modify-write of an engram
// record: testRMWPoint runs after the writer has read the record it will
// rewrite and before it commits. Tests land a concurrent writer at exactly
// that point to prove the read->write window is closed (the writer holds the
// engram's stripe lock across it, so the concurrent writer waits), instead of
// racing a sleep. Production never sets it; the cost is one atomic load.
var rmwHook atomic.Pointer[func(op string, ws [8]byte, id ULID)]

func testRMWPoint(op string, ws [8]byte, id ULID) {
	if h := rmwHook.Load(); h != nil {
		(*h)(op, ws, id)
	}
}

func setRMWHookForTest(fn func(op string, ws [8]byte, id ULID)) func() {
	rmwHook.Store(&fn)
	return func() { rmwHook.Store(nil) }
}

// SetRMWHookForTesting is the exported form of the read-modify-write seam,
// for tests in other packages (the engine's restore tests). It must never be
// called outside a test; the returned func restores the nil hook.
func SetRMWHookForTesting(fn func(op string, ws [8]byte, id ULID)) (restore func()) {
	return setRMWHookForTest(fn)
}

// EngramStripeHeldForTesting reports whether the stripe lock covering id is
// held right now (TryLock, released at once). Test-only: an interleaving test
// uses it at the RMW seam to tell "the writer under test holds the lock" (run
// the concurrent writer on a goroutine; it waits) from "it does not" (run it
// inline, inside the window — the lost-update interleaving).
func (ps *PebbleStore) EngramStripeHeldForTesting(id ULID) bool {
	mu := ps.casLocks.For(id[:])
	if mu.TryLock() {
		mu.Unlock()
		return false
	}
	return true
}
