package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// Issue #780 — the entity graph must not outlive the engrams that support it.
//
// Two of the issue's stated mechanisms do NOT hold, and both were checked
// against the code before anything was changed:
//
//   - "the counter is never decremented" is false. DecrementEntityCoOccurrence
//     exists, deletes the 0x24 key at zero, and DeleteEngram calls it. What is
//     true is narrower: it runs only on HARD delete, so supersession — which is
//     a soft-delete plus a stamp — never funds it.
//   - "evolve leaves the entity behind" is false in the DEFAULT case. EvolveAt
//     carries the predecessor's entity set onto the live successor and
//     re-increments every pair, so after a plain evolve the pair is supported by
//     a LIVE engram and reporting it is correct.
//
// The residual, which is real: when the caller REPLACES the entity set (the
// exact act of "we moved off that technology"), the predecessor is superseded
// and nothing at all still mentions the retired entity — yet the aggregate keeps
// reporting the pair, because both views read capture-time ledgers that only a
// hard delete can correct. The same response's `engrams` list already excludes
// the superseded records, so the aggregate contradicts itself.
//
// Every fixture below is synthetic.
// ---------------------------------------------------------------------------

const (
	entHub     = "Aurora Platform"
	entRetired = "Kestrel Queue"
	entCurrent = "Bellwether Cache"
)

// seedRetiredEntity writes two engrams pairing the hub with a technology, then
// evolves BOTH to a replacement technology by supplying an explicit entity list
// (which replaces the carried set). After this, no live engram mentions the
// retired entity at all.
func seedRetiredEntity(t *testing.T, eng *Engine) {
	t.Helper()
	ctx := context.Background()
	pair := func(name string) []mbp.InlineEntity {
		return []mbp.InlineEntity{
			{Name: entHub, Type: "project"},
			{Name: name, Type: "technology"},
		}
	}
	// minCount is 2, so the pair needs two supporting engrams to be reported
	// at all — a one-engram fixture would pass for the wrong reason.
	for _, content := range []string{
		"The ingest tier of the platform is backed by the queue.",
		"Queue retries on the platform are capped at five attempts.",
	} {
		w, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault: "default", Concept: "ingest tier", Content: content,
			Entities: pair(entRetired),
		})
		if err != nil {
			t.Fatalf("seed write: %v", err)
		}
		if _, err := eng.EvolveAt(ctx, "default", w.ID,
			content+" (now served from the cache tier)", "moved off the queue",
			nil, "ingest tier", pair(entCurrent), nil, timeZeroEngine()); err != nil {
			t.Fatalf("evolve: %v", err)
		}
	}
}

func TestEntityAggregate_DropsCoOccurrenceWithNoLiveSupport(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	seedRetiredEntity(t, eng)

	agg, err := eng.GetEntityAggregate(ctx, "default", entHub, 0)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if agg == nil {
		t.Fatalf("no aggregate for %q", entHub)
	}

	var sawCurrent bool
	for _, c := range agg.CoOccurring {
		if c.Name == entRetired {
			t.Errorf("#780 NOT FIXED: co_occurring still reports %q (count %d) for %q, but NO live engram "+
				"mentions it — every supporting record was superseded. The same response's engrams list "+
				"already excludes those records, so the aggregate contradicts itself. co_occurring=%v",
				entRetired, c.Count, entHub, agg.CoOccurring)
		}
		if c.Name == entCurrent {
			sawCurrent = true
		}
	}
	if !sawCurrent {
		t.Errorf("co_occurring dropped %q, which two LIVE engrams support — the filter must remove stale "+
			"support, never live support. co_occurring=%v", entCurrent, agg.CoOccurring)
	}
}

func TestEntityAggregate_DropsRelationshipsWithNoLiveSupport(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	seedRetiredEntity(t, eng)

	agg, err := eng.GetEntityAggregate(ctx, "default", entHub, 0)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var sawCurrent bool
	for _, r := range agg.Relations {
		other := r.ToEntity
		if other == entHub {
			other = r.FromEntity
		}
		if other == entRetired {
			t.Errorf("#780 NOT FIXED: relationships still lists %s %q %q with no live engram behind it",
				r.FromEntity, r.RelType, r.ToEntity)
		}
		if other == entCurrent {
			sawCurrent = true
		}
	}
	if !sawCurrent {
		t.Errorf("relationships dropped the live %q edge — the filter must remove stale support, never live support", entCurrent)
	}
}

