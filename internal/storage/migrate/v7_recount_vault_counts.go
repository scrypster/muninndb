package migrate

import (
	"encoding/binary"
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// RecountVaultCounts rewrites every vault's persisted engram count (the 9-byte
// 0x15|ws key) from the engram records actually stored (0x01|ws|id), the same
// quantity the counter's seed scan counts.
//
// Startup trusts a persisted count for the life of the data, so a wrong one
// never heals on its own. Two defects wrote wrong ones: counters were seeded
// after the first write committed, which counted that write twice (one high
// per vault whose first touch was a single write, 2k after a first batch of
// k), and PruneVault deleted real memories against the inflated count; and
// the count coalescer could drop an update landing during a flush, leaving a
// count short. Both are fixed at the source. This corrects what they left
// behind, once.
//
// Only 9-byte 0x15 keys are count keys: the episode keyspace shares the byte
// with longer keys, which are never touched. A count key for a vault with no
// engram records is set to 0. Idempotent.
func RecountVaultCounts(db *pebble.DB) error {
	counts := make(map[[8]byte]int64)
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefix.Engram},
		UpperBound: []byte{prefix.Engram + 1},
	})
	if err != nil {
		return fmt.Errorf("recount vault counts: engram iter: %w", err)
	}
	const engramKeyLen = 25 // 0x01(1) + ws(8) + id(16)
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) != engramKeyLen {
			continue
		}
		var ws [8]byte
		copy(ws[:], k[1:9])
		counts[ws]++
	}
	if err := iter.Error(); err != nil {
		iter.Close()
		return fmt.Errorf("recount vault counts: engram scan: %w", err)
	}
	iter.Close()

	persisted := make(map[[8]byte]int64)
	citer, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefix.VaultCount},
		UpperBound: []byte{prefix.VaultCount + 1},
	})
	if err != nil {
		return fmt.Errorf("recount vault counts: count iter: %w", err)
	}
	for valid := citer.First(); valid; valid = citer.Next() {
		k := citer.Key()
		if len(k) != 9 {
			continue // not a count key
		}
		var ws [8]byte
		copy(ws[:], k[1:9])
		v := citer.Value()
		if len(v) == 8 {
			persisted[ws] = int64(binary.BigEndian.Uint64(v))
		} else {
			persisted[ws] = -1 // unreadable: rewrite it
		}
		if _, ok := counts[ws]; !ok {
			counts[ws] = 0
		}
	}
	if err := citer.Error(); err != nil {
		citer.Close()
		return fmt.Errorf("recount vault counts: count scan: %w", err)
	}
	citer.Close()

	batch := db.NewBatch()
	defer batch.Close()
	corrected := 0
	for ws, n := range counts {
		old, had := persisted[ws]
		if had && old == n {
			continue
		}
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(n))
		if err := batch.Set(keys.VaultCountKey(ws), buf[:], nil); err != nil {
			return fmt.Errorf("recount vault counts: stage: %w", err)
		}
		if had {
			corrected++
			slog.Info("migration v7: vault memory count corrected",
				"vault", vaultNameForLog(db, ws), "persisted", old, "stored", n)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("recount vault counts: commit: %w", err)
	}
	slog.Info("migration v7: vault memory counts recounted", "vaults", len(counts), "corrected", corrected)
	return nil
}

// vaultNameForLog returns the vault's name from its 0x0E meta record, or the
// workspace prefix in hex when it has none.
func vaultNameForLog(db *pebble.DB, ws [8]byte) string {
	val, closer, err := db.Get(keys.VaultMetaKey(ws))
	if err != nil {
		return fmt.Sprintf("%x", ws)
	}
	defer closer.Close()
	return string(val)
}
