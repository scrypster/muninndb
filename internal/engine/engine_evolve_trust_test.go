package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// #872: evolve carries a record's TRUST and replaces its content. The verb never
// manufactures a trust level.
//
// Confidence is deliberately NOT carried: the successor is written at 1.0 unless
// the caller supplies a value. Carrying it was built and measured for #872 and
// held back; TestEvolve_PinnedDecision_DoesNotCarryConfidence pins that decision
// and COG-37 records why.
//
// Fixture domain throughout this file: a fictional alpine trail-maintenance
// cooperative. Nothing here is derived from any real vault.

// readEngram fetches the stored engram for a vault-scoped id.
func readEngram(t *testing.T, eng *Engine, vault, id string) *storage.Engram {
	t.Helper()
	ws := eng.store.ResolveVaultPrefix(vault)
	u, err := storage.ParseULID(id)
	if err != nil {
		t.Fatalf("ParseULID(%q): %v", id, err)
	}
	e, err := eng.store.GetEngram(context.Background(), ws, u)
	if err != nil {
		t.Fatalf("GetEngram(%s): %v", id, err)
	}
	if e == nil {
		t.Fatalf("engram %s not found", id)
	}
	return e
}

// assertHeadUntouched fails unless id is still the live, unsuperseded record it
// was before a refused write: active, validity window open, same content, same
// trust. A refusal that left a successor, a stamp or a demotion behind is not a
// refusal. When key is non-empty the upsert pointer must still name id too.
func assertHeadUntouched(t *testing.T, eng *Engine, vault, key, id, content string, trust storage.TrustLevel) {
	t.Helper()
	head := readEngram(t, eng, vault, id)
	if head.State != storage.StateActive || !head.ValidUntil.IsZero() {
		t.Errorf("refused write still superseded the head: state=%v valid_until=%v", head.State, head.ValidUntil)
	}
	if head.Content != content {
		t.Errorf("refused write changed the head's content: %q", head.Content)
	}
	if head.Trust != trust {
		t.Errorf("refused write changed the head's trust: %d (%s), want %d (%s)",
			head.Trust, head.Trust.String(), trust, trust.String())
	}
	if key == "" {
		return
	}
	ws := eng.store.ResolveVaultPrefix(vault)
	ptr, err := eng.store.GetUpsertKey(context.Background(), ws, sha256.Sum256([]byte(key)))
	if err != nil {
		t.Fatalf("GetUpsertKey(%q): %v", key, err)
	}
	if ptr.String() != id {
		t.Errorf("refused write moved the upsert pointer for %q: %s, want %s", key, ptr.String(), id)
	}
}

// TestEvolve_UntrustedSticks: the unreliable flag is a property of the RECORD,
// not of one revision of its text. Editing the content of a memory flagged
// untrusted must not launder the flag away — before #872 an evolve reset every
// successor to "inferred", so any caller with evolve rights could clear an
// untrusted marking simply by rewording the memory, and a vault running
// ExcludeUntrusted would start serving it again.
//
// The chain hops matter: laundering only needs to work once.
func TestEvolve_UntrustedSticks(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "trailwork",
		Concept: "Switchback drainage bar spacing",
		Content: "Drainage bars on the Larkspur switchbacks are spaced every 4 metres.",
		Trust:   "untrusted",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readEngram(t, eng, "trailwork", resp.ID).Trust; got != storage.TrustUntrusted {
		t.Fatalf("premise not reproduced: predecessor trust = %v, want untrusted", got)
	}

	id := resp.ID
	for hop := 1; hop <= 3; hop++ {
		newID, err := eng.Evolve(ctx, "trailwork", id,
			fmt.Sprintf("Drainage bars on the Larkspur switchbacks are spaced every %d metres.", 4+hop),
			"resurveyed", nil, "")
		if err != nil {
			t.Fatalf("Evolve hop %d: %v", hop, err)
		}
		id = newID.String()
		if got := readEngram(t, eng, "trailwork", id).Trust; got != storage.TrustUntrusted {
			t.Fatalf("hop %d: successor trust = %v (%s), want untrusted — evolve laundered the "+
				"unreliable flag off the record", hop, got, got.String())
		}
	}
}

// TestEvolve_VerifiedSurvivesAndKeepsPruneProtection is the control that a
// cosmetic change cannot pass: it asserts the DOWNSTREAM consequence, not the
// field.
//
// storage.EffectiveImportance adds +0.1 for TrustVerified, but only on the
// DERIVED path (an explicit importance is returned verbatim). So a verified
// Decision with no explicit importance sits at 0.6+0.1 = 0.70000005 in float32,
// at or above storage.HighImportanceFloor (0.7), which makes it exempt from the
// COG-20 MaxEngrams prune pass. Resetting trust to inferred on evolve dropped it
// to 0.6 — prunable. Routine curation of a vault's verified decisions moved them
// from prune-exempt to prunable, silently.
func TestEvolve_VerifiedSurvivesAndKeepsPruneProtection(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:      "trailwork",
		Concept:    "Bridge decking material standard",
		Content:    "The cooperative decked the Fernbrook crossing in larch, not spruce.",
		Trust:      "verified",
		MemoryType: uint8(storage.TypeDecision),
		// Importance deliberately unset: the trust bump only reaches the
		// derived path, so an explicit importance would hide the defect.
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	pre := readEngram(t, eng, "trailwork", resp.ID)
	if pre.Trust != storage.TrustVerified {
		t.Fatalf("premise not reproduced: predecessor trust = %v, want verified", pre.Trust)
	}
	if pre.EffectiveImportance() < storage.HighImportanceFloor {
		t.Fatalf("premise not reproduced: predecessor EffectiveImportance %v < floor %v — "+
			"the prune exemption this test measures does not exist for this fixture",
			pre.EffectiveImportance(), storage.HighImportanceFloor)
	}

	newID, err := eng.Evolve(ctx, "trailwork", resp.ID,
		"The cooperative decked the Fernbrook crossing in larch, and re-decks on a 12-year cycle.",
		"added the re-deck cycle", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	post := readEngram(t, eng, "trailwork", newID.String())

	if post.Trust != storage.TrustVerified {
		t.Errorf("successor trust = %v (%s), want verified — evolve demoted a "+
			"human-confirmed record", post.Trust, post.Trust.String())
	}
	// The consequence, not the field.
	if post.EffectiveImportance() < storage.HighImportanceFloor {
		t.Errorf("successor EffectiveImportance = %v, want >= HighImportanceFloor %v: "+
			"the successor lost the COG-20 MaxEngrams prune exemption its predecessor had",
			post.EffectiveImportance(), storage.HighImportanceFloor)
	}
}

// TestEvolve_VerifiedSuccessorSurvivesPruneVault is the end-to-end variant of
// the assertion above: it drives the actual COG-20 MaxEngrams path rather than
// recomputing its predicate, so a change that sets the field but that the pruner
// does not read still fails.
func TestEvolve_VerifiedSuccessorSurvivesPruneVault(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)

	const vaultName = "trailwork-prune"
	ws := store.VaultPrefix(vaultName)
	if err := store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Low-importance filler the pruner is expected to take instead. Written
	// FIRST so they are the oldest and coldest candidates.
	for i := 0; i < 8; i++ {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:      vaultName,
			Concept:    fmt.Sprintf("Work party note %d", i),
			Content:    fmt.Sprintf("Work party %d cleared brush on the lower loop.", i),
			MemoryType: uint8(storage.TypeObservation),
		}); err != nil {
			t.Fatalf("Write filler %d: %v", i, err)
		}
	}

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:      vaultName,
		Concept:    "Tool cache relocation",
		Content:    "The cooperative moved the tool cache to the Fernbrook trailhead.",
		Trust:      "verified",
		MemoryType: uint8(storage.TypeDecision),
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	newID, err := eng.Evolve(ctx, vaultName, resp.ID,
		"The cooperative moved the tool cache to the Fernbrook trailhead, under the old ranger shed.",
		"located it precisely", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	eng.WaitWriteTimeIdle()

	// MaxEngrams=1 forces the prune pass to consider every record; only the
	// COG-20 exemption can save the successor.
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name: vaultName, Public: true,
		Plasticity: &auth.PlasticityConfig{MaxEngrams: ptr(1)},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	if _, err := eng.PruneVault(ctx, vaultName); err != nil {
		t.Fatalf("PruneVault: %v", err)
	}

	got, err := store.GetEngram(ctx, ws, newID)
	if err != nil || got == nil {
		t.Fatalf("the evolved successor of a VERIFIED decision was hard-deleted by the "+
			"MaxEngrams prune pass — it lost its predecessor's COG-20 exemption (err=%v)", err)
	}
}

