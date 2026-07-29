package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// === THE PUSH increment 1: engine firing rules =============================
//
// RED-first: written before internal/engine/prospective.go existed (failed to
// compile: undefined Intend/NoticesForRecall/NoticesForRemember/Notice).

// writeMemWithEntities stores a memory carrying inline entities and returns its ID.
func writeMemWithEntities(t *testing.T, eng *Engine, vault, content string, entities ...string) string {
	t.Helper()
	ents := make([]mbp.InlineEntity, 0, len(entities))
	for _, name := range entities {
		ents = append(ents, mbp.InlineEntity{Name: name, Type: "concept"})
	}
	resp, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault: vault, Content: content, Entities: ents,
	})
	if err != nil {
		t.Fatalf("Write(%q): %v", content, err)
	}
	return resp.ID
}

// newSession returns a sessionSeen func backed by a fresh map plus a marker
// that records delivered notices the way the MCP layer does.
func newSession() (func(string) bool, func([]Notice)) {
	seen := map[string]bool{}
	return func(k string) bool { return seen[k] },
		func(ns []Notice) {
			for _, n := range ns {
				seen[n.DedupKey] = true
			}
		}
}

// sr wraps result IDs as ScoredResult with a uniform score (every result
// equally focal) for unit tests that exercise the firing rule directly.
func sr(ids ...string) []ScoredResult {
	out := make([]ScoredResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, ScoredResult{ID: id, Score: 1})
	}
	return out
}

func TestIntend_WritesGoalEngramAndArms(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-vault"

	id, err := eng.Intend(ctx, vault, "Remind me to rotate the API keys when vault-rotation comes up", []string{"vault-rotation"}, nil, true, nil)
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}
	ulid, err := storage.ParseULID(id)
	if err != nil {
		t.Fatalf("Intend returned non-ULID id %q: %v", id, err)
	}
	got, err := eng.GetEngram(ctx, vault, ulid)
	if err != nil || got == nil {
		t.Fatalf("intention engram not readable: %v", err)
	}
	if got.MemoryType != storage.TypeGoal {
		t.Errorf("intention MemoryType = %d, want TypeGoal (%d)", got.MemoryType, storage.TypeGoal)
	}
	ws := eng.Store().ResolveVaultPrefix(vault)
	armed, err := eng.Store().ScanArmedForEntity(ctx, ws, "vault-rotation")
	if err != nil || len(armed) != 1 {
		t.Fatalf("armed index after Intend: got %d (%v), want 1", len(armed), err)
	}
}

func TestIntend_RequiresAtLeastOneCue(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	if _, err := eng.Intend(context.Background(), "push-vault", "cueless intention", nil, nil, true, nil); !errors.Is(err, ErrInvalidIntention) {
		t.Errorf("Intend with no cues: err = %v, want ErrInvalidIntention", err)
	}
	if _, err := eng.Intend(context.Background(), "push-vault", "blank cue", []string{"   "}, nil, true, nil); !errors.Is(err, ErrInvalidIntention) {
		t.Errorf("Intend with blank cue: err = %v, want ErrInvalidIntention", err)
	}
}

// TestIntend_RejectsUbiquitousCue pins the IDF arming floor: a cue entity
// mentioned by >=10% of a non-tiny vault is refused LOUDLY, naming the cue —
// arming a hub entity would turn every other exchange into a nag.
func TestIntend_RejectsUbiquitousCue(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-ubiq-vault"

	// 20 memories; 3 mention "muninndb" (df=3, n=20 → 15% ≥ 10% floor).
	for i := 0; i < 17; i++ {
		writeMemWithEntities(t, eng, vault, fmt.Sprintf("filler memory number %d with unrelated text", i))
	}
	for i := 0; i < 3; i++ {
		writeMemWithEntities(t, eng, vault, fmt.Sprintf("note %d about the main project", i), "muninndb")
	}

	_, err := eng.Intend(ctx, vault, "ping me when muninndb comes up", []string{"muninndb"}, nil, true, nil)
	if !errors.Is(err, ErrInvalidIntention) {
		t.Fatalf("ubiquitous cue: err = %v, want ErrInvalidIntention", err)
	}
	if !strings.Contains(err.Error(), "muninndb") {
		t.Errorf("rejection must name the offending cue; got %q", err.Error())
	}

	// A rare cue in the same vault is accepted.
	if _, err := eng.Intend(ctx, vault, "ping me when erlang comes up", []string{"erlang"}, nil, true, nil); err != nil {
		t.Errorf("rare cue rejected: %v", err)
	}
}

