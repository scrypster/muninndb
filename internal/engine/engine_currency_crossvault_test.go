package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// --- Grok Gate A: cross-vault generalization for the currency detector -----
//
// engine_currency.go's constants (currencyFacetDF=3, currencyUbiquityRatio=
// 0.10 fixed 10%) and its version-marker vocabulary (currencyIsVersionMarker)
// were ALL measured/tuned on exactly ONE real vault (#716, the widgetflow
// sample). This file is adversarial: it builds SYNTHETIC vaults whose tag
// structure deliberately does NOT match that tuning vault, and records
// whether the detector (a) correctly silences, (b) safely-but-incompletely
// silences on a genuine chain (a "DOCUMENTED-GAP" — never desired, but never
// a correctness violation either, per the design's stated principle that a
// false negative is always the safe direction), or (c) false-positives a
// cluster that should not exist (a "BUG").
//
// Every vault here is 100% synthetic: invented product/tag names, no content
// or structure copied from any real vault. This file does not modify
// engine_currency.go or engine_currency_test.go, and reuses their harness
// (currencyTestHarness / newCurrencyHarness / h.write / h.scored / h.apply /
// h.pad) unchanged.
//
// Verdict labels used throughout, one per assertion block:
//   PASS            - detector behaves correctly (silences when it must,
//                      clusters when it should).
//   DOCUMENTED-GAP   - a genuine chain fails to cluster because the fixed
//                      constants/vocabulary don't fit this vault's structure.
//                      Safe direction (silence), but a real precision loss.
//   BUG             - a false cluster: two things that should NOT be
//                      annotated as version-related were.

// --- 1. TAGLESS vault: version chain with no tags, or only ubiquitous tags -

// TestCrossVault_NoTags_Silences: a genuine version chain (temporally
// separated, high similarity, differing "version" language in the content)
// that carries NO tags at all. currencyTagAnchor requires len(tags) > 0 on
// BOTH sides, so it cannot fire; there are no entities either, so
// currencyEntityAnchor cannot fire; and no RelRefines edge exists (write-time
// novelty never ran for these). Verdict: PASS — this is the correct,
// necessary silence, because a tagless/entity-sparse vault gives the
// detector no anchor signal to work with at all, and guessing from
// similarity alone is exactly what R2 replaced (cosine cannot discriminate a
// version chain from a coexisting variant on its own).
func TestCrossVault_NoTags_Silences(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	h.pad(110, "filler")
	base := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)

	// Distinct per-item unique tokens (matching the pattern the existing
	// ubiquitous-tag-population test uses) so write-time novelty's
	// Jaccard-over-fingerprint check cannot fire a RelRefines edge — the
	// point of this test is to isolate the "zero tags, zero entities" case,
	// not to accidentally exercise the KEPT RelRefines anchor path.
	c1 := h.write(writeOpts{
		concept:   "alpha gadget spec entry9001",
		content:   "alpha gadget spec entry9001 describes the initial rollout shape",
		embedding: embedAt(0),
		validFrom: base,
	})
	c2 := h.write(writeOpts{
		concept:   "alpha gadget spec entry9002",
		content:   "alpha gadget spec entry9002 describes the revised rollout shape",
		embedding: embedAt(4),
		validFrom: base.Add(30 * 24 * time.Hour),
	})
	c3 := h.write(writeOpts{
		concept:   "alpha gadget spec entry9003",
		content:   "alpha gadget spec entry9003 describes the final rollout shape",
		embedding: embedAt(8),
		validFrom: base.Add(60 * 24 * time.Hour),
	})

	results := h.scored(c1, 0.9, c2, 0.9, c3, 0.9)
	out := h.apply(results)

	for _, r := range out {
		if r.VersionCluster != "" || r.NewestOfCluster || r.PossiblySupersededBy != (storage.ULID{}) {
			t.Fatalf("BUG: tagless/entity-less chain must never be clustered (no anchor signal exists) — got %+v", r)
		}
	}
	t.Log("PASS: tagless genuine version chain silences (no anchor possible: no tags, no entities, no RelRefines edge)")
}

