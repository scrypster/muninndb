package engine

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Standing = the record's trust and confidence. #872: evolve INHERITS standing
// and REPLACES content. The verb never manufactures either field.
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

// TestEvolve_InheritsConfidence: 1.0 is not a neutral default, it is a
// manufactured assertion of maximum belief. A caller that stored a fact at 0.4
// and then sharpened its wording must not find the engine has decided the fact
// is now certain — confidence multiplies straight into the recall score, so the
// reset was a 2.5x score inflation on every evolve of a low-confidence memory.
func TestEvolve_InheritsConfidence(t *testing.T) {
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
	if got := readEngram(t, eng, "trailwork", newID.String()).Confidence; got < 0.399 || got > 0.401 {
		t.Errorf("successor confidence = %v, want 0.4 (inherited): evolve manufactured a "+
			"belief the caller never asserted", got)
	}
}

// TestEvolve_LegacyZeroConfidenceStaysOne pins the normalization edge: storage
// decodes a stored 0 confidence as 1.0, so a legacy record that never carried
// the field lands on exactly the value it has today. Inheritance must not turn
// that into a stored 0.
func TestEvolve_LegacyZeroConfidenceStaysOne(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "trailwork",
		Concept: "Signpost paint colour",
		Content: "Trail signposts are painted ochre.",
		// Confidence omitted entirely.
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	newID, err := eng.Evolve(ctx, "trailwork", resp.ID,
		"Trail signposts are painted ochre with a white cap band.", "added the band", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	if got := readEngram(t, eng, "trailwork", newID.String()).Confidence; got < 0.999 || got > 1.001 {
		t.Errorf("successor confidence = %v, want 1.0", got)
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
		t.Errorf("successor trust = %v, want inferred (TrustUnset normalizes on carry)", got)
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
// upsert-evolve branch falls back to inheritance, exactly like a plain evolve.
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
	if got.Confidence < 0.299 || got.Confidence > 0.301 {
		t.Errorf("confidence = %v, want 0.3 (inherited)", got.Confidence)
	}
}

// TestUpsertEvolve_VerifiedStillGated: SEC-14 must survive the new pass-through.
// An observe credential asking for verified on the content-change branch is
// REJECTED, never silently downgraded.
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
		t.Fatalf("upsert-evolve accepted trust=verified under an observe credential (id %s)", first.ID)
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
// the exact way #653 (MemoryType) and #872 (Trust, Confidence) were introduced.
//
// Each entry carries its reason. A future field dropped into a bucket without
// one is the failure mode this census cannot catch by itself.
var evolveFieldBuckets = map[string]map[string]string{
	// Carried from the predecessor. The successor is the same RECORD with new
	// text, so its standing and its classification travel with it.
	"inherited": {
		"Concept":    "the label of the thing being described; caller may override, else carried (#769 announces the carry)",
		"Tags":       "curation state, not content; muninn_update_tags is the way to change it",
		"MemoryType": "what kind of memory this is does not change because the wording did (#653)",
		"TypeLabel":  "free-form companion to MemoryType, same reasoning (#653)",
		"Importance": "caller-asserted priority; explicit override wins, unset stays unset so the type-table default keeps deriving",
		"Trust":      "standing, not content (#872). TrustUnset normalizes to TrustInferred on carry",
		"Confidence": "standing, not content (#872). 1.0 would be a manufactured assertion of maximum belief",
	},
	// Deliberately NOT carried: the successor starts these fresh.
	"reset": {
		"Stability":   "decay resistance restarts with the new text; the successor has its own forgetting curve",
		"LastAccess":  "use history belongs to the revision that was used, not to the new one",
		"AccessCount": "same: the successor has been accessed zero times",
		"Relevance":   "recomputed at read time from the decay curve; never meaningful as a carried literal",
		// TODO(#872 follow-up): both of these SHOULD arguably be inherited and
		// are recorded here honestly rather than quietly fixed in this
		// increment, which is about standing.
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
// COG-34: a content-replacing verb carries every non-content-derived field of
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
			"reset, content-derived or structural (COG-34) — add each one to a bucket with a "+
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
		Confidence: 0.55,
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
	if pre.Trust == storage.TrustInferred || pre.Confidence == 1.0 {
		t.Fatalf("fixture is not discriminating: predecessor trust=%v confidence=%v are the "+
			"values the old hardcodes produced, so this test would pass either way",
			pre.Trust, pre.Confidence)
	}
}

// ---------------------------------------------------------------------------
// The kill condition (design §6): does inheriting a contradiction-penalised
// confidence measurably suppress a RESOLVED memory?
// ---------------------------------------------------------------------------

// TestEvolve_ResolvedConflictSuccessorScoreWithinKillLine measures the accepted
// cost of confidence inheritance. Under COG-29, evolving one endpoint of a
// declared contradiction IS a resolution (the predecessor is soft-deleted, so
// clause 2 no longer holds). Today's reset also cleared the contradiction
// penalty as a side effect; inheritance carries it onto a successor whose
// conflict just ended.
//
// The design pre-committed the kill line: drop confidence inheritance if a
// resolved-conflict successor's recall score sits MORE THAN 5% below its
// pre-conflict score. One penalty application is 1.0 -> 0.975 by construction
// (cognitive.BayesianUpdate with EvidenceContradiction), a 2.5% haircut, and the
// penalty is fire-once per newly-flagged pair. >5% would mean it compounds
// across evolves — i.e. that inheritance re-opens the compounding bug #764
// closed.
//
// Confidence multiplies straight into AbsoluteScore, so the score ratio is
// measured on AbsoluteScore (Final is per-query normalized and would move for
// reasons that have nothing to do with this).
func TestEvolve_ResolvedConflictSuccessorScoreWithinKillLine(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeWrite)

	const vault = "trailwork"
	const query = "chainsaw certification requirement for volunteers"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "Chainsaw certification requirement",
		Content: "Volunteers need a chainsaw certification requirement before felling.",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	eng.WaitWriteTimeIdle()
	preScore := absoluteScoreFor(t, eng, vault, query, resp.ID)
	if preScore <= 0 {
		t.Fatalf("premise not reproduced: pre-conflict absolute score %v; the fixture is not "+
			"recallable and no ratio can be measured", preScore)
	}

	// Apply exactly one contradiction penalty, at the magnitude the worker
	// applies: BayesianUpdate(1.0, EvidenceContradiction) = 0.975.
	target, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID: %v", err)
	}
	if _, err := eng.AdjustConfidence(ctx, vault, target, -0.025, storage.ULID{}, false,
		"contradiction_detected", "test"); err != nil {
		t.Fatalf("AdjustConfidence: %v", err)
	}
	if got := readEngram(t, eng, vault, resp.ID).Confidence; got < 0.974 || got > 0.976 {
		t.Fatalf("premise not reproduced: penalised confidence = %v, want ~0.975", got)
	}

	// Resolve by evolving the endpoint, three hops. Confidence must stay FLAT:
	// the penalty is applied by the worker, never by the carry.
	id := resp.ID
	for hop := 1; hop <= 3; hop++ {
		newID, err := eng.Evolve(ctx, vault, id,
			fmt.Sprintf("Volunteers need a chainsaw certification requirement before felling (rev %d).", hop),
			"resolution", nil, "")
		if err != nil {
			t.Fatalf("Evolve hop %d: %v", hop, err)
		}
		id = newID.String()
		eng.WaitWriteTimeIdle()
		conf := readEngram(t, eng, vault, id).Confidence
		if conf < 0.974 || conf > 0.976 {
			t.Fatalf("hop %d: confidence = %v, want ~0.975 flat — the carried penalty is "+
				"COMPOUNDING across a supersede chain, which is exactly the #764 shape", hop, conf)
		}
	}

	postScore := absoluteScoreFor(t, eng, vault, query, id)
	if postScore <= 0 {
		t.Fatalf("successor not recallable at all (absolute score %v)", postScore)
	}
	drop := (preScore - postScore) / preScore
	t.Logf("pre-conflict absolute score %.6f, resolved successor %.6f, drop %.4f%%",
		preScore, postScore, drop*100)
	if drop > 0.05 {
		t.Fatalf("KILL CONDITION MET: resolved-conflict successor scores %.2f%% below "+
			"pre-conflict (kill line 5%%). Confidence inheritance must be dropped from this "+
			"increment; trust inheritance stands on its own.", drop*100)
	}
}

// absoluteScoreFor recalls a query and returns the AbsoluteScore of the row with
// the given id, or 0 if it is absent. AbsoluteScore is the component confidence
// multiplies into directly; Final is per-query normalized.
func absoluteScoreFor(t *testing.T, eng *Engine, vault, query, id string) float64 {
	t.Helper()
	resp, err := eng.Activate(context.Background(), &mbp.ActivateRequest{
		Vault: vault, Context: []string{query}, MaxResults: 20, Threshold: 0.0, ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	for _, it := range resp.Activations {
		if it.ID == id {
			return float64(it.ScoreComponents.AbsoluteScore)
		}
	}
	return 0
}