// TestNoticesForRecall_FiresOnFocalCue pins the core firing rule: an armed
// intention fires exactly when its cue entity is carried by a RETURNED result.
func TestNoticesForRecall_FiresOnFocalCue(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-vault"

	topicID := writeMemWithEntities(t, eng, vault, "we benchmarked redis for the cache layer", "redis")
	unrelatedID := writeMemWithEntities(t, eng, vault, "picked a font for the landing page", "typography")

	const note = "When redis comes up: mention the eviction-policy bug we deferred"
	intentID, err := eng.Intend(ctx, vault, note, []string{"redis"}, nil, false, nil)
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}

	seen, _ := newSession()
	notices, err := eng.NoticesForRecall(ctx, vault, sr(topicID), seen, false)
	if err != nil {
		t.Fatalf("NoticesForRecall: %v", err)
	}
	if len(notices) != 1 {
		t.Fatalf("focal cue: got %d notices, want 1", len(notices))
	}
	n := notices[0]
	if n.Kind != "intention" || n.MemoryID != intentID || n.Cue != "redis" || n.Note != note {
		t.Errorf("notice = %+v; want kind=intention, memory_id=%s, cue=redis, note verbatim", n, intentID)
	}
	if !strings.Contains(n.Why, "redis") {
		t.Errorf("why must state the focal cue (auditable); got %q", n.Why)
	}

	// Unrelated result → no notice (the focal gate).
	seen2, _ := newSession()
	none, err := eng.NoticesForRecall(ctx, vault, sr(unrelatedID), seen2, false)
	if err != nil {
		t.Fatalf("NoticesForRecall(unrelated): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unrelated result fired %d notices, want 0", len(none))
	}
}

// TestProspective_FocalGateInvariant is the COG-21 pin: a notice fires ONLY
// when its cue entity is in the focal set of the current call — an armed,
// valid, never-fired intention stays silent through any number of unrelated
// exchanges. Unrelated context ⇒ zero notices, always.
func TestProspective_FocalGateInvariant(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-invariant-vault"

	if _, err := eng.Intend(ctx, vault, "surface the licensing question when legal-review is focal", []string{"legal-review"}, nil, false, nil); err != nil {
		t.Fatalf("Intend: %v", err)
	}
	var unrelated []string
	for i := 0; i < 5; i++ {
		unrelated = append(unrelated, writeMemWithEntities(t, eng, vault,
			fmt.Sprintf("unrelated working memory %d", i), fmt.Sprintf("topic-%d", i)))
	}
	seen, _ := newSession()
	for i := 0; i < 5; i++ {
		notices, err := eng.NoticesForRecall(ctx, vault, sr(unrelated...), seen, false)
		if err != nil {
			t.Fatalf("NoticesForRecall: %v", err)
		}
		if len(notices) != 0 {
			t.Fatalf("COG-21 violated: unrelated call %d produced %d notices, want 0", i, len(notices))
		}
	}
}