// TestEvolve_PinnedDecision_DoesNotCarryConfidence is a PINNED DECISION: it
// passes while evolve writes the successor at confidence 1.0 and FAILS WHEN
// EVOLVE STARTS CARRYING CONFIDENCE, so that change cannot land without the
// invariant and the docs moving with it.
//
// This is not the behaviour anyone would design. 1.0 is not a neutral default:
// a caller who stored a fact at 0.4 and then sharpened its wording finds the
// engine has decided the fact is certain. Carrying confidence was built for
// #872 and held back for one measured reason: it also carries a contradiction
// penalty onto the text that RESOLVED the conflict, and that penalty is small
// only from a prior of exactly 1.0 (0.975). From 0.9 one penalty leaves 0.5, a
// second conflict on a lineage that started at 1.0 leaves 0.797 — and nothing on
// MCP or REST can set a confidence back afterwards (#901). So confidence is
// carried only by a change that also provides that route.
func TestEvolve_PinnedDecision_DoesNotCarryConfidence(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:      "trailwork",
		Concept:    "Culvert inlet diameter",
		Content:    "The Larkspur culvert inlet is roughly 300mm; nobody measured it.",
		Confidence: 0.4,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readEngram(t, eng, "trailwork", resp.ID).Confidence; got < 0.399 || got > 0.401 {
		t.Fatalf("premise not reproduced: predecessor confidence = %v, want 0.4", got)
	}

	newID, err := eng.Evolve(ctx, "trailwork", resp.ID,
		"The Larkspur culvert inlet is roughly 300mm; still nobody measured it.",
		"reworded", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	if got := readEngram(t, eng, "trailwork", newID.String()).Confidence; got < 0.999 || got > 1.001 {
		t.Fatalf("PINNED DECISION CHANGED: the successor of a 0.4-confidence memory has confidence %v, "+
			"not the 1.0 evolve writes when the caller supplies none — evolve is now carrying or "+
			"deriving confidence. That was held out of #872 on purpose: a carried confidence also "+
			"carries a contradiction penalty onto the text that resolved the conflict, and no MCP "+
			"or REST route can set it back (#901). If this is the change that provides that route, "+
			"replace this test with one that pins the new rule, move Confidence out of the census's "+
			"reset bucket, and amend COG-37, the COG-10 amendment, the upsert_mode descriptions "+
			"(internal/mcp/tools.go, openapi.yaml), the guide and CHANGELOG. Otherwise revert.", got)
	}
}

// TestEvolve_TrustUnsetNormalizesToInferred: a legacy record stored before the
// trust field existed decodes as TrustUnset (0x00). Carrying that verbatim would
// mint successors in a state resolveTrust("") never produces; the carry
// normalizes it to inferred, matching every other write path.
func TestEvolve_TrustUnsetNormalizesToInferred(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "trailwork", Concept: "Legacy note", Content: "Original legacy content.",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Force the predecessor into the legacy 0x00 state that no current write
	// path can produce.
	ws := eng.store.ResolveVaultPrefix("trailwork")
	oldULID, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID: %v", err)
	}
	if err := eng.store.UpdateTrust(ctx, ws, oldULID, storage.TrustUnset); err != nil {
		t.Fatalf("UpdateTrust(TrustUnset): %v", err)
	}
	if got := readEngram(t, eng, "trailwork", resp.ID).Trust; got != storage.TrustUnset {
		t.Fatalf("premise not reproduced: predecessor trust = %v, want TrustUnset", got)
	}

	newID, err := eng.Evolve(ctx, "trailwork", resp.ID, "Updated legacy content.", "update", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	if got := readEngram(t, eng, "trailwork", newID.String()).Trust; got != storage.TrustInferred {
		// %d, not %v: TrustUnset also DISPLAYS as "inferred", so the label alone
		// reads "inferred, want inferred" when the normalization is missing.
		t.Errorf("successor trust byte = %d, want %d (TrustInferred; TrustUnset normalizes on carry)",
			got, storage.TrustInferred)
	}
}

// TestUpsertEvolve_HonorsExplicitTrust is the principle #1 half of #872: on the
// stable_key (upsert_mode) path an explicit caller trust was honored on the
// CREATE branch and silently discarded on every subsequent content-change
// re-sync. A sync job that stamps trust=verified on each run therefore produced
// a verified record on day one and an inferred one on day two, with no error and
// no warning. Same for confidence.
//
// The SEC-14 gate stays on this path: verified still requires write/full.
func TestUpsertEvolve_HonorsExplicitTrust(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)

	first, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "trailwork",
		Concept:      "Bridge inspection status",
		Content:      "The Fernbrook crossing passed inspection in the spring survey.",
		IdempotentID: "bridge-inspection",
		UpsertMode:   true,
		Trust:        "verified",
		Confidence:   0.8,
	})
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	created := readEngram(t, eng, "trailwork", first.ID)
	if created.Trust != storage.TrustVerified {
		t.Fatalf("premise not reproduced: create-branch trust = %v, want verified", created.Trust)
	}

	second, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "trailwork",
		Concept:      "Bridge inspection status",
		Content:      "The Fernbrook crossing passed inspection in the autumn survey.",
		IdempotentID: "bridge-inspection",
		UpsertMode:   true,
		Trust:        "verified",
		Confidence:   0.8,
	})
	if err != nil {
		t.Fatalf("upsert evolve: %v", err)
	}
	if second.Hint != "upsert-evolved" {
		t.Fatalf("premise not reproduced: hint = %q, want upsert-evolved", second.Hint)
	}
	evolved := readEngram(t, eng, "trailwork", second.ID)
	if evolved.Trust != storage.TrustVerified {
		t.Errorf("upsert-evolve trust = %v (%s), want verified: the caller's EXPLICIT trust was "+
			"silently dropped on the content-change branch", evolved.Trust, evolved.Trust.String())
	}
	if evolved.Confidence < 0.799 || evolved.Confidence > 0.801 {
		t.Errorf("upsert-evolve confidence = %v, want 0.8: the caller's EXPLICIT confidence was "+
			"silently dropped on the content-change branch", evolved.Confidence)
	}
}

