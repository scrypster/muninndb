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

	_, ws := findVaultWithNonZeroTail(t, store)
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

// wsCapturingHNSW is a real-store-aware fake HNSWIndex: it records every ws it
// is called with and only returns candidates when ws matches the ONE real
// vault prefix it was seeded to recognize. A stub that ignores ws (like
// stubTrigHNSW / mockHNSW elsewhere in this package) cannot distinguish "the
// real prefix" from "the reconstructed-from-uint32 prefix vaultWS produced",
// which is exactly the class of bug #692 fixed and #696 asks for coverage of
// on the sweep path specifically — sweepVault is the caller that also feeds
// ws into hnsw.Search, and that HNSW-keyed lookup was silently broken by the
// same truncation as the two GetMetadata call sites #692 already covers.
type wsCapturingHNSW struct {
	want    [8]byte
	id      storage.ULID
	seenWS  [][8]byte
	callErr error
}

func (h *wsCapturingHNSW) Search(_ context.Context, ws [8]byte, _ []float32, _ int) ([]ScoredID, error) {
	h.seenWS = append(h.seenWS, ws)
	if h.callErr != nil {
		return nil, h.callErr
	}
	if ws != h.want {
		return nil, nil
	}
	return []ScoredID{{ID: h.id, Score: 0.95}}, nil
}

// TestSweepVault_RealStore_UsesFullVaultPrefix is #696's sweep-path
// real-store regression test. handleSweep takes ws from subs[0].WSPrefix
// (the #692 fix) and passes it to sweepVault, which uses it for BOTH
// hnsw.Search AND store.GetMetadata — this test seeds a vault whose real
// SipHash prefix has a non-zero tail (the exact shape that a
// truncated-from-uint32 reconstruction gets wrong) and a real
// *storage.PebbleStore, and requires the fake HNSW to see the untruncated
// prefix before it will hand back a candidate at all.
func TestSweepVault_RealStore_UsesFullVaultPrefix(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-trigger-sweep-realstore-test-*")
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

	_, ws := findVaultWithNonZeroTail(t, store)
	vaultID := binary.BigEndian.Uint32(ws[:4])

	ctx := context.Background()
	eng := &storage.Engram{
		Concept:    "sweep regression",
		Content:    "content for #696",
		Confidence: 0.9,
		Relevance:  0.9,
		LastAccess: time.Now(),
		State:      storage.StateActive,
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	registry := newRegistry()
	deliver := &DeliveryRouter{registry: registry}
	hnsw := &wsCapturingHNSW{want: ws, id: id}

	received := make(chan *ActivationPush, 1)
	sub := &Subscription{
		ID:             "red-696-sweep-sub",
		VaultID:        vaultID,
		WSPrefix:       ws,
		Context:        []string{"sweep context"},
		Threshold:      0.0,
		DeltaThreshold: 0.0,
		embedding:      []float32{0.5, 0.5, 0.5, 0.5},
		Deliver: func(_ context.Context, push *ActivationPush) error {
			received <- push
			return nil
		},
		pushedScores: make(map[storage.ULID]float64),
		rateLimiter:  newTokenBucket(100),
	}
	registry.Add(sub)

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        store,
		hnsw:         hnsw,
		deliver:      deliver,
		writeEvents:  make(chan *EngramEvent, 1),
		cogEvents:    make(chan CognitiveEvent, 1),
		contraEvents: make(chan ContradictEvent, 1),
	}

	// Exercise the SAME path the periodic ticker uses: handleSweep derives ws
	// from subs[0].WSPrefix and hands it to sweepVault, rather than the test
	// constructing ws itself and calling sweepVault directly (which would
	// pass whether or not handleSweep's own derivation were reintroduced as
	// a truncating reconstruction).
	worker.handleSweep(ctx)

	select {
	case push := <-received:
		if push == nil {
			t.Fatal("received nil push")
		}
		if push.Engram == nil || push.Engram.ID != id {
			t.Errorf("push carries the wrong engram: %+v", push)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("#696 regression: no sweep push delivered — hnsw.Search saw ws=%v, want %v "+
			"(the real vault prefix); a truncated-from-uint32 reconstruction would produce a "+
			"different value here and the fake HNSW would correctly refuse to return candidates",
			hnsw.seenWS, ws)
	}
}