// The default evolve — no explicit entity list — CARRIES the entity set onto the
// live successor, so the pair keeps live support and must still be reported.
// This is the half of #780 that was never broken, and pinning it stops the fix
// from over-reaching into "evolve retires entities", which it must not.
func TestEntityAggregate_CarriedEntitiesStayLive(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	for _, content := range []string{
		"The ingest tier of the platform is backed by the queue.",
		"Queue retries on the platform are capped at five attempts.",
	} {
		w, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault: "default", Concept: "ingest tier", Content: content,
			Entities: []mbp.InlineEntity{
				{Name: entHub, Type: "project"},
				{Name: entRetired, Type: "technology"},
			},
		})
		if err != nil {
			t.Fatalf("seed write: %v", err)
		}
		// No entities argument: the predecessor's set is carried forward.
		if _, err := eng.EvolveAt(ctx, "default", w.ID, content+" (reworded)", "clarity",
			nil, "ingest tier", nil, nil, timeZeroEngine()); err != nil {
			t.Fatalf("evolve: %v", err)
		}
	}

	agg, err := eng.GetEntityAggregate(ctx, "default", entHub, 0)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	found := false
	for _, c := range agg.CoOccurring {
		if c.Name == entRetired {
			found = true
		}
	}
	if !found {
		t.Errorf("a carried entity pair is supported by the LIVE successor and must still be reported; co_occurring=%v", agg.CoOccurring)
	}
}

// The live-support set and the ledger must agree on ONE key, and so must the
// subject lookup. The 0x24/0x1F ledgers canonicalize with
// keys.NormalizeEntityName (NFKC, trimmed, lowercased) but a 0x24 record keeps
// the FIRST writer's raw spelling forever, while each engram's 0x20 links keep
// their own. So a raw-string live-support set drops a LIVE edge as soon as the
// engram that created the ledger entry is superseded and the survivors spell
// the name differently — precisely the false-removal this filter must never do.
//
// The fixture is that sequence, not a contrivance: original memory retired,
// replacements written with different capitalization.
func TestEntityAggregate_LiveSupportIsCaseInsensitive(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	inline := func(hub, other string) []mbp.InlineEntity {
		return []mbp.InlineEntity{
			{Name: hub, Type: "project"},
			{Name: other, Type: "technology"},
		}
	}
	// The two engrams that CREATE the ledger entry, in title case.
	var seeded []string
	for _, content := range []string{
		"The ingest tier of the platform is backed by the cache.",
		"Cache retries on the platform are capped at five attempts.",
	} {
		w, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault: "default", Concept: "ingest tier", Content: content,
			Entities: inline(entHub, entCurrent),
		})
		if err != nil {
			t.Fatalf("seed write: %v", err)
		}
		seeded = append(seeded, w.ID)
	}
	// Both are superseded by successors naming the SAME entities in upper case.
	// The 0x24 record keeps the title-case spelling; no live engram has it.
	for i, id := range seeded {
		if _, err := eng.EvolveAt(ctx, "default", id, "reworded body text number "+string(rune('a'+i)),
			"rewrite", nil, "ingest tier",
			inline(strings.ToUpper(entHub), strings.ToUpper(entCurrent)), nil, timeZeroEngine()); err != nil {
			t.Fatalf("evolve: %v", err)
		}
	}

	// Asked for in a case that matches NEITHER stored spelling — which also pins
	// the subject-lookup half: step 1 finds the record by hash, so the edge lists
	// must be matched by the same key or the caller gets a record with empty
	// lists and no indication why.
	agg, err := eng.GetEntityAggregate(ctx, "default", strings.ToLower(entHub), 0)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if agg == nil {
		t.Fatalf("no aggregate for %q", strings.ToLower(entHub))
	}
	found := false
	for _, c := range agg.CoOccurring {
		if keys.NormalizeEntityName(c.Name) == keys.NormalizeEntityName(entCurrent) {
			found = true
		}
	}
	if !found {
		t.Errorf("a LIVE pair was dropped because the ledger and the live engrams spell the name "+
			"differently — the live-support filter must key on the normalized name, the same key the "+
			"ledger hashes. co_occurring=%v", agg.CoOccurring)
	}
}

func timeZeroEngine() time.Time { return time.Time{} }
