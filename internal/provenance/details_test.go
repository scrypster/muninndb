package provenance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/types"
)

func openTestStore(t *testing.T) (*Store, *pebble.DB) {
	t.Helper()
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db), db
}

// TestProvenance_DetailsRoundTrip pins that an entry carrying evolve's
// what-changed-and-why survives a write/read cycle intact, including the
// valid-time boundary (which is NOT the write timestamp).
func TestProvenance_DetailsRoundTrip(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	ws := keys.VaultPrefix("test-vault")
	id := [16]byte(types.NewULID())

	effective := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	pred := types.NewULID().String()

	if err := store.Append(ctx, ws, id, ProvenanceEntry{
		Timestamp: time.Now(),
		Source:    SourceHuman,
		AgentID:   "user:test",
		Operation: "evolve",
		Details: &Details{
			PredecessorID: pred,
			Reason:        "the deploy target moved to us-east-2",
			EffectiveAt:   &effective,
		},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries, err := store.Get(ctx, ws, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	d := entries[0].Details
	if d == nil {
		t.Fatal("Details is nil — the what-changed-and-why did not survive the round trip")
	}
	if d.PredecessorID != pred {
		t.Errorf("PredecessorID = %q, want %q", d.PredecessorID, pred)
	}
	if d.Reason != "the deploy target moved to us-east-2" {
		t.Errorf("Reason = %q", d.Reason)
	}
	if d.EffectiveAt == nil || !d.EffectiveAt.Equal(effective) {
		t.Errorf("EffectiveAt = %v, want %v", d.EffectiveAt, effective)
	}
}

// TestProvenance_LegacyEntryDecodes writes a hand-built pre-Details record —
// byte-for-byte the format entries already on disk use — straight into Pebble
// under the 0x16 key, then reads it back through the new decoder. The legacy
// entry must decode with every old field intact and Details ABSENT (nil), not
// an empty struct: an unknown predecessor reported as a known-empty one is the
// silently-wrong failure this whole change exists to remove.
func TestProvenance_LegacyEntryDecodes(t *testing.T) {
	store, db := openTestStore(t)
	ctx := context.Background()
	ws := keys.VaultPrefix("test-vault")
	id := [16]byte(types.NewULID())

	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	legacy := []byte(`{"Timestamp":"2026-01-02T03:04:05Z","Source":1,"AgentID":"user:mj","Operation":"evolve","Note":"legacy"}`)

	// Sanity: this really is the old shape — no Details key at all.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(legacy, &raw); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if _, ok := raw["Details"]; ok {
		t.Fatal("fixture is not a legacy entry — it already carries Details")
	}

	key := keys.ProvenanceSuffixKey(ws, id, uint64(ts.UnixNano()), 1)
	if err := db.Set(key, legacy, pebble.Sync); err != nil {
		t.Fatalf("Set legacy entry: %v", err)
	}

	entries, err := store.Get(ctx, ws, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 — a legacy entry must still decode", len(entries))
	}
	e := entries[0]
	if !e.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v", e.Timestamp, ts)
	}
	if e.Source != SourceHuman || e.AgentID != "user:mj" || e.Operation != "evolve" || e.Note != "legacy" {
		t.Errorf("legacy fields did not survive: %+v", e)
	}
	if e.Details != nil {
		t.Errorf("Details = %+v, want nil — a legacy entry knows no predecessor/reason and must not claim one", e.Details)
	}
}

// TestProvenance_NewEntryIsOldBinaryReadable pins the other compat direction:
// a Details-carrying entry must still decode into the pre-Details struct shape
// (an older binary ignores the unknown key rather than failing the read).
func TestProvenance_NewEntryIsOldBinaryReadable(t *testing.T) {
	effective := time.Now()
	blob, err := json.Marshal(ProvenanceEntry{
		Timestamp: time.Now(),
		Source:    SourceHuman,
		AgentID:   "user:test",
		Operation: "evolve",
		Details:   &Details{PredecessorID: "01ABC", Reason: "why", EffectiveAt: &effective},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// The pre-Details struct, as an old binary would define it.
	var old struct {
		Timestamp time.Time
		Source    SourceType
		AgentID   string
		Operation string
		Note      string
	}
	if err := json.Unmarshal(blob, &old); err != nil {
		t.Fatalf("a new entry must decode on an old binary, got: %v", err)
	}
	if old.Operation != "evolve" || old.AgentID != "user:test" {
		t.Errorf("old-shape decode lost fields: %+v", old)
	}
}

// TestProvenance_DetailsOmittedWhenUnset pins that entries with no details add
// no bytes and no empty object to the stored record.
func TestProvenance_DetailsOmittedWhenUnset(t *testing.T) {
	blob, err := json.Marshal(ProvenanceEntry{
		Timestamp: time.Now(),
		Source:    SourceInferred,
		Operation: "update-relevance",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := raw["Details"]; ok {
		t.Errorf("Details key present on an entry that has none: %s", blob)
	}
}
