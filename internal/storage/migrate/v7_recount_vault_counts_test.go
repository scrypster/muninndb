package migrate

import (
	"encoding/binary"
	"testing"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func v7PutEngrams(t *testing.T, db *pebble.DB, ws [8]byte, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		var id [16]byte
		binary.BigEndian.PutUint64(id[8:], uint64(i+1))
		if err := db.Set(keys.EngramKey(ws, id), []byte("record"), nil); err != nil {
			t.Fatal(err)
		}
	}
}

func v7PutCount(t *testing.T, db *pebble.DB, ws [8]byte, n int64) {
	t.Helper()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(n))
	if err := db.Set(keys.VaultCountKey(ws), buf[:], nil); err != nil {
		t.Fatal(err)
	}
}

func v7Count(t *testing.T, db *pebble.DB, ws [8]byte) (int64, bool) {
	t.Helper()
	val, closer, err := db.Get(keys.VaultCountKey(ws))
	if err == pebble.ErrNotFound {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	return int64(binary.BigEndian.Uint64(val)), true
}

// TestRecountVaultCounts_CorrectsEveryShape: the first-write seeding defect
// left persisted counts too high (1 per single first write, 2k after a first
// batch of k), and the coalescer's lost update could leave them short. Startup
// trusts the persisted value forever, so only a recount repairs a vault.
func TestRecountVaultCounts_CorrectsEveryShape(t *testing.T) {
	db := v6TestDB(t)
	inflated := [8]byte{'h', 'i', 'v', 'e', 0, 0, 0, 1}
	batchInflated := [8]byte{'h', 'i', 'v', 'e', 0, 0, 0, 2}
	short := [8]byte{'h', 'i', 'v', 'e', 0, 0, 0, 3}
	exact := [8]byte{'h', 'i', 'v', 'e', 0, 0, 0, 4}
	emptied := [8]byte{'h', 'i', 'v', 'e', 0, 0, 0, 5}
	noKey := [8]byte{'h', 'i', 'v', 'e', 0, 0, 0, 6}

	v7PutEngrams(t, db, inflated, 3)
	v7PutCount(t, db, inflated, 4)
	v7PutEngrams(t, db, batchInflated, 5)
	v7PutCount(t, db, batchInflated, 10)
	v7PutEngrams(t, db, short, 6)
	v7PutCount(t, db, short, 5)
	v7PutEngrams(t, db, exact, 2)
	v7PutCount(t, db, exact, 2)
	v7PutCount(t, db, emptied, 7)
	v7PutEngrams(t, db, noKey, 4)

	// A longer 0x15 key (the episode keyspace shares the byte) must be left alone.
	episodeKey := append([]byte{prefix.VaultCount}, make([]byte, 20)...)
	episodeKey[1] = 'e'
	if err := db.Set(episodeKey, []byte("episode"), nil); err != nil {
		t.Fatal(err)
	}

	if err := RecountVaultCounts(db); err != nil {
		t.Fatalf("RecountVaultCounts: %v", err)
	}

	for _, tc := range []struct {
		name string
		ws   [8]byte
		want int64
	}{
		{"inflated by single writes", inflated, 3},
		{"inflated by a first batch", batchInflated, 5},
		{"short after a lost update", short, 6},
		{"already exact", exact, 2},
		{"no engrams left", emptied, 0},
		{"engrams but no count key", noKey, 4},
	} {
		got, ok := v7Count(t, db, tc.ws)
		if !ok || got != tc.want {
			t.Errorf("%s: persisted count = %d (present=%v), want %d", tc.name, got, ok, tc.want)
		}
	}
	val, closer, err := db.Get(episodeKey)
	if err != nil || string(val) != "episode" {
		t.Errorf("a non-count 0x15 key was modified: %q, %v", val, err)
	}
	if closer != nil {
		closer.Close()
	}

	// Idempotent: a second run changes nothing.
	if err := RecountVaultCounts(db); err != nil {
		t.Fatal(err)
	}
	if got, _ := v7Count(t, db, batchInflated); got != 5 {
		t.Errorf("second run: count = %d, want 5", got)
	}
}

func TestRegisterMigrations_IncludesV7(t *testing.T) {
	r := &Runner{}
	RegisterMigrations(r)
	found := false
	for _, m := range r.migrations {
		if m.Version == 7 {
			found = true
		}
	}
	if !found {
		t.Fatalf("migration v7 (vault count recount) is not registered")
	}
	if got := MaxRegisteredVersion(); got != 7 {
		t.Errorf("MaxRegisteredVersion() = %d; want 7 — the refuse-newer downgrade guard keys off this", got)
	}
}
