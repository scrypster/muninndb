package storage

import "errors"

// ErrNotFound is the sentinel wrapped by storage "not found" errors (missing
// engram, metadata, etc.). Callers should test for it with errors.Is rather
// than matching on the error string.
var ErrNotFound = errors.New("not found")