// TestNoticesForRecall_TimeSilences pins "time SILENCES, never fires": an
// intention whose valid_until has passed does not fire even on a focal cue.
func TestNoticesForRecall_TimeSilences(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-vault"

	topicID := writeMemWithEntities(t, eng, vault, "quarterly numbers for the board deck", "board-deck")

	// Expired intention: stamp ValidUntil in the past via forget(not_true_since)
	// (Intend itself rejects an already-past valid_until — so create it live,
	// then invalidate, exactly how a window closes in production).
	intentID, err := eng.Intend(ctx, vault, "add the churn slide when board-deck comes up", []string{"board-deck"}, nil, false, nil)
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}
	// The window must be non-empty (not_true_since after ValidFrom=CreatedAt),
	// so close it a few ms ahead and wait for it to lapse.
	soon := time.Now().Add(20 * time.Millisecond)
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: intentID, NotTrueSince: &soon}); err != nil {
		t.Fatalf("Forget(not_true_since): %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	seen, _ := newSession()
	notices, err := eng.NoticesForRecall(ctx, vault, sr(topicID), seen, false)
	if err != nil {
		t.Fatalf("NoticesForRecall: %v", err)
	}
	if len(notices) != 0 {
		t.Errorf("expired intention fired %d notices, want 0 (time silences)", len(notices))
	}
}

// TestNoticesForRecall_OneShotConsumed pins the one-shot lifecycle: first
// focal delivery fires and disarms; a later session gets nothing.
func TestNoticesForRecall_OneShotConsumed(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-vault"

	topicID := writeMemWithEntities(t, eng, vault, "reviewing the stripe webhook retries", "stripe")
	if _, err := eng.Intend(ctx, vault, "one-shot: confirm the refund path when stripe comes up", []string{"stripe"}, nil, true, nil); err != nil {
		t.Fatalf("Intend: %v", err)
	}

	seen1, mark1 := newSession()
	first, err := eng.NoticesForRecall(ctx, vault, sr(topicID), seen1, false)
	if err != nil || len(first) != 1 {
		t.Fatalf("first delivery: %d notices (%v), want 1", len(first), err)
	}
	mark1(first)

	// New session, same focal cue: consumed one-shot must stay silent.
	seen2, _ := newSession()
	second, err := eng.NoticesForRecall(ctx, vault, sr(topicID), seen2, false)
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("one-shot fired twice (%d notices in second session), want 0", len(second))
	}
}

// TestNoticesForRecall_SessionDedup pins recurring-intention dedup: within a
// session an intention fires once; a fresh session fires it again.
func TestNoticesForRecall_SessionDedup(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-vault"

	topicID := writeMemWithEntities(t, eng, vault, "tuning grafana alert thresholds", "grafana")
	if _, err := eng.Intend(ctx, vault, "recurring: check the silenced alerts when grafana comes up", []string{"grafana"}, nil, false, nil); err != nil {
		t.Fatalf("Intend: %v", err)
	}

	seen, mark := newSession()
	first, err := eng.NoticesForRecall(ctx, vault, sr(topicID), seen, false)
	if err != nil || len(first) != 1 {
		t.Fatalf("first call: %d notices (%v), want 1", len(first), err)
	}
	mark(first)

	again, err := eng.NoticesForRecall(ctx, vault, sr(topicID), seen, false)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("same-session repeat fired %d notices, want 0", len(again))
	}

	fresh, _ := newSession()
	next, err := eng.NoticesForRecall(ctx, vault, sr(topicID), fresh, false)
	if err != nil || len(next) != 1 {
		t.Errorf("new session: %d notices (%v), want 1 (recurring re-fires)", len(next), err)
	}
}