// TestUpsertEvolve_NoExplicitTrustInherits: with no trust on the request the
// upsert-evolve branch falls back to inheritance, exactly like a plain evolve —
// and, exactly like a plain evolve, it does NOT carry confidence: with none on
// the request the successor is written at 1.0 (the pinned decision above).
func TestUpsertEvolve_NoExplicitTrustInherits(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)

	first, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "trailwork",
		Concept:      "Winter closure dates",
		Content:      "The upper loop closes from the first hard frost.",
		IdempotentID: "winter-closure",
		UpsertMode:   true,
		Trust:        "untrusted",
		Confidence:   0.3,
	})
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	second, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "trailwork",
		Content:      "The upper loop closes from the first hard frost through to the thaw.",
		IdempotentID: "winter-closure",
		UpsertMode:   true,
		// no trust, no confidence
	})
	if err != nil {
		t.Fatalf("upsert evolve: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("premise not reproduced: content change did not evolve (hint %q)", second.Hint)
	}
	got := readEngram(t, eng, "trailwork", second.ID)
	if got.Trust != storage.TrustUntrusted {
		t.Errorf("trust = %v, want untrusted (inherited)", got.Trust)
	}
	if got.Confidence < 0.999 || got.Confidence > 1.001 {
		t.Errorf("confidence = %v, want 1.0: the predecessor held 0.3 and the request supplied "+
			"none, and confidence is not carried (see "+
			"TestEvolve_PinnedDecision_DoesNotCarryConfidence before changing this)", got.Confidence)
	}
}

// TestUpsertEvolve_VerifiedStillGated: SEC-14 must survive the new pass-through.
// A context whose mode may not assert verified, asking for verified on the
// content-change branch, is REJECTED, never silently downgraded.
func TestUpsertEvolve_VerifiedStillGated(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	first, err := eng.Write(withMode(auth.ModeWrite), &mbp.WriteRequest{
		Vault:        "trailwork",
		Concept:      "Rope bridge tension log",
		Content:      "The rope bridge was re-tensioned in May.",
		IdempotentID: "rope-tension",
		UpsertMode:   true,
	})
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	_, err = eng.Write(withMode(auth.ModeObserve), &mbp.WriteRequest{
		Vault:        "trailwork",
		Content:      "The rope bridge was re-tensioned in May and again in September.",
		IdempotentID: "rope-tension",
		UpsertMode:   true,
		Trust:        "verified",
	})
	if err == nil {
		t.Fatalf("upsert-evolve accepted trust=verified from a context whose mode may not assert it (id %s)", first.ID)
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("refusal is not an ErrInvalidRequest (transports map that one to an invalid-request "+
			"status, anything else to a server error): %v", err)
	}
	assertHeadUntouched(t, eng, "trailwork", "rope-tension", first.ID,
		"The rope bridge was re-tensioned in May.", storage.TrustInferred)
}

// TestUpsertEvolve_ExplicitTrustAndConfidenceAreApplied is the discriminating
// half of TestUpsertEvolve_HonorsExplicitTrust. That test re-syncs with the SAME
// trust the predecessor already holds, so with trust inherited its trust
// assertion passes whether or not writeUpsert forwards the caller's value —
// carried and honored are indistinguishable there (shown by deleting the
// pass-through: only its confidence assertion notices). Here the re-sync asserts
// a trust and a confidence that both DIFFER from the predecessor's, so only the
// forwarded request can produce them.
func TestUpsertEvolve_ExplicitTrustAndConfidenceAreApplied(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)

	first, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "trailwork",
		Concept:      "Gate latch condition",
		Content:      "The Fernbrook stock gate latch was replaced in spring.",
		IdempotentID: "gate-latch",
		UpsertMode:   true,
		Trust:        "verified",
		Confidence:   0.8,
	})
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	pre := readEngram(t, eng, "trailwork", first.ID)
	if pre.Trust != storage.TrustVerified || pre.Confidence < 0.799 || pre.Confidence > 0.801 {
		t.Fatalf("premise not reproduced: predecessor trust=%v confidence=%v, want verified/0.8",
			pre.Trust, pre.Confidence)
	}

	second, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "trailwork",
		Content:      "A hiker reports the Fernbrook stock gate latch is sticking again.",
		IdempotentID: "gate-latch",
		UpsertMode:   true,
		Trust:        "external",
		Confidence:   0.6,
	})
	if err != nil {
		t.Fatalf("upsert evolve: %v", err)
	}
	if second.Hint != "upsert-evolved" {
		t.Fatalf("premise not reproduced: hint = %q, want upsert-evolved", second.Hint)
	}
	got := readEngram(t, eng, "trailwork", second.ID)
	if got.Trust != storage.TrustExternal {
		t.Errorf("upsert-evolve trust = %d (%s), want external: the request's explicit trust lost "+
			"to the predecessor's inherited level", got.Trust, got.Trust.String())
	}
	if got.Confidence < 0.599 || got.Confidence > 0.601 {
		t.Errorf("upsert-evolve confidence = %v, want 0.6: the request's explicit confidence was "+
			"not applied on the content-change branch", got.Confidence)
	}
}

// ---------------------------------------------------------------------------
// The census — kill the defect PATTERN, not the defect.
// ---------------------------------------------------------------------------