// TestCrossVault_OnlyUbiquitousTags_Silences: the chain DOES carry tags, but
// both are shared vault-wide above currencyUbiquityRatio — e.g. every
// engram in this vault carries a blanket process label. currencyTagAnchor's
// ubiquity filter (mirroring entityIDF's df->0 guard) strips these before
// counting shared tags, so the shared-tag floor (currencyTagShareMin=2) is
// never reached even though the two tags are nominally "shared". Verdict:
// PASS — this is exactly the ubiquity guard's designed job (it already has a
// same-vault regression test, TestCurrencyAnnotation_SilenceControl_
// UbiquitousTagPopulation); this test additionally confirms it holds for a
// vault whose write pattern is "everything gets the same 2 tags" rather than
// that test's larger population shape.
func TestCrossVault_OnlyUbiquitousTags_Silences(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	const ubiqA, ubiqB = "process-note", "team-standard"
	base := time.Now()

	// Pad so ubiqA/ubiqB reach clearly-ubiquitous df: every padding AND
	// chain engram carries both tags.
	for i := 0; i < 60; i++ {
		h.write(writeOpts{
			concept:   fmt.Sprintf("pad%d", i),
			content:   fmt.Sprintf("padding content entry unique%d about nothing related", i*997%9973),
			tags:      []string{ubiqA, ubiqB},
			embedding: embedAt(float64((i * 53) % 360)),
			validFrom: base.Add(time.Duration(i) * time.Hour),
		})
	}

	c1 := h.write(writeOpts{
		concept:   "beta widget rollout v1",
		content:   "beta widget rollout v1 describes the first stage plan",
		tags:      []string{ubiqA, ubiqB, "v1"},
		embedding: embedAt(0),
		validFrom: base.Add(200 * time.Hour),
	})
	c2 := h.write(writeOpts{
		concept:   "beta widget rollout v2 final",
		content:   "beta widget rollout v2 final describes the completed stage plan",
		tags:      []string{ubiqA, ubiqB, "v2", "final"},
		embedding: embedAt(4),
		validFrom: base.Add(400 * time.Hour),
	})

	results := h.scored(c1, 0.9, c2, 0.9)
	out := h.apply(results)
	for _, r := range out {
		if r.VersionCluster != "" {
			t.Fatalf("BUG: shared-but-ubiquitous tags (df ~97%% of vault) must not anchor a cluster — got %+v", r)
		}
	}
	t.Log("PASS: chain sharing only ubiquitous tags silences (ubiquity filter strips both shared tags below currencyTagShareMin)")
}

// --- 2. ALIEN-MARKER vault: genuine chain, version words outside the fixed -
//        vocabulary (currencyIsVersionMarker's hand-picked list).

