package storage

import (
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func TestArchiveBloom_AddAndMayContain(t *testing.T) {
	b := newArchiveBloom(1000)
	var id1 [16]byte
	id1[0] = 0xAA

	if b.MayContain(id1) {
		t.Error("empty filter should not contain id1")
	}
	b.Add(id1)
	if !b.MayContain(id1) {
		t.Error("filter should contain id1 after Add")
	}

	var id2 [16]byte
	id2[0] = 0xBB
	// id2 may or may not produce a false positive — don't assert on it
	_ = b.MayContain(id2)
}

func TestRebuildArchiveBloom_ScansPrefix(t *testing.T) {
	store := newTestStore(t)
	ws := store.VaultPrefix("bloom-test")
	src := NewULID()
	dst := NewULID()

	// Initially no archived edges.
	bloom := store.RebuildArchiveBloom()
	if bloom.MayContain([16]byte(src)) {
		t.Error("bloom should not contain src before any archive write")
	}

	// Directly write an archive key to simulate an archived edge.
	archiveKey := keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst))
	archiveVal := encodeArchiveValue(RelSupports, 0.9, time.Now(), int32(time.Now().Unix()), 0.8, 1, 0)
	if err := store.db.Set(archiveKey, archiveVal[:], pebble.Sync); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Rebuild: should now find src.
	bloom = store.RebuildArchiveBloom()
	if !bloom.MayContain([16]byte(src)) {
		t.Error("bloom should contain src after archive write")
	}
}