// evolveFieldBuckets is an EXHAUSTIVE, hand-written classification of every
// storage.Engram field, stating what a content-replacing verb does with it and
// WHY. TestEvolve_FieldCensus fails on any field that is not listed, so adding
// a field to storage.Engram forces the author to make this decision explicitly
// rather than inheriting whatever the struct literal happens to omit — which is
// the exact way #653 (MemoryType) and #872 (Trust) were introduced.
//
// Each entry carries its reason. A future field dropped into a bucket without
// one is the failure mode this census cannot catch by itself.
var evolveFieldBuckets = map[string]map[string]string{
	// Carried from the predecessor. The successor is the same RECORD with new
	// text, so its trust and its classification travel with it.
	"inherited": {
		"Concept":    "the label of the thing being described; caller may override, else carried (#769 announces the carry)",
		"Tags":       "curation state, not content; muninn_update_tags is the way to change it",
		"MemoryType": "what kind of memory this is does not change because the wording did (#653)",
		"TypeLabel":  "free-form companion to MemoryType, same reasoning (#653)",
		"Importance": "caller-asserted priority; explicit override wins, unset stays unset so the type-table default keeps deriving",
		"Trust":      "who vouched for the record, not content (#872). TrustUnset normalizes to TrustInferred on carry",
	},
	// Deliberately NOT carried: the successor starts these fresh.
	"reset": {
		// NOT CARRIED, BY DECISION (#872) — not by omission and not because 1.0
		// is right. It belongs with Trust in "inherited" and moves there only
		// in a change that also lets a caller set confidence back (#901).
		// Pinned by TestEvolve_PinnedDecision_DoesNotCarryConfidence.
		"Confidence":  "NOT carried, by decision: written at 1.0 unless the caller supplies one. Carrying it also carries a contradiction penalty onto the text that resolved the conflict, with no MCP/REST route to set it back (#901)",
		"Stability":   "decay resistance restarts with the new text; the successor has its own forgetting curve",
		"LastAccess":  "use history belongs to the revision that was used, not to the new one",
		"AccessCount": "same: the successor has been accessed zero times",
		"Relevance":   "recomputed at read time from the decay curve; never meaningful as a carried literal",
		// TODO(#872 follow-up): both of these SHOULD arguably be inherited and
		// are recorded here honestly rather than quietly fixed in this
		// increment, which is about trust.
		"CreatedBy":      "TODO: dropped today, which drops the successor out of the creator index. Inert in practice — no transport populates it (mbp.WriteRequest has no such field) — which is exactly why it must be written down",
		"Classification": "TODO: concept-cluster id, dropped today. Re-derivable by the clustering pass, but nothing re-runs it on evolve",
	},
	// Derived from the NEW content. Carrying these would assert the
	// predecessor's text is true of the successor's.
	"contentDerived": {
		"Content":   "the whole point of the verb",
		"Embedding": "vector of the new content; caller-supplied or re-embedded",
		"EmbedDim":  "dimension of the new embedding, stamped by storage on write",
		"Summary":   "left EMPTY on purpose so the summarize stage regenerates it — a non-empty Summary is treated as caller-authoritative and would pin the stale text forever, and KeyPoints with it",
		"KeyPoints": "produced only by the summarize stage, alongside Summary",
	},
	// Owned by the verb itself: identity, lifecycle, and the valid-time axis.
	"structural": {
		"ID":           "a new ULID; the successor is a new record linked by a supersedes association",
		"CreatedAt":    "when this revision was created",
		"UpdatedAt":    "same instant",
		"State":        "StateActive; the predecessor is soft-deleted in the same batch",
		"Associations": "engram-engram edges are written as keyspace rows in the batch, never as a carried slice on the struct",
		"ValidFrom":    "the effectiveAt boundary; old.ValidUntil = new.ValidFrom (half-open windows meet exactly)",
		"ValidUntil":   "open on the successor by construction — it is the current version",
	},
}

// TestEvolve_FieldCensus is the TestAppendMode_MethodCensus (SEC-15) pattern
// applied to a struct instead of a method set: every storage.Engram field must
// appear in exactly one bucket, and the "inherited" bucket is then asserted
// BEHAVIORALLY against a predecessor written with distinctive values.
//
// COG-37: a content-replacing verb carries every non-content-derived field of
// the record it replaces, and the split is an explicit exhaustive classification
// pinned here.
func TestEvolve_FieldCensus(t *testing.T) {
	rt := reflect.TypeOf(storage.Engram{})

	// 1. Exhaustiveness and disjointness.
	seen := map[string]string{}
	for bucket, fields := range evolveFieldBuckets {
		for name, reason := range fields {
			if prev, dup := seen[name]; dup {
				t.Errorf("field %q classified in two buckets (%q and %q)", name, prev, bucket)
			}
			seen[name] = bucket
			if reason == "" {
				t.Errorf("field %q in bucket %q has no reason; a bucket entry without a "+
					"reason is a rubber stamp", name, bucket)
			}
			if _, ok := rt.FieldByName(name); !ok {
				t.Errorf("classified field %q does not exist on storage.Engram — stale census entry", name)
			}
		}
	}
	var unclassified []string
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if _, ok := seen[name]; !ok {
			unclassified = append(unclassified, name)
		}
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Fatalf("storage.Engram field(s) %v are not classified in evolveFieldBuckets. "+
			"A content-replacing verb must decide EXPLICITLY whether each field is carried, "+
			"reset, content-derived or structural (COG-37) — add each one to a bucket with a "+
			"reason, and make evolveAtInternal do what the bucket says.", unclassified)
	}

	// 2. Behavioral check of the "inherited" bucket, against a predecessor
	// written with values distinct from every zero value and every default.
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)

	imp := float32(0.42)
	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:      "trailwork",
		Concept:    "Boardwalk plank rotation",
		Content:    "Boardwalk planks are rotated end-for-end every third season.",
		Tags:       []string{"boardwalk", "schedule"},
		MemoryType: uint8(storage.TypeProcedure),
		TypeLabel:  "maintenance_routine",
		Importance: &imp,
		Trust:      "verified",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	pre := readEngram(t, eng, "trailwork", resp.ID)

	newID, err := eng.Evolve(ctx, "trailwork", resp.ID,
		"Boardwalk planks are rotated end-for-end every second season on the shaded span.",
		"revised the interval", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	post := readEngram(t, eng, "trailwork", newID.String())

	preV, postV := reflect.ValueOf(*pre), reflect.ValueOf(*post)
	for name := range evolveFieldBuckets["inherited"] {
		a := preV.FieldByName(name).Interface()
		b := postV.FieldByName(name).Interface()
		if !reflect.DeepEqual(a, b) {
			t.Errorf("inherited field %q: predecessor %v, successor %v — the census says this "+
				"field is CARRIED and evolveAtInternal did not carry it", name, a, b)
		}
	}
	// Guard against a fixture that would pass vacuously.
	if pre.Trust == storage.TrustInferred {
		t.Fatalf("fixture is not discriminating: predecessor trust=%v is the value the old "+
			"hardcode produced, so this test would pass either way", pre.Trust)
	}
}

