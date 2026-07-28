package storage

import (
	"math"
	"testing"
)

// TestEffectiveImportance_TypeTable pins the documented memory-type derivation
// table: the values here ARE the contract (muninn_guide documents them and the
// pruner exemption depends on them). A change to the table must change this
// test deliberately.
func TestEffectiveImportance_TypeTable(t *testing.T) {
	cases := []struct {
		memType MemoryType
		want    float32
	}{
		{TypeDecision, 0.6},
		{TypeGoal, 0.6},
		{TypeConstraint, 0.6},
		{TypeIdentity, 0.6},
		{TypePreference, 0.5},
		{TypeProcedure, 0.5},
		{TypeFact, 0.4},
		{TypeReference, 0.4},
		{TypeIssue, 0.4},
		{TypeObservation, 0.3},
		{TypeEvent, 0.3},
		{TypeTask, 0.3},
		{MemoryType(200), 0.3}, // unknown types fall back to the floor tier
	}
	for _, c := range cases {
		got := EffectiveImportance(0, c.memType, TrustUnset)
		if got != c.want {
			t.Errorf("EffectiveImportance(0, %v, unset) = %v, want %v", c.memType, got, c.want)
		}
	}
}

// TestEffectiveImportance_ExplicitWins verifies an explicit (stored > 0)
// importance overrides the type table entirely — including the trust bump.
func TestEffectiveImportance_ExplicitWins(t *testing.T) {
	// Explicit low value on a high-tier type: explicit wins.
	if got := EffectiveImportance(0.15, TypeDecision, TrustVerified); got != 0.15 {
		t.Errorf("explicit 0.15 on verified decision = %v, want 0.15", got)
	}
	// Quantized-explicit-zero (the write path stores 0.01 for explicit 0.0):
	// still explicit, still wins over the derived 0.4 for a fact.
	if got := EffectiveImportance(0.01, TypeFact, TrustUnset); got != 0.01 {
		t.Errorf("explicit 0.01 = %v, want 0.01", got)
	}
	// Out-of-range stored value clamps to 1.
	if got := EffectiveImportance(3.0, TypeFact, TrustUnset); got != 1 {
		t.Errorf("explicit 3.0 = %v, want 1 (clamped)", got)
	}
}

// TestEffectiveImportance_TrustBump verifies the +0.1 verified bump applies
// only on the derived path and is capped at 1.0.
func TestEffectiveImportance_TrustBump(t *testing.T) {
	base := EffectiveImportance(0, TypeDecision, TrustUnset)
	bumped := EffectiveImportance(0, TypeDecision, TrustVerified)
	if diff := bumped - base; math.Abs(float64(diff)-0.1) > 1e-6 {
		t.Errorf("verified bump = %v, want 0.1", diff)
	}
	// Non-verified trust levels get no bump.
	for _, tr := range []TrustLevel{TrustInferred, TrustExternal, TrustUntrusted} {
		if got := EffectiveImportance(0, TypeDecision, tr); got != base {
			t.Errorf("trust %v changed derived importance: %v != %v", tr, got, base)
		}
	}
	if got := EffectiveImportance(0, TypeDecision, TrustVerified); got > 1 {
		t.Errorf("bumped importance %v exceeds 1.0", got)
	}
}

// TestEffectiveImportance_EngramAndMetaAgree pins that the Engram and
// EngramMeta derivations agree — EngramMeta now carries Trust precisely so
// the pruner's slim-metadata path honors the verified bump (a verified
// decision derives 0.7, exactly at HighImportanceFloor).
func TestEffectiveImportance_EngramAndMetaAgree(t *testing.T) {
	e := &Engram{MemoryType: TypeDecision, Trust: TrustVerified}
	m := &EngramMeta{MemoryType: TypeDecision, Trust: TrustVerified}
	if e.EffectiveImportance() != m.EffectiveImportance() {
		t.Errorf("Engram (%v) and EngramMeta (%v) derivations diverge",
			e.EffectiveImportance(), m.EffectiveImportance())
	}
	if got := m.EffectiveImportance(); got < HighImportanceFloor {
		t.Errorf("verified decision meta importance = %v, want >= HighImportanceFloor (%v)", got, HighImportanceFloor)
	}
	// Explicit path parity too.
	e.Importance, m.Importance = 0.9, 0.9
	if e.EffectiveImportance() != 0.9 || m.EffectiveImportance() != 0.9 {
		t.Errorf("explicit parity broken: engram=%v meta=%v", e.EffectiveImportance(), m.EffectiveImportance())
	}
}

// TestMetaDecodeCarriesTrust verifies the slim 0x02 metadata decode carries
// Trust (needed by the meta-path EffectiveImportance): write a verified
// engram, drop caches, and read back via GetMetadata.
func TestMetaDecodeCarriesTrust(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ws := store.VaultPrefix("imp-meta-trust")

	id, err := store.WriteEngram(t.Context(), ws, &Engram{
		Concept: "verified decision",
		Content: "we chose pebble",
		Trust:   TrustVerified,
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	// Force the slow path (Pebble read + erf.DecodeMeta), not the caches.
	store.cache.Delete(ws, id)
	store.metaCache.Remove([16]byte(id))

	metas, err := store.GetMetadata(t.Context(), ws, []ULID{id})
	if err != nil || len(metas) != 1 || metas[0] == nil {
		t.Fatalf("GetMetadata: %v (%v)", err, metas)
	}
	if metas[0].Trust != TrustVerified {
		t.Errorf("meta Trust = %v, want TrustVerified", metas[0].Trust)
	}
}