// TestCrossVault_AlienMarkerVocabulary_Silences is table-driven over three
// invented version-marker vocabularies that a DIFFERENT team/vault might use
// instead of "v2"/"final"/"four-zone": "mk1"/"mk2" (manufacturing-style
// marks), "rev-a"/"rev-b" (letter revisions), "phase-alpha"/"phase-beta"
// (named phases). Each chain otherwise reproduces the tuning vault's
// structure exactly: shared non-ubiquitous topic+structure tags, temporal
// separation, high similarity. None of these tokens match
// currencyVersionMarkerRe (requires literal "v<digits>"),
// currencyCountedStructureRe (requires a number WORD or digit prefix, e.g.
// "four-zone"), or currencyVersionStatusMarkers (a fixed word list) — so
// currencyMarkerSet is empty on both sides, currencyTagAnchor's marker gate
// (`len(markersA) == 0 || len(markersB) == 0`) fails, and the pair never
// clusters. Verdict for all three: DOCUMENTED-GAP — a real, temporally
// separated, same-topic version chain fails to cluster purely because its
// vocabulary wasn't in the hand-picked list. Safe direction (no false
// pair emerges), but this is the concrete cost of a fixed vocabulary: it
// does not generalize past the words it was built from.
func TestCrossVault_AlienMarkerVocabulary_Silences(t *testing.T) {
	cases := []struct {
		name      string
		markerOld string
		markerNew string
	}{
		{"manufacturing-marks", "mk1", "mk2"},
		{"letter-revisions", "rev-a", "rev-b"},
		{"named-phases", "phase-alpha", "phase-beta"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, cleanup := newCurrencyHarness(t)
			defer cleanup()

			h.pad(110, "filler")
			base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

			const topicTag, structTag = "gizmoline", "framework"

			// Sanity: confirm neither marker token is recognized by the
			// current vocabulary at all (this is the premise of the test,
			// not the observed behavior — pin it directly).
			if currencyIsVersionMarker(tc.markerOld) || currencyIsVersionMarker(tc.markerNew) {
				t.Fatalf("test premise broken: %q/%q unexpectedly matched currencyIsVersionMarker — pick different alien tokens", tc.markerOld, tc.markerNew)
			}

			// Content deliberately carries unique per-item filler tokens (as
			// in TestCrossVault_NoTags_Silences above) so write-time
			// novelty's Jaccard-over-fingerprint check (threshold 0.70,
			// novelty.Threshold) cannot cross the bar and open a KEPT
			// RelRefines edge between c1/c2 regardless of how the marker
			// token itself tokenizes (a hyphenated marker like "rev-a"
			// changes the token count enough to shift Jaccard on its own —
			// confirmed empirically while building this test). Without this,
			// the test would sometimes pass via the UNRELATED RelRefines
			// anchor path instead of isolating the tag/marker-vocabulary
			// path this test targets.
			c1 := h.write(writeOpts{
				concept:   "gizmoline layout " + tc.markerOld,
				content:   fmt.Sprintf("gizmoline layout %s alpha-uniq-token-lorem ipsum quux zephyr blorptastic wibblefen", tc.markerOld),
				tags:      []string{topicTag, structTag, tc.markerOld},
				embedding: embedAt(0),
				validFrom: base,
			})
			c2 := h.write(writeOpts{
				concept:   "gizmoline layout " + tc.markerNew,
				content:   fmt.Sprintf("gizmoline layout %s beta-uniq-token-dolor sit amet vroomfondel plonktastic snarklewax", tc.markerNew),
				tags:      []string{topicTag, structTag, tc.markerNew},
				embedding: embedAt(6),
				validFrom: base.Add(45 * 24 * time.Hour),
			})

			c1Eng, _ := h.eng.store.GetEngram(h.ctx, h.ws, mustParseULIDCur(t, c1))
			c2Eng, _ := h.eng.store.GetEngram(h.ctx, h.ws, mustParseULIDCur(t, c2))
			n := h.vaultCount()
			dfCache := map[string]int64{}

			// Confirm every OTHER gate would admit this pair, isolating the
			// marker vocabulary as the sole reason for silence.
			if !h.eng.currencyTemporallySeparated(c1Eng, c2Eng) {
				t.Fatalf("expected temporal separation")
			}
			if !h.eng.currencyPassesSimilarityGate(h.ctx, h.ws, c1Eng, c2Eng) {
				t.Fatalf("expected the similarity floor to admit this genuinely-related pair")
			}
			cut := currencyUbiquityCutpoint(h.vaultCount())
			gotAnchor := h.eng.currencyTagAnchor(h.ctx, h.ws, c1Eng, c2Eng, cut, dfCache)
			if gotAnchor {
				t.Fatalf("test premise broken: tag anchor unexpectedly fired for alien markers %q/%q — investigate before trusting the DOCUMENTED-GAP verdict below", tc.markerOld, tc.markerNew)
			}
			// Also confirm the KEPT RelRefines/entity anchor paths are not
			// the ones silently doing the work here — this test isolates
			// the tag/marker-vocabulary mechanism specifically.
			if h.eng.currencySharedAnchor(h.ctx, h.ws, c1Eng, c2Eng, n, cut, dfCache) {
				t.Fatalf("test premise broken: currencySharedAnchor fired via a non-tag path (RelRefines/entity) for %q/%q — this test's content must stay below the novelty Jaccard threshold so it isolates the marker-vocabulary gap", tc.markerOld, tc.markerNew)
			}

			results := h.scored(c1, 0.9, c2, 0.9)
			out := h.apply(results)
			r1, r2 := findCur(out, c1), findCur(out, c2)
			if r1.VersionCluster != "" || r2.VersionCluster != "" {
				t.Fatalf("BUG: expected silence (unrecognized marker vocabulary), but got a cluster: %+v / %+v", r1, r2)
			}
			t.Logf("DOCUMENTED-GAP (%s): genuine chain using %q/%q markers silences — currencyIsVersionMarker's fixed vocabulary does not recognize these tokens", tc.name, tc.markerOld, tc.markerNew)
		})
	}
}

