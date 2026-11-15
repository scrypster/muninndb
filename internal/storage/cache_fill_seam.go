package storage

import "sync/atomic"

// fillHook is a TEST-ONLY seam: when set, testFillPoint calls it after a read
// path has obtained a value from Pebble and immediately before that value is
// installed in a cache (L1 or metaCache). Tests use it to land a concurrent
// write at exactly that point, deterministically, instead of racing a sleep.
// Production never sets it; the cost on the miss path is one atomic load.
var fillHook atomic.Pointer[func(ws [8]byte, id ULID)]

func testFillPoint(ws [8]byte, id ULID) {
	if h := fillHook.Load(); h != nil {
		(*h)(ws, id)
	}
}

// setFillHookForTest installs fn as the fill hook and returns a restore func.
func setFillHookForTest(fn func(ws [8]byte, id ULID)) func() {
	fillHook.Store(&fn)
	return func() { fillHook.Store(nil) }
}

// SetCacheFillHookForTesting is the exported form of the fill seam, for
// tests in OTHER packages (the engine's forget-vs-recall test) that need to
// land a write between a read's Pebble load and its cache fill. It must never
// be called outside a test; the returned func restores the nil hook.
func SetCacheFillHookForTesting(fn func(ws [8]byte, id ULID)) (restore func()) {
	return setFillHookForTest(fn)
}

// snapshotPinHook is a TEST-ONLY seam inside NewSnapshotContext, between the
// fill token and the snapshot pin. A write run from it lands in exactly the
// window whose ordering STO-22 depends on: token first, then pin.
var snapshotPinHook atomic.Pointer[func()]

func testSnapshotPinPoint() {
	if h := snapshotPinHook.Load(); h != nil {
		(*h)()
	}
}
