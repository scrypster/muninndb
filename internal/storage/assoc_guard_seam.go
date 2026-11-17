package storage

import "sync/atomic"

// assocGuardHook is a TEST-ONLY seam inside the association writers that check
// committed state under a pair's STO-15 guard lock and then commit: the lazy
// archive restore ("lazy-restore"), the archived supersedes restore
// ("supersedes-restore") and the weight update ("weight-update"). It runs
// after the writer's checks for one pair and before its commit. Tests land a
// concurrent writer at exactly that point to prove the check-to-commit window
// is closed (the writer holds the pair's stripe, so the concurrent writer
// waits), instead of racing a sleep. Production never sets it; the cost is one
// atomic load. The rmwHook precedent (rmw_seam.go).
var assocGuardHook atomic.Pointer[func(op string, ws [8]byte, src, dst ULID)]

func testAssocGuardPoint(op string, ws [8]byte, src, dst ULID) {
	if h := assocGuardHook.Load(); h != nil {
		(*h)(op, ws, src, dst)
	}
}

// SetAssocGuardHookForTesting installs the association guard seam, for tests
// in this package and in others (the engine's restore tests). It must never be
// called outside a test; the returned func restores the nil hook.
func SetAssocGuardHookForTesting(fn func(op string, ws [8]byte, src, dst ULID)) (restore func()) {
	assocGuardHook.Store(&fn)
	return func() { assocGuardHook.Store(nil) }
}

// AssocPairStripeHeldForTesting reports whether the STO-15 guard stripe
// covering the (ws, src, dst) pair is held right now (TryLock, released at
// once). Test-only: an interleaving test uses it at the guard seam to tell
// "the writer under test holds the pair" (run the concurrent writer on a
// goroutine; it waits) from "it does not" (run it inline, inside the window).
func (ps *PebbleStore) AssocPairStripeHeldForTesting(ws [8]byte, src, dst ULID) bool {
	mu := ps.assocGuardLocks.For(assocPairLockKey(ws, src, dst))
	if mu.TryLock() {
		mu.Unlock()
		return false
	}
	return true
}