// --- 3. HIGH-CHURN / SMALL vault: the fixed 10% ubiquity line on a 15-row --
//        vault behaves very differently than on the ~114-row tuning vault.

// TestCrossVault_SmallVault_FixedUbiquityRatio_FalseSilence builds a 15-
// engram vault (no currencyTestHarness.pad — the tuning vault's own test
// helper explicitly pads to ~114 rows specifically so a 2-occurrence tag
// doesn't look "ubiquitous"; this test deliberately withholds that padding
// to show what happens without it). A genuine 4-member version chain shares
// its topic+structure tags with ONLY the 4 chain members (df=4); the other
// 11 engrams in the vault carry unrelated tags. On a 15-row vault, df=4 is
// 26.7% of the vault — comfortably over the FIXED 10% line — so
// currencyIsUbiquitous marks BOTH shared tags ubiquitous and strips them,
// even though, structurally, these are exactly the kind of genuine,
// non-ambient topic tags the ubiquity guard is supposed to let through (the
// tuning vault's real topic tag was ~3% of ~114 rows; here the identical
// STRUCTURE — "shared by exactly the chain" — lands at 26.7% purely because
// N is small). Verdict: DOCUMENTED-GAP — a real chain, with real differing
// version markers, silently fails to cluster on a small vault, purely
// because currencyUbiquityRatio is a fixed fraction rather than derived from
// the vault's own tag-df distribution.
// TestCrossVault_SmallVault_SelfDerivedUbiquity_ChainClusters proves the #11
// self-derive fix. A genuine 4-member chain on a ~15-row vault shares topic+struct
// tags at df=4 (25% of the vault) — which the OLD fixed 10% ratio wrongly called
// "ubiquitous", false-silencing the chain. The self-derived cutpoint instead
// reads the vault's own tag-df distribution ([misc=11, topic=4, struct=4,
// markers=1...]) and finds NO ambient-tag break, so the df=4 chain tags survive
// and the chain CLUSTERS. RED without the self-derive wiring (chain silences),
// GREEN with it. Hermetic: the cutpoint reads the tag index (synchronous), not
// the async GetVaultCount.
func TestCrossVault_SmallVault_SelfDerivedUbiquity_ChainClusters(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	const topicTag, structTag = "microgadget", "core"

	fillerWords := []string{"lorem quux zephyr", "ipsum blorp wibble", "dolor vroom plonk", "sit snarkle fenwick"}
	var chainIDs []string
	markers := []string{"v1", "v2", "v3", "final"}
	for i, m := range markers {
		id := h.write(writeOpts{
			concept:   fmt.Sprintf("microgadget core %s", m),
			content:   fmt.Sprintf("microgadget core %s revision describes the rollout stage %d %s", m, i, fillerWords[i]),
			tags:      []string{topicTag, structTag, m},
			embedding: embedAt(float64(i * 4)),
			validFrom: base.Add(time.Duration(i) * 30 * 24 * time.Hour),
		})
		chainIDs = append(chainIDs, id)
	}
	// 11 unrelated engrams -> a ~15-row vault where topic df=4 is 25% (well over
	// the old fixed 10% line that false-silenced this exact chain).
	for i := 0; i < 11; i++ {
		h.write(writeOpts{
			concept:   fmt.Sprintf("misc note %d", i),
			content:   fmt.Sprintf("misc note %d about an unrelated small-vault topic entryTok%d", i, i*911%997),
			tags:      []string{"misc"},
			embedding: embedAt(float64(180 + i)),
			validFrom: base.Add(time.Duration(i) * 24 * time.Hour),
		})
	}

	// On this ~15-row vault the absolute floor dominates the ubiquity cutpoint
	// (max(10%*N, currencyUbiquityAbsMin) = the floor), so the df=4 chain tags are
	// kept — the chain-clustering assertions below are what pin the fix.
	if cut := currencyUbiquityCutpoint(h.vaultCount()); cut != currencyUbiquityAbsMin {
		t.Logf("ubiquity cutpoint on this small vault = %d (absolute floor expected)", cut)
	}

	out := h.apply(h.scored(chainIDs[0], 0.9, chainIDs[1], 0.9, chainIDs[2], 0.9, chainIDs[3], 0.9))

	// All four members must land in ONE cluster with exactly one crowned newest —
	// the newest (latest validFrom) is chainIDs[3].
	var key string
	crowned := 0
	for i, id := range chainIDs {
		r := findCur(out, id)
		if r == nil || r.VersionCluster == "" {
			t.Fatalf("chain member %d did not cluster — the small-vault false-silence is not fixed", i)
		}
		if i == 0 {
			key = r.VersionCluster
		} else if r.VersionCluster != key {
			t.Fatalf("chain members landed in different clusters (%q vs %q)", key, r.VersionCluster)
		}
		if r.NewestOfCluster {
			crowned++
		}
	}
	if crowned != 1 {
		t.Fatalf("expected exactly one crowned newest_of_cluster, got %d", crowned)
	}
	if r4 := findCur(out, chainIDs[3]); r4 == nil || !r4.NewestOfCluster {
		t.Fatalf("expected the newest chain member (chainIDs[3]) to be crowned newest_of_cluster")
	}
}

