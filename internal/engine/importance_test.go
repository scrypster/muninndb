package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func f32ptr(v float32) *float32 { return &v }

// TestWriteImportanceClampAndQuantize pins the write-path importance rules:
// nil stays 0 (unset), values clamp to [0,1], and an explicit 0.0 (or
// negative) quantizes to 0.01 so the stored 0 keeps meaning "unset".
func TestWriteImportanceClampAndQuantize(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	cases := []struct {
		name string
		in   *float32
		want float32
	}{
		{"unset stays zero", nil, 0},
		{"explicit zero quantizes", f32ptr(0), 0.01},
		{"negative quantizes", f32ptr(-3), 0.01},
		{"in-range stored verbatim", f32ptr(0.42), 0.42},
		{"above one clamps", f32ptr(1.5), 1},
	}
	for _, c := range cases {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:      "imp-test",
			Concept:    "importance " + c.name,
			Content:    "importance content: " + c.name,
			Importance: c.in,
		})
		if err != nil {
			t.Fatalf("%s: Write: %v", c.name, err)
		}
		read, err := eng.Read(ctx, &mbp.ReadRequest{ID: resp.ID, Vault: "imp-test", ReadOnly: true})
		if err != nil {
			t.Fatalf("%s: Read: %v", c.name, err)
		}
		if read.Importance != c.want {
			t.Errorf("%s: stored importance = %v, want %v", c.name, read.Importance, c.want)
		}
	}
}

// TestWriteBatchImportance verifies the batch write path threads importance
// through the same clamp/quantize rules as the single-write path.
func TestWriteBatchImportance(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	reqs := []*mbp.WriteRequest{
		{Vault: "imp-batch", Concept: "a", Content: "batch importance a", Importance: f32ptr(0.9)},
		{Vault: "imp-batch", Concept: "b", Content: "batch importance b"},
		{Vault: "imp-batch", Concept: "c", Content: "batch importance c", Importance: f32ptr(0)},
	}
	resps, errs := eng.WriteBatch(ctx, reqs)
	for i, e := range errs {
		if e != nil {
			t.Fatalf("WriteBatch[%d]: %v", i, e)
		}
	}
	want := []float32{0.9, 0, 0.01}
	for i, r := range resps {
		read, err := eng.Read(ctx, &mbp.ReadRequest{ID: r.ID, Vault: "imp-batch", ReadOnly: true})
		if err != nil {
			t.Fatalf("Read[%d]: %v", i, err)
		}
		if read.Importance != want[i] {
			t.Errorf("batch item %d: importance = %v, want %v", i, read.Importance, want[i])
		}
	}
}

// TestEvolveImportanceInheritance pins the evolve rules: nil inherits the
// predecessor's explicit importance verbatim (including unset staying unset),
// and an explicit override wins with the same clamp/quantize rules as Write.
func TestEvolveImportanceInheritance(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "imp-evolve"

	readImp := func(id string) float32 {
		t.Helper()
		read, err := eng.Read(ctx, &mbp.ReadRequest{ID: id, Vault: vault, ReadOnly: true})
		if err != nil {
			t.Fatalf("Read %s: %v", id, err)
		}
		return read.Importance
	}

	// Explicit predecessor, nil override → inherited.
	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "decision", Content: "importance evolve base", Importance: f32ptr(0.8)})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	newID, err := eng.EvolveAt(ctx, vault, resp.ID, "importance evolve v2", "update", nil, "", time.Time{}, nil)
	if err != nil {
		t.Fatalf("EvolveAt: %v", err)
	}
	if got := readImp(newID.String()); got != 0.8 {
		t.Errorf("inherited importance = %v, want 0.8", got)
	}

	// Explicit override wins.
	overrideID, err := eng.EvolveAt(ctx, vault, newID.String(), "importance evolve v3", "update", nil, "", time.Time{}, f32ptr(0.3))
	if err != nil {
		t.Fatalf("EvolveAt override: %v", err)
	}
	if got := readImp(overrideID.String()); got != 0.3 {
		t.Errorf("override importance = %v, want 0.3", got)
	}

	// Unset predecessor stays unset (0) — the type-derived default is never
	// frozen into the successor record.
	plain, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "plain", Content: "importance evolve unset base"})
	if err != nil {
		t.Fatalf("Write plain: %v", err)
	}
	plainV2, err := eng.EvolveAt(ctx, vault, plain.ID, "importance evolve unset v2", "update", nil, "", time.Time{}, nil)
	if err != nil {
		t.Fatalf("EvolveAt plain: %v", err)
	}
	if got := readImp(plainV2.String()); got != 0 {
		t.Errorf("unset predecessor: successor importance = %v, want 0 (unset)", got)
	}
}