// TestConsolidate_KnownResidual_DoesNotCarryTrust is a PINNED KNOWN RESIDUAL:
// it passes while the gap exists and FAILS WHEN IT IS FIXED, so the fix has to
// amend COG-37 with it.
//
// COG-37's rule is written for every content-replacing verb. Engine.Consolidate
// is one (#779), and it does not obey the rule yet: it builds the merged memory
// through Engine.Write with no trust. Exactly what that yields:
//
//   - merged text that is NEW (the usual case) creates a memory at inferred,
//     whatever its sources held. Merging a memory flagged untrusted therefore
//     still clears the flag — the laundering #872 closed on evolve, by the
//     neighbouring verb;
//   - merged text byte-identical to one of the inputs creates nothing: Write's
//     content dedup returns THAT input, which survives with its own trust.
//
// Deliberately not fixed with evolve: a many-to-one join needs its own rule
// (which source's trust wins?) and that is a design decision, not a carry.
func TestConsolidate_KnownResidual_DoesNotCarryTrust(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)

	flagged, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "trailwork",
		Concept: "Footbridge load rating",
		Content: "Someone said the Larkspur footbridge is rated for pack animals.",
		Trust:   "untrusted",
	})
	if err != nil {
		t.Fatalf("Write flagged: %v", err)
	}
	other, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "trailwork",
		Concept: "Footbridge span",
		Content: "The Larkspur footbridge spans eleven metres.",
	})
	if err != nil {
		t.Fatalf("Write other: %v", err)
	}
	if pre := readEngram(t, eng, "trailwork", flagged.ID); pre.Trust != storage.TrustUntrusted {
		t.Fatalf("premise not reproduced: source trust = %d (%s), want untrusted", pre.Trust, pre.Trust.String())
	}

	res, err := eng.Consolidate(ctx, "trailwork", []string{flagged.ID, other.ID},
		"The Larkspur footbridge spans eleven metres and is said to be rated for pack animals.")
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	merged := readEngram(t, eng, "trailwork", res.MergedID.String())

	if merged.Trust != storage.TrustInferred {
		t.Fatalf("KNOWN RESIDUAL APPEARS FIXED: a newly created merged memory has trust=%d (%s), no "+
			"longer the inferred Consolidate used to mint whatever its sources held. Replace this "+
			"test with one that pins the new join rule and remove the Consolidate residual from COG-37.",
			merged.Trust, merged.Trust.String())
	}

	// The other half of "exactly what that yields": merged text identical to an
	// input is a dedup hit, so that input IS the result and keeps its trust.
	const keptText = "A walker claims the Fernbrook stile is loose on the north side."
	kept, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "trailwork", Concept: "Stile condition", Content: keptText, Trust: "untrusted",
	})
	if err != nil {
		t.Fatalf("Write kept: %v", err)
	}
	dup, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "trailwork", Concept: "Stile note", Content: "The Fernbrook stile is on the north boundary.",
	})
	if err != nil {
		t.Fatalf("Write dup: %v", err)
	}
	res2, err := eng.Consolidate(ctx, "trailwork", []string{kept.ID, dup.ID}, keptText)
	if err != nil {
		t.Fatalf("Consolidate (identical text): %v", err)
	}
	if res2.MergedID.String() != kept.ID {
		t.Fatalf("premise not reproduced: merged text identical to an input returned %s, not that "+
			"input %s — the dedup-hit case this half describes did not occur", res2.MergedID.String(), kept.ID)
	}
	if got := readEngram(t, eng, "trailwork", kept.ID).Trust; got != storage.TrustUntrusted {
		t.Errorf("the surviving input's trust = %d (%s), want untrusted: a dedup hit returns the "+
			"input itself and must leave its trust alone", got, got.String())
	}
}

// ---------------------------------------------------------------------------
// SEC-14: one gate for verified, however it would arrive.
//
// resolveTrust decides which context modes may assert trust=verified on a
// write. The same function decides whether an evolve may CARRY verified onto
// its successor and whether SetTrust may set it. It is checked in the engine,
// as defence in depth: the rule holds independent of how a call was routed.
//
//	A context whose mode may not assert verified may not produce a verified
//	record by any engine path.
// ---------------------------------------------------------------------------

// trustModeContext returns a context carrying the given mode; "" yields a
// context that carries none.
func trustModeContext(mode string) context.Context {
	if mode == "" {
		return context.Background()
	}
	return withMode(mode)
}

// TestUpsertEvolve_VerifiedCarryNeedsTheStampGate: a content-change re-sync that
// supplies NO trust would carry the head's verified. Under a mode that may not
// assert verified it is refused as an invalid request — not downgraded to
// inferred, which would be the silent demotion — and nothing is written.
func TestUpsertEvolve_VerifiedCarryNeedsTheStampGate(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	const headText = "The Fernbrook crossing passed its load test."
	first, err := eng.Write(withMode(auth.ModeWrite), &mbp.WriteRequest{
		Vault:        "trailwork",
		Concept:      "Crossing load test",
		Content:      headText,
		IdempotentID: "crossing-load-test",
		UpsertMode:   true,
		Trust:        "verified",
	})
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	if got := readEngram(t, eng, "trailwork", first.ID).Trust; got != storage.TrustVerified {
		t.Fatalf("premise not reproduced: head trust = %d (%s), want verified", got, got.String())
	}

	resp, err := eng.Write(withMode(auth.ModeObserve), &mbp.WriteRequest{
		Vault:        "trailwork",
		Content:      "The Fernbrook crossing failed its load test.",
		IdempotentID: "crossing-load-test",
		UpsertMode:   true,
		// no trust: the successor would inherit verified
	})
	if err == nil {
		got := readEngram(t, eng, "trailwork", resp.ID)
		t.Fatalf("a context whose mode may not assert verified got a successor (%s, hint %q) with "+
			"trust=%d (%s)", resp.ID, resp.Hint, got.Trust, got.Trust.String())
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("refusal is not an ErrInvalidRequest: %v", err)
	}
	assertHeadUntouched(t, eng, "trailwork", "crossing-load-test", first.ID, headText, storage.TrustVerified)
}

