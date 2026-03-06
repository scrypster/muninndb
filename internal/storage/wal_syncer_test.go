package storage

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble"
)

// TestIsClosedPanic covers all panic shapes that Pebble can produce when the
// DB or WAL writer has been closed. Any gap here can cause a real goroutine
// panic to escape the walSyncer recover() and fail tests non-deterministically.
func TestIsClosedPanic(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want bool
	}{
		// Error path — pebble.ErrClosed directly.
		{"ErrClosed direct", pebble.ErrClosed, true},
		// Error path — wrapped pebble.ErrClosed.
		{"ErrClosed wrapped", fmt.Errorf("wrapped: %w", pebble.ErrClosed), true},
		// String path — applyInternal formatted message.
		{"applyInternal string", "pebble: closed", true},
		// String path — record package LogWriter teardown (the flaky-test variant).
		{"record closed LogWriter", "pebble/record: closed LogWriter", true},
		// String path — partial match within longer message.
		{"pebble/record prefix only", "pebble/record: closed", true},
		// Unrelated error — must not be swallowed.
		{"unrelated error", errors.New("some other error"), false},
		// Unrelated string — must not be swallowed.
		{"unrelated string", "index out of range", false},
		// Nil — must not be swallowed.
		{"nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isClosedPanic(tt.val)
			if got != tt.want {
				t.Errorf("isClosedPanic(%v) = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}

// TestWALSyncer_CloseBeforeGoroutineExits verifies that closing the walSyncer
// while a doSync is in flight does not panic. This is the scenario that
// produced the "pebble/record: closed LogWriter" flake: the ticker fires,
// doSync starts, then the store is torn down concurrently.
func TestWALSyncer_CloseBeforeGoroutineExits(t *testing.T) {
	db := openTestPebble(t)
	for range 50 {
		s := newWALSyncer(db)
		// Close immediately — races with any in-flight doSync tick.
		s.Close()
	}
	// If we reach here without panic the race is handled correctly.
}
