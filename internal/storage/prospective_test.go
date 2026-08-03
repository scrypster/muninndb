package storage

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// === THE PUSH increment 1: 0x2D armed-intention index =====================
//
// RED-first: these tests were written before internal/storage/prospective.go
// existed and failed to compile (undefined ArmIntention/ScanArmedForEntity/
// MarkIntentionFired/RelinkProspectiveIntent), pinning each behavior.

// TestArmIntention_ScanRoundTrip pins the arm→scan round trip: one 0x2D key
// per cue, retrievable via ScanArmedForEntity under any of its cue entities,
// case-insensitively (entity identity is NormalizeEntityName).
func TestArmIntention_ScanRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := keys.VaultPrefix("prospective-vault")
	id := NewULID()

	if err := store.ArmIntention(ctx, ws, id, []string{"Redis", "cache-layer"}, true); err != nil {
		t.Fatalf("ArmIntention: %v", err)
	}

	for _, cue := range []string{"Redis", "redis", "  REDIS ", "cache-layer"} {
		armed, err := store.ScanArmedForEntity(ctx, ws, cue)
		if err != nil {
			t.Fatalf("ScanArmedForEntity(%q): %v", cue, err)
		}
		if len(armed) != 1 {
			t.Fatalf("ScanArmedForEntity(%q) = %d intentions, want 1", cue, len(armed))
		}
		got := armed[0]
		if got.ID != id {
			t.Errorf("armed ID = %s, want %s", got.ID, id)
		}
		if !got.OneShot {
			t.Errorf("armed OneShot = false, want true")
		}
		if got.FiredCount != 0 {
			t.Errorf("armed FiredCount = %d, want 0", got.FiredCount)
		}
		if len(got.Cues) != 2 {
			t.Errorf("armed Cues = %v, want the 2 armed cues", got.Cues)
		}
	}

	// An entity that was never armed returns nothing.
	other, err := store.ScanArmedForEntity(ctx, ws, "postgres")
	if err != nil {
		t.Fatalf("ScanArmedForEntity(postgres): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("unarmed entity returned %d intentions, want 0", len(other))
	}

	// Vault isolation: another vault sees nothing.
	otherWS := keys.VaultPrefix("some-other-vault")
	cross, err := store.ScanArmedForEntity(ctx, otherWS, "Redis")
	if err != nil {
		t.Fatalf("ScanArmedForEntity(cross-vault): %v", err)
	}
	if len(cross) != 0 {
		t.Errorf("cross-vault scan returned %d intentions, want 0", len(cross))
	}
}

// TestMarkIntentionFired_Recurring pins the recurring (one_shot=false) fire
// path: FiredCount increments, LastFiredAt is stamped, and the cue keys stay
// armed for future sessions.
func TestMarkIntentionFired_Recurring(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := keys.VaultPrefix("prospective-vault")
	id := NewULID()
	cues := []string{"grafana", "alerting"}

	if err := store.ArmIntention(ctx, ws, id, cues, false); err != nil {
		t.Fatalf("ArmIntention: %v", err)
	}
	if err := store.MarkIntentionFired(ctx, ws, id, cues, false); err != nil {
		t.Fatalf("MarkIntentionFired: %v", err)
	}
	if err := store.MarkIntentionFired(ctx, ws, id, cues, false); err != nil {
		t.Fatalf("MarkIntentionFired (2nd): %v", err)
	}

	for _, cue := range cues {
		armed, err := store.ScanArmedForEntity(ctx, ws, cue)
		if err != nil {
			t.Fatalf("ScanArmedForEntity(%q): %v", cue, err)
		}
		if len(armed) != 1 {
			t.Fatalf("recurring intention gone after fire under cue %q (got %d keys)", cue, len(armed))
		}
		if armed[0].FiredCount != 2 {
			t.Errorf("FiredCount = %d, want 2", armed[0].FiredCount)
		}
		if armed[0].LastFiredAt == 0 {
			t.Errorf("LastFiredAt not stamped")
		}
	}
}