// TestEvolve_VerifiedCarryNeedsTheStampGate is the same rule on the plain evolve
// path.
func TestEvolve_VerifiedCarryNeedsTheStampGate(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	const headText = "The cooperative signed off the Larkspur handrail."
	first, err := eng.Write(withMode(auth.ModeWrite), &mbp.WriteRequest{
		Vault:   "trailwork",
		Concept: "Handrail sign-off",
		Content: headText,
		Trust:   "verified",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	newID, err := eng.Evolve(withMode(auth.ModeObserve), "trailwork", first.ID,
		"The cooperative rejected the Larkspur handrail.", "reworded", nil, "")
	if err == nil {
		got := readEngram(t, eng, "trailwork", newID.String())
		t.Fatalf("a context whose mode may not assert verified got a successor (%s) with trust=%d (%s)",
			newID.String(), got.Trust, got.Trust.String())
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("refusal is not an ErrInvalidRequest: %v", err)
	}
	assertHeadUntouched(t, eng, "trailwork", "", first.ID, headText, storage.TrustVerified)
}

// TestEvolve_VerifiedCarry_ClearsTheStampGate is the per-mode table that pins
// the single gate. For each mode — the four the system defines, one it does
// not, and a context carrying none — it measures against the real engine:
//
//	stamp             a plain write asserting trust=verified is accepted
//	evolve            Evolve of a verified head yields a verified successor
//	upsertEvolve      an upsert content change with no trust yields a verified successor
//	setTrustVerified  SetTrust(id, "verified") is accepted
//
// and asserts the invariant: a mode that may not assert verified on a write
// gets a verified record by none of the other three. The modes that may assert
// it (full, write) are pinned to succeed in every column, so the table cannot
// pass by refusing everything. There is no canonical list of modes to derive
// the rows from, so a mode added later must be added here by hand; the
// "unrecognised" row stands in for any mode nobody has listed.
func TestEvolve_VerifiedCarry_ClearsTheStampGate(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	const vault = "trailwork"
	setup := withMode(auth.ModeWrite)

	modes := []string{auth.ModeFull, auth.ModeWrite, auth.ModeAppend, auth.ModeObserve, "unrecognised", ""}
	for i, mode := range modes {
		ctx := trustModeContext(mode)
		label := mode
		if label == "" {
			label = "(none)"
		}

		// stamp
		_, stampErr := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("Waymark audit %d", i),
			Content: fmt.Sprintf("Waymark audit %d was signed by the trail warden.", i),
			Trust:   "verified",
		})
		stamp := stampErr == nil

		// evolve
		head, err := eng.Write(setup, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("Ford depth gauge %d", i),
			Content: fmt.Sprintf("Ford depth gauge %d reads true.", i),
			Trust:   "verified",
		})
		if err != nil {
			t.Fatalf("[%s] seed verified head: %v", label, err)
		}
		evolve := false
		if newID, evErr := eng.Evolve(ctx, vault, head.ID,
			fmt.Sprintf("Ford depth gauge %d reads two centimetres high.", i), "recalibrated", nil, ""); evErr == nil {
			got := readEngram(t, eng, vault, newID.String()).Trust
			if got != storage.TrustVerified {
				t.Errorf("[%s] evolve of a verified head succeeded with trust=%d (%s): accepted and "+
					"silently downgraded, which SEC-14 forbids", label, got, got.String())
			}
			evolve = got == storage.TrustVerified
		}

		// upsertEvolve
		key := fmt.Sprintf("cairn-count-%d", i)
		if _, err := eng.Write(setup, &mbp.WriteRequest{
			Vault:        vault,
			Concept:      fmt.Sprintf("Cairn count %d", i),
			Content:      fmt.Sprintf("Survey %d counted nine cairns on the ridge.", i),
			IdempotentID: key,
			UpsertMode:   true,
			Trust:        "verified",
		}); err != nil {
			t.Fatalf("[%s] seed verified upsert head: %v", label, err)
		}
		upsertEvolve := false
		if resp, upErr := eng.Write(ctx, &mbp.WriteRequest{
			Vault:        vault,
			Content:      fmt.Sprintf("Survey %d counted eleven cairns on the ridge.", i),
			IdempotentID: key,
			UpsertMode:   true,
		}); upErr == nil {
			if resp.Hint != "upsert-evolved" {
				t.Fatalf("[%s] premise not reproduced: hint = %q, want upsert-evolved", label, resp.Hint)
			}
			got := readEngram(t, eng, vault, resp.ID).Trust
			if got != storage.TrustVerified {
				t.Errorf("[%s] upsert content change on a verified head succeeded with trust=%d (%s): "+
					"accepted and silently downgraded, which SEC-14 forbids", label, got, got.String())
			}
			upsertEvolve = got == storage.TrustVerified
		}

		// setTrustVerified
		plain, err := eng.Write(setup, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("Signpost check %d", i),
			Content: fmt.Sprintf("Signpost check %d found every post upright.", i),
		})
		if err != nil {
			t.Fatalf("[%s] seed inferred memory: %v", label, err)
		}
		setTrust := false
		if stErr := eng.SetTrust(ctx, vault, plain.ID, "verified"); stErr == nil {
			got := readEngram(t, eng, vault, plain.ID).Trust
			if got != storage.TrustVerified {
				t.Errorf("[%s] SetTrust(verified) returned nil but the stored trust is %d (%s)", label, got, got.String())
			}
			setTrust = got == storage.TrustVerified
		}

		t.Logf("mode=%-12s stampVerifiedOnWrite=%-5v evolve=%-5v upsertEvolve=%-5v setTrustVerified=%v",
			label, stamp, evolve, upsertEvolve, setTrust)

		if (evolve || upsertEvolve || setTrust) && !stamp {
			t.Errorf("[%s] GATE MISMATCH: this mode may not assert verified on a write (%v) but got a "+
				"verified record another way (evolve=%v upsertEvolve=%v setTrustVerified=%v). Every "+
				"engine path that sets or carries verified must clear resolveTrust (SEC-14).",
				label, stampErr, evolve, upsertEvolve, setTrust)
		}
		if mode == auth.ModeFull || mode == auth.ModeWrite {
			if !stamp || !evolve || !upsertEvolve || !setTrust {
				t.Errorf("[%s] a mode that may assert verified was refused somewhere (stamp=%v evolve=%v "+
					"upsertEvolve=%v setTrustVerified=%v): the gate is over-blocking",
					label, stamp, evolve, upsertEvolve, setTrust)
			}
		}
	}
}

