package storage

import (
	"context"
	"testing"
	"time"
)

// UpdateConfidenceWithContradiction was a second, unguarded 0x0A writer.
//
// FlagContradiction gained the timestamped value format and a carry-forward
// ("the stamp is the moment the contradiction FIRST became known"), but the
// confidence path still wrote bare 16-byte legacy values and OVERWROTE the
// marker unconditionally. Consequences (adversarial review of #754, finding 6):
//
//   - a pair flagged through the confidence path reported detection time
//     "unknown" forever, and
//   - a marker that already carried a real stamp had it ERASED on the next
//     confidence adjustment — the plausible-wrong-value class again, on the
//     exact field the contradiction surface just added.
func TestUpdateConfidenceWithContradiction_PreservesDetectedAt(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{9}

	a := writeConfPathEngram(t, store, ws, "the widget limit is ten")
	b := writeConfPathEngram(t, store, ws, "the widget limit is forty")

	// The pair becomes known via the declared path: a real stamp is recorded.
	newly, err := store.FlagContradiction(ctx, ws, a, b)
	if err != nil || !newly {
		t.Fatalf("FlagContradiction: newly=%v err=%v", newly, err)
	}
	before := readDetectedAt(t, store, ws, a, b)
	if before.IsZero() {
		t.Fatal("declared flag did not record a detection time — fixture broken")
	}

	// A confidence adjustment re-observes the same pair a beat later.
	time.Sleep(5 * time.Millisecond)
	if _, _, err := store.UpdateConfidenceWithContradiction(ctx, ws, a, 0.05, b, true); err != nil {
		t.Fatalf("UpdateConfidenceWithContradiction: %v", err)
	}

	after := readDetectedAt(t, store, ws, a, b)
	if after.IsZero() {
		t.Fatal("confidence path erased the detection time entirely (legacy 16-byte overwrite)")
	}
	if !after.Equal(before) {
		t.Fatalf("confidence path re-stamped the marker: detectedAt %v -> %v. The stamp is the "+
			"moment the contradiction FIRST became known and must survive later adjustments verbatim.",
			before, after)
	}
}

// And the reverse order: a pair whose FIRST observation arrives via the
// confidence path must get a real stamp, not a legacy unknown-forever value.
func TestUpdateConfidenceWithContradiction_StampsFirstObservation(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{9}

	a := writeConfPathEngram(t, store, ws, "the retention window is thirty days")
	b := writeConfPathEngram(t, store, ws, "the retention window is ninety days")

	if _, _, err := store.UpdateConfidenceWithContradiction(ctx, ws, a, 0.05, b, true); err != nil {
		t.Fatalf("UpdateConfidenceWithContradiction: %v", err)
	}
	if readDetectedAt(t, store, ws, a, b).IsZero() {
		t.Fatal("first observation via the confidence path recorded no detection time — it still " +
			"writes the bare legacy value")
	}
}

func readDetectedAt(t *testing.T, store *PebbleStore, ws [8]byte, a, b ULID) time.Time {
	t.Helper()
	recs, err := store.GetContradictionRecords(context.Background(), ws)
	if err != nil {
		t.Fatalf("GetContradictionRecords: %v", err)
	}
	for _, r := range recs {
		if (r.A == a && r.B == b) || (r.A == b && r.B == a) {
			return r.DetectedAt
		}
	}
	t.Fatalf("pair not found in contradiction scan")
	return time.Time{}
}

func writeConfPathEngram(t *testing.T, store *PebbleStore, ws [8]byte, content string) ULID {
	t.Helper()
	id, err := store.WriteEngram(context.Background(), ws, &Engram{
		Concept: "conf-path fixture", Content: content, State: StateActive, Confidence: 1.0,
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	return id
}
