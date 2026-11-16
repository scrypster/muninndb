package storage

import "errors"

// ErrNotFound is the sentinel wrapped by storage "not found" errors (missing
// engram, metadata, etc.). Callers should test for it with errors.Is rather
// than matching on the error string.
var ErrNotFound = errors.New("not found")

// ErrRestoreRequired is returned by a state transition that would move a
// soft-deleted (forgotten) memory to any other state. A forget drops the
// memory's keyword postings and only Restore (muninn_restore) puts them back,
// so Restore is the one way out of the soft-deleted state; a plain state
// change would bring the memory back unfindable by keyword.
var ErrRestoreRequired = errors.New("memory is forgotten (soft-deleted); use muninn_restore to bring it back — a state change cannot")