// TestSetTrust_VerifiedNeedsTheStampGate: setting verified on an existing
// memory goes through the same resolveTrust as asserting it on a write. A
// context whose mode may not assert verified is refused as an invalid request
// and the stored level does not move.
func TestSetTrust_VerifiedNeedsTheStampGate(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	const vault = "trailwork"

	w, err := eng.Write(withMode(auth.ModeWrite), &mbp.WriteRequest{
		Vault: vault, Concept: "Ridge cairn height", Content: "The ridge cairn stands waist high.",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, mode := range []string{auth.ModeObserve, "unrecognised", ""} {
		label := mode
		if label == "" {
			label = "(none)"
		}
		err := eng.SetTrust(trustModeContext(mode), vault, w.ID, "verified")
		if err == nil {
			t.Errorf("[%s] SetTrust(verified) was accepted from a context whose mode may not assert verified", label)
		} else if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("[%s] refusal is not an ErrInvalidRequest: %v", label, err)
		}
		if got := readEngram(t, eng, vault, w.ID).Trust; got != storage.TrustInferred {
			t.Fatalf("[%s] stored trust is now %d (%s); a SetTrust from this mode must not move it", label, got, got.String())
		}
	}
	// A mode that may assert verified still can.
	if err := eng.SetTrust(withMode(auth.ModeWrite), vault, w.ID, "verified"); err != nil {
		t.Fatalf("SetTrust(verified) under a mode that may assert it: %v", err)
	}
	if got := readEngram(t, eng, vault, w.ID).Trust; got != storage.TrustVerified {
		t.Errorf("stored trust = %d (%s), want verified", got, got.String())
	}
}

// ---------------------------------------------------------------------------
// SetTrust reaches the version recall serves, or says where that version is.
// ---------------------------------------------------------------------------

// TestSetTrust_SupersededTargetIsRefusedAndNamesTheHead: a trust level set on a
// retired version changes nothing a reader sees. The write used to succeed
// against the dead version while the live head kept its level, so a revocation
// aimed at a stale id was silently lost. It is now refused the way an evolve of
// a superseded target is, naming the current version — across a chain longer
// than the 8-hop slack other walks keep.
func TestSetTrust_SupersededTargetIsRefusedAndNamesTheHead(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)
	const vault = "trailwork"

	first, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: vault, Concept: "Rope bridge capacity",
		Content: "The rope bridge carries two walkers at a time.", Trust: "verified",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	const hops = 10
	ids := []string{first.ID}
	for hop := 1; hop <= hops; hop++ {
		newID, err := eng.Evolve(ctx, vault, ids[len(ids)-1],
			fmt.Sprintf("The rope bridge carries two walkers at a time (survey %d).", hop), "resurveyed", nil, "")
		if err != nil {
			t.Fatalf("Evolve hop %d: %v", hop, err)
		}
		ids = append(ids, newID.String())
	}
	head := ids[len(ids)-1]

	for _, stale := range []string{ids[0], ids[hops/2]} {
		err := eng.SetTrust(ctx, vault, stale, "untrusted")
		if err == nil {
			t.Fatalf("SetTrust(untrusted) on the retired version %s reported success: the revocation "+
				"went to a dead version (now trust=%s) while the live head %s is still trust=%s",
				stale, readEngram(t, eng, vault, stale).Trust.String(), head, readEngram(t, eng, vault, head).Trust.String())
		}
		if !errors.Is(err, ErrTrustTargetSuperseded) {
			t.Errorf("refusal is not ErrTrustTargetSuperseded: %v", err)
		}
		if !strings.Contains(err.Error(), head) {
			t.Errorf("refusal does not name the current version %s, so the caller cannot retry on it: %v", head, err)
		}
		if got := readEngram(t, eng, vault, stale).Trust; got != storage.TrustVerified {
			t.Errorf("a refused SetTrust still changed the retired version: trust=%d (%s)", got, got.String())
		}
	}

	// The level the caller wanted, on the id the refusal named.
	if err := eng.SetTrust(ctx, vault, head, "untrusted"); err != nil {
		t.Fatalf("SetTrust on the current version: %v", err)
	}
	if got := readEngram(t, eng, vault, head).Trust; got != storage.TrustUntrusted {
		t.Errorf("current version trust = %d (%s), want untrusted", got, got.String())
	}
}

// TestSetTrust_RacingAnEvolve_NeverLandsOnARetiredVersion: SetTrust and an
// evolve of the same memory, started together. Whatever the interleaving, a
// SetTrust that reports success must be visible on the live version — it ran
// first and the evolve carried it — and one that lost the race must be refused
// naming the successor. The outcome it must never have: success reported, level
// written to the version the evolve just retired.
func TestSetTrust_RacingAnEvolve_NeverLandsOnARetiredVersion(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)
	const vault = "trailwork"
	const rounds = 200

	landed, refused := 0, 0
	for i := 0; i < rounds; i++ {
		w, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault: vault, Concept: fmt.Sprintf("Marker post %d", i),
			Content: fmt.Sprintf("Marker post %d stands at the ridge junction.", i),
		})
		if err != nil {
			t.Fatalf("round %d: Write: %v", i, err)
		}

		var (
			wg           sync.WaitGroup
			start        = make(chan struct{})
			newID        storage.ULID
			evErr, trErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			newID, evErr = eng.Evolve(ctx, vault, w.ID,
				fmt.Sprintf("Marker post %d stands at the ridge junction, repainted.", i), "repainted", nil, "")
		}()
		go func() {
			defer wg.Done()
			<-start
			// Vary who reaches the memory first without sleeping: yield a
			// round-dependent number of times, so the rounds cover SetTrust
			// first, the evolve first, and the overlap between.
			for k := 0; k < (i%16)*4; k++ {
				runtime.Gosched()
			}
			trErr = eng.SetTrust(ctx, vault, w.ID, "untrusted")
		}()
		close(start)
		wg.Wait()

		if evErr != nil {
			t.Fatalf("round %d: Evolve: %v", i, evErr)
		}
		live := readEngram(t, eng, vault, newID.String())
		switch {
		case trErr == nil:
			landed++
			if live.Trust != storage.TrustUntrusted {
				t.Fatalf("round %d: SetTrust(untrusted) reported success, but the live version %s has "+
					"trust=%d (%s): the level was written to the version the evolve retired and is lost",
					i, newID.String(), live.Trust, live.Trust.String())
			}
		case errors.Is(trErr, ErrTrustTargetSuperseded):
			refused++
			if !strings.Contains(trErr.Error(), newID.String()) {
				t.Fatalf("round %d: refusal does not name the successor %s: %v", i, newID.String(), trErr)
			}
			if live.Trust != storage.TrustInferred {
				t.Fatalf("round %d: SetTrust was refused, yet the live version's trust is %d (%s)",
					i, live.Trust, live.Trust.String())
			}
		default:
			t.Fatalf("round %d: SetTrust: unexpected error: %v", i, trErr)
		}
	}
	t.Logf("%d rounds: the level landed first and was carried %d times; SetTrust was refused naming the successor %d times",
		rounds, landed, refused)
}

// ---------------------------------------------------------------------------
// The upsert path resolves a caller's trust and confidence before it picks a
// branch.
// ---------------------------------------------------------------------------