// TestCrossVault_SmallVault_IncidentalTagCorrectlyNonUbiquitous is the
// control half of the above: on the SAME small (15-row) vault, a tag that is
// genuinely incidental (appears on only 1 of the 15 engrams) is correctly
// NOT flagged ubiquitous, confirming the false silence above is specifically
// a small-N/fixed-ratio interaction for the "shared by the whole chain"
// case, not a blanket small-vault failure of the ubiquity gate itself.
func TestCrossVault_SmallVault_IncidentalTagCorrectlyNonUbiquitous(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	base := time.Now()
	for i := 0; i < 14; i++ {
		h.write(writeOpts{
			concept:   fmt.Sprintf("filler%d", i),
			content:   fmt.Sprintf("filler content entry unique token%d", i*719%991),
			tags:      []string{"misc"},
			embedding: embedAt(float64(i * 25)),
			validFrom: base.Add(time.Duration(i) * time.Hour),
		})
	}
	h.write(writeOpts{
		concept:   "one-off note",
		content:   "one off note carrying a single incidental tag",
		tags:      []string{"incidental-oneoff"},
		embedding: embedAt(200),
		validFrom: base.Add(15 * time.Hour),
	})

	// n is read dynamically — see the comment in the sibling test above on
	// why an exact literal isn't asserted (async counterCoalescer).
	n := h.vaultCount()
	dfCache := map[string]int64{}
	df := h.eng.currencyTagDF(h.ctx, h.ws, "incidental-oneoff", dfCache)
	if df != 1 {
		t.Fatalf("test premise: expected df=1, got %d", df)
	}
	ratio := float64(df) / float64(n)
	if currencyIsUbiquitous(df, n) {
		t.Fatalf("BUG: an incidental tag at df=1/%d=%.1f%% (below the 10%% line) was wrongly flagged ubiquitous", n, ratio*100)
	}
	t.Logf("PASS: an incidental (df=1) tag on a ~%d-row vault (%.1f%%) is correctly left non-ubiquitous — the fixed ratio's failure mode above is specific to tags shared by an entire small chain, not small vaults in general", n, ratio*100)
}

// --- 4. DENSE-FACET vault: many facets at df just below vs. just above ----
//        currencyFacetDF=3 — boundary behavior of the facet-conflict veto.

