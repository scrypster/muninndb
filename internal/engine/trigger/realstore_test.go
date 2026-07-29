package trigger

// Regression test for #692: TriggerWorker.vaultWS() reconstructed the 8-byte
// Pebble workspace prefix from only the 4-byte routing VaultID, zeroing bytes
// 4-7. Real vault prefixes are the full 8-byte SipHash output of
// keys.VaultPrefix (see internal/storage/keys/keys.go), so any vault whose
// prefix has non-zero trailing bytes caused every trigger consumer that does a
// store lookup (handleCognitive, handleContradiction, handleSweep) to look up
// the WRONG key and silently deliver nothing.
//
// The fake/mock TriggerStore used elsewhere in this package ignores the ws
// argument entirely, so it cannot catch this class of bug. This test uses a
// real *storage.PebbleStore on a temp dir instead.

import (
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// findVaultWithNonZeroTail returns a vault name whose real 8-byte SipHash
// prefix (storage.PebbleStore.VaultPrefix) has at least one non-zero byte in
// [4:8]. vaultWS() always zeroed that range, so such a name is required to
// distinguish "reconstructed from uint32" from "the real prefix".
func findVaultWithNonZeroTail(t *testing.T, store *storage.PebbleStore) (name string, ws [8]byte) {
	t.Helper()
	for i := 0; i < 10000; i++ {
		name = "trig-vault-" + storage.NewULID().String()
		ws = store.VaultPrefix(name)
		for _, b := range ws[4:] {
			if b != 0 {
				return name, ws
			}
		}
	}
	t.Fatal("could not find a vault name with a non-zero tail in its SipHash prefix after 10000 tries")
	return "", [8]byte{}
}

// TestNotifyCognitive_RealStore_UsesFullVaultPrefix is the RED-first
// regression test for #692. Against unfixed code it fails: handleCognitive
// derives its store lookup key from vaultWS(event.VaultID), which zeroes
// bytes 4-7 of the real prefix, so GetMetadata misses and no push is
// delivered.
func TestNotifyCognitive_RealStore_UsesFullVaultPrefix(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-trigger-realstore-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	defer store.Close()

	vaultName, ws := findVaultWithNonZeroTail(t, store)
	vaultID := binary.BigEndian.Uint32(ws[:4])

	ctx := context.Background()
	eng := &storage.Engram{
		Concept:    "trigger regression",
		Content:    "content for #692",
		Confidence: 0.9,
		Relevance:  0.9,
		LastAccess: time.Now(),
		State:      storage.StateActive,
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	ts := New(store, &stubTrigFTS{}, &stubTrigHNSW{}, &stubTrigEmbedder{})

	received := make(chan *ActivationPush, 1)
	sub := &Subscription{
		ID:        "red-692-sub",
		VaultID:   vaultID,
		Threshold: 0.0, // always fires once metadata is found
		Deliver: func(_ context.Context, push *ActivationPush) error {
			received <- push
			return nil
		},
	}
	if err := ts.Subscribe(sub); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ts.Start(runCtx)

	_ = vaultName
	// Pass the REAL 8-byte prefix through explicitly — this is the fix for
	// #692. Before the fix, NotifyCognitive had no ws parameter at all and
	// handleCognitive reconstructed (and truncated) the prefix internally
	// from vaultID; see git history / PR description for the RED failure
	// this test produced against that code.
	ts.NotifyCognitive(vaultID, ws, id, "relevance", 0.1, 0.9)

	select {
	case push := <-received:
		if push == nil {
			t.Fatal("received nil push")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("#692 regression: no push delivered — handleCognitive's store lookup used a " +
			"truncated 8-byte prefix (bytes 4-7 zeroed by vaultWS) instead of the real vault " +
			"prefix, so GetMetadata missed the engram written under the real SipHash prefix")
	}
}