// TestUpsert_TrustIsResolvedOnEveryBranch: the identical-content branch writes
// nothing, and it used to skip validation with it — trust="nonsense" was
// accepted there with no error, as was a verified the caller may not assert.
// The label is now resolved before the branch is chosen. The VALUES still apply
// only where something is written: an identical-content re-sync stays a no-op.
func TestUpsert_TrustIsResolvedOnEveryBranch(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)

	const text = "The lower loop footbridge was re-boarded in June."
	first, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "trailwork", Concept: "Footbridge boards", Content: text,
		IdempotentID: "footbridge-boards", UpsertMode: true,
	})
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	same := func(c context.Context, trust string, confidence float32) (*mbp.WriteResponse, error) {
		return eng.Write(c, &mbp.WriteRequest{
			Vault: "trailwork", Concept: "Footbridge boards", Content: text,
			IdempotentID: "footbridge-boards", UpsertMode: true,
			Trust: trust, Confidence: confidence,
		})
	}

	if resp, err := same(ctx, "nonsense", 0); err == nil {
		t.Errorf("identical-content re-sync accepted an invalid trust label (hint %q)", resp.Hint)
	} else if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("invalid trust label: error is not ErrInvalidRequest: %v", err)
	}
	if resp, err := same(withMode(auth.ModeObserve), "verified", 0); err == nil {
		t.Errorf("identical-content re-sync accepted trust=verified from a mode that may not assert "+
			"it (hint %q)", resp.Hint)
	} else if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("unauthorised verified: error is not ErrInvalidRequest: %v", err)
	}

	// A valid label on identical content: accepted, and still a no-op.
	resp, err := same(ctx, "external", 0.5)
	if err != nil {
		t.Fatalf("identical-content re-sync with a valid trust: %v", err)
	}
	if resp.Hint != "upsert-identical" || resp.ID != first.ID {
		t.Fatalf("premise not reproduced: hint=%q id=%s, want upsert-identical on %s", resp.Hint, resp.ID, first.ID)
	}
	got := readEngram(t, eng, "trailwork", first.ID)
	if got.Trust != storage.TrustInferred || got.Confidence < 0.999 {
		t.Errorf("an identical-content re-sync changed the record: trust=%d (%s) confidence=%v, want "+
			"inferred/1.0 — it is documented as a no-op", got.Trust, got.Trust.String(), got.Confidence)
	}
	assertHeadUntouched(t, eng, "trailwork", "footbridge-boards", first.ID, text, storage.TrustInferred)
}

// ---------------------------------------------------------------------------
// Confidence is range-checked wherever a request's value is stored.
// ---------------------------------------------------------------------------

// TestConfidenceFromRequest is the table for the helper: (0,1] is kept, above 1
// clamps to 1, and NaN, zero and negatives mean "not supplied" (0, the unset
// sentinel that reads back as 1.0).
func TestConfidenceFromRequest(t *testing.T) {
	cases := []struct {
		name string
		in   float32
		want float32
	}{
		{"zero is not supplied", 0, 0},
		{"negative is not supplied", -0.5, 0},
		{"NaN is not supplied", float32(math.NaN()), 0},
		{"-Inf is not supplied", float32(math.Inf(-1)), 0},
		{"small positive is kept", 0.01, 0.01},
		{"mid-range is kept", 0.6, 0.6},
		{"one is kept", 1, 1},
		{"above one clamps", 5, 1},
		{"+Inf clamps", float32(math.Inf(1)), 1},
	}
	for _, c := range cases {
		if got := confidenceFromRequest(c.in); got != c.want {
			t.Errorf("%s: confidenceFromRequest(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestStoredConfidence_IsRangeChecked drives the three places a request's
// confidence is stored — create, batch create and the upsert content-change
// step — with out-of-range values. Confidence multiplies straight into the
// recall score, so a stored 5 outranks every honest memory. The upsert step is
// the one #872 made live (a re-sync's confidence used to be dropped); the two
// create paths had the same hole before it.
func TestStoredConfidence_IsRangeChecked(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)
	const vault = "trailwork"

	cases := []struct {
		name string
		in   float32
		want float32 // as read back
	}{
		{"above one clamps to one", 5, 1},
		{"negative is not supplied", -0.5, 1},
		{"NaN is not supplied", float32(math.NaN()), 1},
		{"in range is kept", 0.6, 0.6},
	}
	near := func(a, b float32) bool { return a > b-0.001 && a < b+0.001 }

	for i, c := range cases {
		// create
		w, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault: vault, Concept: fmt.Sprintf("Gate count %d", i),
			Content: fmt.Sprintf("Gate count %d on the valley path is fourteen.", i), Confidence: c.in,
		})
		if err != nil {
			t.Fatalf("%s: Write: %v", c.name, err)
		}
		if got := readEngram(t, eng, vault, w.ID).Confidence; !near(got, c.want) {
			t.Errorf("create, %s: requested %v, stored confidence reads %v, want %v", c.name, c.in, got, c.want)
		}

		// batch create
		resps, errs := eng.WriteBatch(ctx, []*mbp.WriteRequest{{
			Vault: vault, Concept: fmt.Sprintf("Stile count %d", i),
			Content: fmt.Sprintf("Stile count %d on the valley path is six.", i), Confidence: c.in,
		}})
		if errs[0] != nil {
			t.Fatalf("%s: WriteBatch: %v", c.name, errs[0])
		}
		if got := readEngram(t, eng, vault, resps[0].ID).Confidence; !near(got, c.want) {
			t.Errorf("batch create, %s: requested %v, stored confidence reads %v, want %v", c.name, c.in, got, c.want)
		}

		// upsert content change
		key := fmt.Sprintf("bench-count-%d", i)
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault: vault, Concept: fmt.Sprintf("Bench count %d", i),
			Content:      fmt.Sprintf("Bench count %d on the valley path is three.", i),
			IdempotentID: key, UpsertMode: true, Confidence: 0.8,
		}); err != nil {
			t.Fatalf("%s: upsert create: %v", c.name, err)
		}
		up, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:        vault,
			Content:      fmt.Sprintf("Bench count %d on the valley path is four.", i),
			IdempotentID: key, UpsertMode: true, Confidence: c.in,
		})
		if err != nil {
			t.Fatalf("%s: upsert evolve: %v", c.name, err)
		}
		if up.Hint != "upsert-evolved" {
			t.Fatalf("%s: premise not reproduced: hint = %q, want upsert-evolved", c.name, up.Hint)
		}
		if got := readEngram(t, eng, vault, up.ID).Confidence; !near(got, c.want) {
			t.Errorf("upsert content change, %s: requested %v, stored confidence reads %v, want %v",
				c.name, c.in, got, c.want)
		}
	}
}