// TestCrossVault_FacetDFBoundary is table-driven over facet tags at df=1,
// df=2 (both just below currencyFacetDF=3), and df=3, df=4 (at/above). Each
// case attaches an EXCLUSIVE (present on only one side) non-marker tag at
// the given df between an otherwise-clean, tag-anchored version pair, and
// checks currencyFacetConflict directly.
func TestCrossVault_FacetDFBoundary(t *testing.T) {
	cases := []struct {
		df       int
		wantVeto bool
		verdict  string
	}{
		{1, false, "PASS: df=1 incidental descriptor does not block (below currencyFacetDF)"},
		{2, false, "BUG-CANDIDATE-BUT-SAFE-DIRECTION: df=2 exclusive tag LEAKS THROUGH the facet-conflict veto (below currencyFacetDF=3) — if this were a genuine coexisting facet rather than an incidental descriptor, it would falsely cluster; the design's own doc comment concedes df 1-2 must not block, so this is the intended boundary, not a bug in the current design — but it is a real, demonstrated gap: a real facet mentioned only twice in the vault is indistinguishable from an incidental descriptor mentioned twice"},
		{3, true, "PASS: df=3 exclusive tag correctly blocks (at currencyFacetDF floor)"},
		{4, true, "PASS: df=4 exclusive tag correctly blocks (above currencyFacetDF floor)"},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("df=%d", tc.df), func(t *testing.T) {
			h, cleanup := newCurrencyHarness(t)
			defer cleanup()

			h.pad(110, "filler")
			base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
			const topicTag, structTag = "beamline", "assembly"
			facetTag := fmt.Sprintf("facetword-df%d", tc.df)

			c1 := h.write(writeOpts{
				concept:   "beamline assembly v1",
				content:   "beamline assembly v1 describes the initial configuration",
				tags:      []string{topicTag, structTag, "v1"},
				embedding: embedAt(0),
				validFrom: base,
			})
			// c2 is the side carrying the exclusive facet tag.
			c2Tags := []string{topicTag, structTag, "v2", "final", facetTag}
			c2 := h.write(writeOpts{
				concept:   "beamline assembly v2 final",
				content:   "beamline assembly v2 final describes the completed configuration",
				tags:      c2Tags,
				embedding: embedAt(6),
				validFrom: base.Add(30 * 24 * time.Hour),
			})
			// Additional engrams carrying ONLY the facet tag (not the chain's
			// topic/structure tags) to reach the target df without adding
			// more chain members.
			for i := 0; i < tc.df-1; i++ {
				h.write(writeOpts{
					concept:   fmt.Sprintf("%s filler %d", facetTag, i),
					content:   fmt.Sprintf("unrelated content mentioning %s filler entry %d", facetTag, i),
					tags:      []string{facetTag, "other-context"},
					embedding: embedAt(float64(100 + i)),
					validFrom: base.Add(time.Duration(10+i) * 24 * time.Hour),
				})
			}

			c1Eng, _ := h.eng.store.GetEngram(h.ctx, h.ws, mustParseULIDCur(t, c1))
			c2Eng, _ := h.eng.store.GetEngram(h.ctx, h.ws, mustParseULIDCur(t, c2))
			dfCache := map[string]int64{}

			gotDF := h.eng.currencyTagDF(h.ctx, h.ws, facetTag, dfCache)
			if int(gotDF) != tc.df {
				t.Fatalf("test premise: expected facet tag df=%d, got %d", tc.df, gotDF)
			}

			gotVeto := h.eng.currencyFacetConflict(h.ctx, h.ws,
				currencyFilterTypeLabels(c1Eng.Tags), currencyFilterTypeLabels(c2Eng.Tags), dfCache)
			if gotVeto != tc.wantVeto {
				t.Fatalf("df=%d: currencyFacetConflict = %v, want %v", tc.df, gotVeto, tc.wantVeto)
			}

			// Confirm end-to-end: when the veto does NOT fire, the pair
			// clusters (proving df<currencyFacetDF genuinely leaks through
			// the full pipeline, not just the unit-level gate).
			results := h.scored(c1, 0.9, c2, 0.9)
			out := h.apply(results)
			r1, r2 := findCur(out, c1), findCur(out, c2)
			clustered := r1.VersionCluster != "" && r1.VersionCluster == r2.VersionCluster
			if tc.wantVeto && clustered {
				t.Fatalf("df=%d: expected facet veto to block clustering end-to-end, but pair clustered", tc.df)
			}
			if !tc.wantVeto && !clustered {
				t.Fatalf("df=%d: expected the pair to cluster end-to-end (no veto), but it did not", tc.df)
			}
			t.Log(tc.verdict)
		})
	}
}
