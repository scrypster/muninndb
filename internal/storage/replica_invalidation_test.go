package storage

import (
	"context"
	"testing"
)

// The end-to-end #869 regression (replicated soft-delete must penetrate a
// follower's warm cache) lives in internal/replication/invalidation_869_test.go
// as an external test package: this package is imported by replication via
// cognitive, so the cross-seam test cannot live here without a cycle.

// Keys that are not engram or meta records must pass through untouched, and
// arbitrary shapes must not panic the invalidator — the applier feeds it every
// key of every replicated batch, including association, index and counter keys.
func TestInvalidateReplicatedKey_IgnoresForeignKeys(t *testing.T) {
	db, err := OpenPebble(t.TempDir(), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	ps := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 10})
	defer ps.Close()

	var ws [8]byte
	id, err := ps.WriteEngram(context.Background(), ws, &Engram{Concept: "c", Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ps.GetEngram(context.Background(), ws, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := ps.cache.Get(ws, id); !ok {
		t.Fatal("engram not cached after read")
	}

	for _, key := range [][]byte{
		nil,
		{},
		{0x01},           // engram prefix, truncated
		make([]byte, 24), // one byte short
		make([]byte, 26), // one byte long
		append([]byte{0x03}, make([]byte, 24)...), // assoc-shaped, 25 bytes, wrong prefix
	} {
		ps.InvalidateReplicatedKey(key)
	}

	if _, ok := ps.cache.Get(ws, id); !ok {
		t.Fatal("foreign keys must not evict unrelated cache entries")
	}
}
