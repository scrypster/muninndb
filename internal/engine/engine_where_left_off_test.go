package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestWhereLeftOff_ExcludesSessionLogs is a RED-first regression test for S5:
// muninn_where_left_off must accept an opt-in exclude_type_labels filter so
// callers can keep session-log noise out of the recency scan, while still
// filling out the requested limit with real memories. Default behavior
// (no exclude_type_labels) must be unchanged — see
// TestWhereLeftOff_DefaultExcludeIsNoOp.
func TestWhereLeftOff_ExcludesSessionLogs(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Seed a session-log entry first (oldest LastAccess), then real memories
	// on top of it, so a naive limit=3 scan would normally include the log.
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "test",
		Concept:   "Session log entry",
		Content:   "Routine session bookkeeping, not a real memory.",
		TypeLabel: "session-log",
	})
	if err != nil {
		t.Fatalf("Write session-log: %v", err)
	}

	for i := 0; i < 3; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "test",
			Concept: "Real memory",
			Content: fmt.Sprintf("An actual piece of remembered content #%d.", i),
		})
		if err != nil {
			t.Fatalf("Write real memory %d: %v", i, err)
		}
	}

	// With exclude_type_labels=["session-log"], the log is omitted and the
	// scan keeps going to fill the requested limit with real memories.
	excluded, err := eng.WhereLeftOff(ctx, "test", 3, []string{"session-log"})
	if err != nil {
		t.Fatalf("WhereLeftOff (excluding): %v", err)
	}
	if len(excluded) != 3 {
		t.Fatalf("len(excluded) = %d, want 3 (short scan — exclusion must keep scanning to fill limit)", len(excluded))
	}
	for _, e := range excluded {
		if e.TypeLabel == "session-log" {
			t.Errorf("result contains excluded type_label %q: id=%s", e.TypeLabel, e.ID)
		}
	}

	// With the default (nil/empty) exclude list, the session-log entry is
	// included — no behavior change for existing callers.
	all, err := eng.WhereLeftOff(ctx, "test", 4, nil)
	if err != nil {
		t.Fatalf("WhereLeftOff (default): %v", err)
	}
	foundLog := false
	for _, e := range all {
		if e.TypeLabel == "session-log" {
			foundLog = true
		}
	}
	if !foundLog {
		t.Error("default (no exclusion) scan should still include the session-log entry")
	}
}

// TestWhereLeftOff_DefaultExcludeIsNoOp pins the S5 contract: passing an
// empty/nil exclude_type_labels must produce byte-identical results to the
// pre-S5 behavior (no exclusion applied).
func TestWhereLeftOff_DefaultExcludeIsNoOp(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:     "test",
			Concept:   "Entry",
			Content:   fmt.Sprintf("Content #%d", i),
			TypeLabel: "session-log",
		})
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	withNil, err := eng.WhereLeftOff(ctx, "test", 10, nil)
	if err != nil {
		t.Fatalf("WhereLeftOff(nil): %v", err)
	}
	withEmpty, err := eng.WhereLeftOff(ctx, "test", 10, []string{})
	if err != nil {
		t.Fatalf("WhereLeftOff(empty): %v", err)
	}
	if len(withNil) != len(withEmpty) || len(withNil) != 3 {
		t.Fatalf("withNil=%d withEmpty=%d, want both 3", len(withNil), len(withEmpty))
	}
}