// TestNoticesForRecall_ReadOnlySuppressesMarker pins COG-11 for THE PUSH:
// read_only (observe) delivery must NOT write the fired marker — the one-shot
// stays armed — while sessionSeen still dedups within the session.
func TestNoticesForRecall_ReadOnlySuppressesMarker(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-vault"

	topicID := writeMemWithEntities(t, eng, vault, "auditing the backup cron", "backup-cron")
	if _, err := eng.Intend(ctx, vault, "one-shot: verify offsite copies when backup-cron comes up", []string{"backup-cron"}, nil, true, nil); err != nil {
		t.Fatalf("Intend: %v", err)
	}

	seen, mark := newSession()
	first, err := eng.NoticesForRecall(ctx, vault, sr(topicID), seen, true /* readOnly */)
	if err != nil || len(first) != 1 {
		t.Fatalf("readOnly delivery: %d notices (%v), want 1", len(first), err)
	}
	mark(first)

	// COG-11: no marker write — the one-shot key must still be armed.
	ws := eng.Store().ResolveVaultPrefix(vault)
	armed, err := eng.Store().ScanArmedForEntity(ctx, ws, "backup-cron")
	if err != nil || len(armed) != 1 {
		t.Fatalf("readOnly fired the marker: armed = %d (%v), want 1 (still armed)", len(armed), err)
	}
	if armed[0].FiredCount != 0 {
		t.Errorf("readOnly bumped FiredCount to %d, want 0", armed[0].FiredCount)
	}

	// Intra-session dedup still holds without the marker.
	again, err := eng.NoticesForRecall(ctx, vault, sr(topicID), seen, true)
	if err != nil {
		t.Fatalf("readOnly repeat: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("readOnly repeat fired %d notices, want 0 (sessionSeen dedup)", len(again))
	}

	// A later non-readOnly session still gets the (never-consumed) one-shot.
	fresh, _ := newSession()
	next, err := eng.NoticesForRecall(ctx, vault, sr(topicID), fresh, false)
	if err != nil || len(next) != 1 {
		t.Errorf("post-readOnly session: %d notices (%v), want 1", len(next), err)
	}
}

// TestNoticesForRemember_SelfEcho pins the self-echo rule: the engram created
// by the current call never fires its own intention.
func TestNoticesForRemember_SelfEcho(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-vault"

	intentID, err := eng.Intend(ctx, vault, "self-echo guard intention", []string{"kafka"}, nil, false, nil)
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}

	// Simulate the write that created the intention flowing through remember's
	// notice hook: focal carries the cue, createdID is the intention itself.
	seen, _ := newSession()
	notices, err := eng.NoticesForRemember(ctx, vault, []string{"kafka"}, intentID, seen)
	if err != nil {
		t.Fatalf("NoticesForRemember: %v", err)
	}
	if len(notices) != 0 {
		t.Errorf("self-echo: intention fired on its own creating call (%d notices), want 0", len(notices))
	}

	// A DIFFERENT write mentioning the cue does fire it.
	otherID := writeMemWithEntities(t, eng, vault, "kafka consumer lag spiked", "kafka")
	seen2, _ := newSession()
	fired, err := eng.NoticesForRemember(ctx, vault, []string{"kafka"}, otherID, seen2)
	if err != nil || len(fired) != 1 {
		t.Errorf("non-self write: %d notices (%v), want 1", len(fired), err)
	}
}

// TestNoticesForRecall_SoftDeletedIntentionSilent: a forgotten intention's
// stale 0x2D keys must not fire.
func TestNoticesForRecall_SoftDeletedIntentionSilent(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-vault"

	topicID := writeMemWithEntities(t, eng, vault, "terraform state drift on prod", "terraform")
	intentID, err := eng.Intend(ctx, vault, "check drift when terraform comes up", []string{"terraform"}, nil, false, nil)
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: intentID}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	seen, _ := newSession()
	notices, err := eng.NoticesForRecall(ctx, vault, sr(topicID), seen, false)
	if err != nil {
		t.Fatalf("NoticesForRecall: %v", err)
	}
	if len(notices) != 0 {
		t.Errorf("soft-deleted intention fired %d notices, want 0", len(notices))
	}
}

// TestNoticesForRecall_CapTwo pins the 2-notice cap and importance ranking:
// three eligible intentions → the two with highest effective importance.
func TestNoticesForRecall_CapTwo(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-vault"

	topicID := writeMemWithEntities(t, eng, vault, "planning the nats migration", "nats")

	imp := func(v float32) *float32 { return &v }
	hiID, err := eng.Intend(ctx, vault, "HIGH: escrow the credentials before nats work", []string{"nats"}, nil, false, imp(0.9))
	if err != nil {
		t.Fatalf("Intend hi: %v", err)
	}
	midID, err := eng.Intend(ctx, vault, "MID: benchmark jetstream before nats work", []string{"nats"}, nil, false, imp(0.8))
	if err != nil {
		t.Fatalf("Intend mid: %v", err)
	}
	if _, err := eng.Intend(ctx, vault, "LOW: rename the nats channel someday", []string{"nats"}, nil, false, imp(0.1)); err != nil {
		t.Fatalf("Intend low: %v", err)
	}

	seen, _ := newSession()
	notices, err := eng.NoticesForRecall(ctx, vault, sr(topicID), seen, false)
	if err != nil {
		t.Fatalf("NoticesForRecall: %v", err)
	}
	if len(notices) != 2 {
		t.Fatalf("cap: got %d notices, want 2", len(notices))
	}
	got := map[string]bool{notices[0].MemoryID: true, notices[1].MemoryID: true}
	if !got[hiID] || !got[midID] {
		t.Errorf("cap-2 must keep the two highest-importance intentions; got %v, want {%s,%s}", got, hiID, midID)
	}
	if notices[0].MemoryID != hiID {
		t.Errorf("ranking: first notice = %s, want highest-importance %s", notices[0].MemoryID, hiID)
	}
}

