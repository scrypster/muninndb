package storage

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/pebble"
)

// IsClosedPanic reports whether a recovered panic value represents a
// closed-DB condition from Pebble. Pebble surfaces this in three ways:
//
//   - An error value wrapping pebble.ErrClosed ("pebble: closed")
//   - A formatted string from applyInternal containing "pebble: closed"
//   - A raw string from the record package: "pebble/record: closed LogWriter"
//
// All three represent the same unrecoverable state: the DB has been closed.
// This function is intentionally broad — any panic mentioning Pebble being
// closed is treated as an expected shutdown condition.
func IsClosedPanic(r any) bool {
	if err, ok := r.(error); ok {
		return errors.Is(err, pebble.ErrClosed)
	}
	s := fmt.Sprintf("%v", r)
	// "pebble: closed" — applyInternal / standard ErrClosed string path.
	// "pebble/record: closed" — LogWriter torn down before the goroutine exits.
	return strings.Contains(s, pebble.ErrClosed.Error()) ||
		strings.Contains(s, "pebble/record: closed")
}
