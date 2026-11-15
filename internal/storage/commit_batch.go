package storage

import "github.com/cockroachdb/pebble"

// commitBatch commits b and then hands it to the retired set
// (noteCommittedBatch, retired.go) — whether or not Commit returned an error.
// Telling the set is fail-safe: it never trusts the batch's contents, it only
// learns WHICH engram records the batch touched and re-derives each from the
// record as it now is in Pebble, so a batch that did not land changes nothing
// and a batch that landed despite an error is not missed.
//
// MERGE NOTE: fix/cache-stale-fill defines the same function in
// cache_commit.go (commit, then cache invalidation from the repr, also on
// error) and converts every commit in the package to it. When the two
// branches meet, keep that definition and add ps.noteCommittedBatch(b) after
// its invalidation walk; delete this file.
func (ps *PebbleStore) commitBatch(b *pebble.Batch, opts *pebble.WriteOptions) error {
	err := b.Commit(opts)
	ps.noteCommittedBatch(b)
	return err
}