// TestNoticesForRecall_ContradictionNotice pins the flagship zero-new-detection
// payload: a RETURNED engram that sits in an unresolved 0x0A pair yields a
// kind:"contradiction" notice naming both sides; unrelated results yield none.
func TestNoticesForRecall_ContradictionNotice(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-vault"

	aID := writeMemWithEntities(t, eng, vault, "the launch date is March 3", "launch")
	bID := writeMemWithEntities(t, eng, vault, "the launch date is April 9", "launch")
	cID := writeMemWithEntities(t, eng, vault, "office plants need watering", "plants")

	ws := eng.Store().ResolveVaultPrefix(vault)
	a, _ := storage.ParseULID(aID)
	b, _ := storage.ParseULID(bID)
	if err := eng.Store().FlagContradiction(ctx, ws, a, b); err != nil {
		t.Fatalf("FlagContradiction: %v", err)
	}

	seen, mark := newSession()
	notices, err := eng.NoticesForRecall(ctx, vault, sr(aID), seen, false)
	if err != nil {
		t.Fatalf("NoticesForRecall: %v", err)
	}
	if len(notices) != 1 {
		t.Fatalf("contradiction: got %d notices, want 1", len(notices))
	}
	n := notices[0]
	if n.Kind != "contradiction" || n.MemoryID != aID || n.ConflictsWith != bID {
		t.Errorf("notice = %+v; want kind=contradiction, memory_id=%s, conflicts_with=%s", n, aID, bID)
	}
	mark(notices)

	// Session dedup applies to contradiction notices too.
	again, _ := eng.NoticesForRecall(ctx, vault, sr(aID), seen, false)
	if len(again) != 0 {
		t.Errorf("same-session contradiction re-fired (%d), want 0", len(again))
	}

	// A result outside any pair yields nothing.
	seen2, _ := newSession()
	none, err := eng.NoticesForRecall(ctx, vault, sr(cID), seen2, false)
	if err != nil {
		t.Fatalf("NoticesForRecall(unrelated): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unrelated result produced %d contradiction notices, want 0", len(none))
	}
}

// TestMergeEntity_RewritesArmedIntentions pins the composition obligation:
// after merging cue entity A into B, the intention fires on B-focal calls.
func TestMergeEntity_RewritesArmedIntentions(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-merge-vault"

	// Both entity records must exist for MergeEntity.
	writeMemWithEntities(t, eng, vault, "notes on pg vacuum tuning", "pg")
	topicID := writeMemWithEntities(t, eng, vault, "postgresql upgrade to 17 planned", "postgresql")

	intentID, err := eng.Intend(ctx, vault, "flag the extension audit when pg comes up", []string{"pg"}, nil, false, nil)
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}
	if _, err := eng.MergeEntity(ctx, vault, "pg", "postgresql", false); err != nil {
		t.Fatalf("MergeEntity: %v", err)
	}

	seen, _ := newSession()
	notices, err := eng.NoticesForRecall(ctx, vault, sr(topicID), seen, false)
	if err != nil {
		t.Fatalf("NoticesForRecall: %v", err)
	}
	if len(notices) != 1 || notices[0].MemoryID != intentID {
		t.Fatalf("post-merge focal on canonical entity: got %d notices (%v), want the relinked intention %s", len(notices), notices, intentID)
	}
}
