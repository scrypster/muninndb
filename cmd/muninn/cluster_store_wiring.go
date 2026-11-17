package main

import (
	"log/slog"

	"github.com/scrypster/muninndb/internal/replication"
	"github.com/scrypster/muninndb/internal/storage"
)

// wireApplierToStore connects a follower's applier to the local store. The
// applier writes the leader's batches straight into Pebble, past the store's
// write path. Both consumers run on every attempted commit (success or error)
// and on a resnapshot, in this order: the read-through caches are invalidated
// first (cache_guard.go), then the retired set re-derives each touched entry
// from the stored record (retired.go), which reads Pebble directly, never a
// cache. Either hook also marks the store's vault counts uncertifiable for the
// rest of the process, so its clean Close writes no clean-shutdown marker and
// the next start recounts (STO-30).
func wireApplierToStore(applier *replication.Applier, store *storage.PebbleStore) {
	applier.SetCommitHooks(
		func(repr []byte) {
			store.InvalidateCommittedRepr(repr)
			store.NoteCommittedRepr(repr)
		},
		func() {
			store.InvalidateAllCaches()
			store.ResetAllRetired()
		},
	)
}

// vaultCountResetAtPromotion is the coordinator's BeforeBecomingCortex hook. A
// follower applied the leader's writes as raw records and never moved its own
// vault counts. Only a leader prunes, and a stale count under max_engrams
// prunes real memories, so every count is reset before the role changes and
// is seeded from a scan on next access (STO-30).
func vaultCountResetAtPromotion(store *storage.PebbleStore) func(epoch uint64) {
	return func(epoch uint64) {
		if err := store.ResetAllVaultCounts(); err != nil {
			slog.Error("[cluster] vault count reset at promotion failed", "epoch", epoch, "err", err)
		}
	}
}