// TestMarkIntentionFired_OneShotDeletesAllCueKeys pins the one-shot fire path:
// firing deletes EVERY cue key of the intention — including cues other than
// the one that fired — so a consumed intention can never fire again.
func TestMarkIntentionFired_OneShotDeletesAllCueKeys(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := keys.VaultPrefix("prospective-vault")
	id := NewULID()
	cues := []string{"stripe", "billing", "invoices"}

	if err := store.ArmIntention(ctx, ws, id, cues, true); err != nil {
		t.Fatalf("ArmIntention: %v", err)
	}
	if err := store.MarkIntentionFired(ctx, ws, id, cues, true); err != nil {
		t.Fatalf("MarkIntentionFired: %v", err)
	}
	for _, cue := range cues {
		armed, err := store.ScanArmedForEntity(ctx, ws, cue)
		if err != nil {
			t.Fatalf("ScanArmedForEntity(%q): %v", cue, err)
		}
		if len(armed) != 0 {
			t.Errorf("one-shot cue key %q survived the fire (got %d)", cue, len(armed))
		}
	}
}

// TestRelinkProspectiveIntent_EntityMergeRewrites0x2D pins the entity-merge
// obligation for the 0x2D index (mirrors the 0x26 relink in
// RelinkRelationshipEntity): after merging cue entity A into B, the armed key
// must be reachable under B, gone under A, and the stored cue list — used by
// MarkIntentionFired to find every sibling key — must name B, on BOTH the
// moved key and any sibling cue keys of the same intention.
func TestRelinkProspectiveIntent_EntityMergeRewrites0x2D(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := keys.VaultPrefix("prospective-vault")
	id := NewULID()

	if err := store.ArmIntention(ctx, ws, id, []string{"postgres", "migration-plan"}, true); err != nil {
		t.Fatalf("ArmIntention: %v", err)
	}
	if err := store.RelinkProspectiveIntent(ctx, ws, "postgres", "postgresql"); err != nil {
		t.Fatalf("RelinkProspectiveIntent: %v", err)
	}

	// Old cue: gone.
	old, err := store.ScanArmedForEntity(ctx, ws, "postgres")
	if err != nil {
		t.Fatalf("ScanArmedForEntity(old): %v", err)
	}
	if len(old) != 0 {
		t.Errorf("old cue still armed after merge (got %d)", len(old))
	}

	// New cue: present, with the cue list rewritten.
	assertCues := func(cueName string) {
		t.Helper()
		armed, err := store.ScanArmedForEntity(ctx, ws, cueName)
		if err != nil {
			t.Fatalf("ScanArmedForEntity(%q): %v", cueName, err)
		}
		if len(armed) != 1 {
			t.Fatalf("ScanArmedForEntity(%q) = %d intentions, want 1", cueName, len(armed))
		}
		var sawNew, sawOld bool
		for _, c := range armed[0].Cues {
			if keys.NormalizeEntityName(c) == "postgresql" {
				sawNew = true
			}
			if keys.NormalizeEntityName(c) == "postgres" {
				sawOld = true
			}
		}
		if !sawNew || sawOld {
			t.Errorf("cue list under %q = %v; want postgres→postgresql rewritten", cueName, armed[0].Cues)
		}
	}
	assertCues("postgresql")
	// The SIBLING cue key's value must also be rewritten, or a later one-shot
	// fire would compute the old hash and leak the postgresql key.
	assertCues("migration-plan")

	// End-to-end: one-shot fire through the rewritten cue list removes both keys.
	armed, _ := store.ScanArmedForEntity(ctx, ws, "postgresql")
	if err := store.MarkIntentionFired(ctx, ws, id, armed[0].Cues, true); err != nil {
		t.Fatalf("MarkIntentionFired after relink: %v", err)
	}
	for _, cue := range []string{"postgresql", "migration-plan"} {
		got, _ := store.ScanArmedForEntity(ctx, ws, cue)
		if len(got) != 0 {
			t.Errorf("cue key %q leaked after one-shot fire post-merge", cue)
		}
	}
}

// TestClearVault_Removes0x2D pins that ClearVault range-tombstones the armed
// intention index — a re-created vault with the same name must not resurrect
// old intentions.
func TestClearVault_Removes0x2D(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := keys.VaultPrefix("prospective-clear-vault")
	id := NewULID()

	if err := store.ArmIntention(ctx, ws, id, []string{"redis"}, false); err != nil {
		t.Fatalf("ArmIntention: %v", err)
	}
	if _, err := store.ClearVault(ctx, ws); err != nil {
		t.Fatalf("ClearVault: %v", err)
	}
	armed, err := store.ScanArmedForEntity(ctx, ws, "redis")
	if err != nil {
		t.Fatalf("ScanArmedForEntity after clear: %v", err)
	}
	if len(armed) != 0 {
		t.Errorf("armed intention survived ClearVault (got %d)", len(armed))
	}
}
