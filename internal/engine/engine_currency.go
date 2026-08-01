package engine

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/novelty"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
)

// #712 / #716 / COG-25 — recall-time version-cluster ADVISORY annotation.
//
// This is a HIGH-PRECISION HINT, never an authority: no mechanical signal can
// prove "B replaces A" vs "B supplements A" (that is a judgment about the
// world), so this phase only ever ANNOTATES (possibly_superseded_by /
// version_cluster / newest_of_cluster) — it never changes a Score, never
// removes or demotes a result, never writes, and never fires on a pair that
// already carries an explicit RelSupersedes edge (the asserted signal always
// wins over the inferred one). The only ordering change is a reorder of
// EXACT score ties within a single detected cluster, newest-
// EffectiveValidFrom-first — global ordering, and all other tiebreaks, are
// untouched.
//
// REVISION 2 (this file): the v1 gate (entity-or-RelRefines anchor + a
// cosine >= 0.83 discriminator) was measured against a real sample
// vault (#716) and failed to fire: the widgetflow version chain carries ZERO
// extracted entities (the vault is entity-sparse generally) and no
// RelRefines edges (the novelty cache was cold), so the anchor gate was
// permanently false; and offline cosines over the real content showed
// version-chain pairs (0.75-0.92) OVERLAPPING coexisting-variant pairs
// (0.68-0.86) by 0.11 — cosine cannot discriminate this data (worst case:
// "final v3 structure" <-> "telemetry fee tiered v2" = 0.86, a false
// pair). Tags carry the real signal on this vault (and, per its measured
// structure, on tag-organized vaults generally): a TAG ANCHOR (shared
// non-ubiquitous topic tags + differing version-marker tags + no
// facet-conflict tag) replaces cosine as the discriminator; cosine survives
// only as a loose sanity floor (see currencySimThreshold, demoted from 0.83
// to 0.70). Entity/RelRefines anchors are KEPT as additional-when-present
// (harmless on entity-sparse vaults — they just rarely fire).
const (
	// currencySimThreshold is now a loose SANITY FLOOR, not the
	// discriminator (R2): measured real-vault cosines put every true
	// version-chain pair at >= 0.75 but also put at least one false
	// (coexisting-variant) pair at 0.86 — no cosine cutoff separates the two
	// populations. 0.70 (chain floor 0.75, 0.05 margin) vetoes only
	// tag-coincidences over genuinely unrelated content; discrimination is
	// the tag anchor's job (currencyTagAnchor).
	currencySimThreshold = 0.70

	// currencyJaccardThreshold is the fallback, veto-only bar used ONLY when
	// an embedding is missing/meaningless for either candidate. It is its
	// OWN (lower, R2-recalibrated) bar, not currencySimThreshold or the
	// write-time novelty near-dup bar (novelty.Threshold): Jaccard and
	// cosine are different metrics on different scales, and this floor no
	// longer needs to be a near-dup bar now that tags do the discriminating.
	currencyJaccardThreshold = 0.50

	// currencyTemporalFloor is the minimum EffectiveValidFrom separation for
	// two results to be treated as different generations rather than a
	// same-breath batch write.
	currencyTemporalFloor = time.Hour

	// currencyMargin bounds how many top (already score-sorted) survivors are
	// examined — mirrors supersessionMargin's rationale: results below
	// MaxResults+margin cannot survive truncation, so pairwise-clustering them
	// is wasted work on the hot recall path.
	currencyMargin = supersessionMargin

	// currencyAssocScan bounds the association scan used to look for an
	// explicit RelSupersedes/RelRefines edge between two specific candidates.
	currencyAssocScan = supersessionReverseScan

	// currencyTagShareMin is the minimum number of shared tags (after
	// excluding type-label and ubiquitous tags) required for the tag anchor.
	// Measured: shared topic tags alone (e.g. two shared topic tags) appear
	// on 12/14 real chain-and-variant rows, so this is necessary but NOT
	// sufficient — the version-marker and facet-conflict gates below do the
	// real discrimination.
	currencyTagShareMin = 2

	// currencyUbiquityRatio: a tag whose vault-local document frequency
	// exceeds this fraction of the vault is "ubiquitous" and does not count
	// toward currencyTagShareMin (a shared ubiquitous tag anchors nothing —
	// the tag-anchor's version of entityIDF's "df >= n -> idf 0" guard).
	// Measured on a real vault: kills an ambient/bookmark tag (~27% of the
	// vault), keeps a genuine topic tag (~3%).
	currencyUbiquityRatio = 0.10

	// currencyFacetDF is the vault-df floor at which an exclusive
	// (present-on-one-side-only) non-marker tag signals a COEXISTING FACET
	// rather than an incidental descriptor, and blocks clustering. Measured
	// basis: genuine facet nouns (measured df 5-7 on a real vault) must block;
	// incidental chain descriptors (df 1-2) must not.
	currencyFacetDF = 3

	// currencyTagDFScanCap bounds the per-tag document-frequency lookup
	// (a key-only Pebble range scan over the existing 0x0C tag index — no
	// new keyspace). Far above any plausible real tag's df; exists only so a
	// single pathological tag can't turn an O(examined-tags) pass unbounded.
	currencyTagDFScanCap = 1_000_000
)

// currencyTypeLabelTags are tags MuninnDB itself auto-applies as a
// type/kind label rather than a content descriptor (muninn_decide stamps
// "decision" — engine.go Decide; the prospective-notice path stamps
// "prospective" — prospective.go). They never count toward the tag anchor's
// shared-tag requirement: sharing "decision" says nothing about topic.
// "insight" is included defensively (a natural label for TypeObservation)
// even though no producer currently auto-stamps it, per the design's named
// example set.
var currencyTypeLabelTags = map[string]struct{}{
	"decision":    {},
	"prospective": {},
	"insight":     {},
}

// currencyVersionMarkerPatterns/currencyVersionStatusMarkers are the
// mechanical, constants-only version-marker vocabulary (no LLM). Honestly
// brittle: hand-picked from ONE measured vault (widgetflow facts). See the
// package doc comment on currencyIsVersionMarker for how this generalizes.
var (
	// currencyVersionMarkerRe matches "v2", "v3", "v1.1", "v2-3", etc.
	currencyVersionMarkerRe = regexp.MustCompile(`^v\d+([.-]\d+)*$`)
	// currencyCountedStructureRe matches SPELLED-OUT counted noun-phrases
	// ("four-zone", "three-zone") that describe a structure shape. R3: a bare
	// numeric prefix (\d+) is deliberately NOT matched — digit-prefixed tags
	// collide with real FACET/ID tags (form numbers, year cohorts, ticket refs
	// like "42-alpha"/"2024-cohort"). Treating those as version markers exempted
	// them from the facet-conflict veto (the gate R2.3 proved load-bearing),
	// causing false clusters. Cost: a digit-form structure tag like "5-tier" is
	// no longer recognized; the spelled-out "five-tier" still is. Precision over
	// recall here — a false cluster annotates a live fact as possibly-stale.
	currencyCountedStructureRe = regexp.MustCompile(`^(one|two|three|four|five|six|seven|eight|nine|ten)-[a-z][a-z-]*$`)
)

// currencyVersionStatusMarkers is the status-word half of the marker
// vocabulary — words that describe a fact's place in a lifecycle rather than
// its topic.
var currencyVersionStatusMarkers = map[string]struct{}{
	"final": {}, "live": {}, "current": {}, "future": {}, "planned": {},
	"future-state": {}, "draft": {}, "proposed": {}, "interim": {},
	"deprecated": {}, "superseded": {}, "revised": {}, "revision": {},
	"refinement": {}, "legacy": {}, "old": {}, "original": {},
}

// applyCurrencyAnnotation is the recall-time version-cluster ADVISORY phase.
// It examines the top min(len(results), maxResults+currencyMargin) survivors
// (results is assumed score-sorted descending, matching every caller in this
// package), pairwise-clusters them via union-find under the conjunctive gate
// (temporal-separation AND similarity AND version-marker vocabulary on BOTH
// sides — currencyVersionMarkerGate, universal since R4 — AND shared-anchor,
// MINUS facet conflicts and MINUS any member of a DECLARED supersession chain
// in either direction, currencyInDeclaredChain), and:
//   - crowns the max-EffectiveValidFrom, non-future member of each 2+ cluster
//     newest_of_cluster,
//   - annotates every other member possibly_superseded_by <- the crowned ID,
//   - re-sorts ONLY exact score ties within a detected cluster, newest-first.
//
// Zero writes. Zero score changes. A future-ValidFrom member is never
// crowned (a planned/aspirational fact is not "true now"). Pure read path;
// observe-safe. vaultSize feeds the same entityIDF rarity calc the entity
// boost phase uses (COG-18) so "shared rare entity" means the same thing
// here as it does there.
func (e *Engine) applyCurrencyAnnotation(ctx context.Context, ws [8]byte, results []activation.ScoredEngram, vaultSize int64, maxResults int) []activation.ScoredEngram {
	if len(results) < 2 {
		return results
	}

	examined := len(results)
	if maxResults > 0 && examined > maxResults+currencyMargin {
		examined = maxResults + currencyMargin
	}

	// Per-call tag document-frequency cache: at most one 0x0C range scan per
	// DISTINCT tag across the whole examined set (bounded — "no new
	// keyspace", per design R2.2), never per-pair.
	dfCache := make(map[string]int64, examined*4)

	// Per-call declared-chain memo (R4): whether an engram participates in ANY
	// explicit RelSupersedes edge, in EITHER direction. Computed lazily — only
	// for engrams that have already cleared every cheap gate — and at most once
	// per engram, never once per pair.
	chainMemo := make(map[storage.ULID]bool, examined)
	inDeclaredChain := func(id storage.ULID) bool {
		if v, ok := chainMemo[id]; ok {
			return v
		}
		v := e.currencyInDeclaredChain(ctx, ws, id)
		chainMemo[id] = v
		return v
	}

	// Self-derived per-vault ubiquity cutpoint (#11), computed once per pass and
	// cached across calls with a TTL. Replaces the fixed 10%-of-N ratio, which
	// false-silenced genuine chains on small vaults.
	ubiquityCutpoint := currencyUbiquityCutpoint(vaultSize)

	// Union-find over the examined prefix.
	parent := make([]int, examined)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	for i := 0; i < examined; i++ {
		for j := i + 1; j < examined; j++ {
			if ctx.Err() != nil {
				break
			}
			a, b := results[i].Engram, results[j].Engram

			// Cheapest gate first: temporal separation (pure in-memory).
			if !e.currencyTemporallySeparated(a, b) {
				continue
			}
			// Similarity: cached point reads / in-memory fingerprints. Each
			// signal source is compared against ITS OWN calibrated bar.
			if !e.currencyPassesSimilarityGate(ctx, ws, a, b) {
				continue
			}
			// Version markers: a UNIVERSAL gate (R4), independent of which
			// anchor fires below. Before R4 the marker requirement lived
			// INSIDE currencyTagAnchor, so the entity and RelRefines anchors —
			// which return early from currencySharedAnchor — bypassed it
			// entirely: any two temporally-separated, loosely-similar memories
			// sharing one rare entity (in a project vault, the project name)
			// were clustered as generations of one fact with no version
			// vocabulary anywhere. That is the reproduced "unrelated
			// project-identity memory called a newer version of the storage
			// decision" false advisory. See
			// TestCurrencyPrecision_EntityAnchorSideDoor_UnrelatedTopics /
			// _RelRefinesSideDoor_NoVersionVocabulary.
			if !currencyVersionMarkerGate(a.Tags, b.Tags) {
				continue
			}
			// Shared anchor: entity scan / association scan / tag anchor.
			if !e.currencySharedAnchor(ctx, ws, a, b, vaultSize, ubiquityCutpoint, dfCache) {
				continue
			}
			// Facet conflict: a UNIVERSAL veto, independent of which anchor
			// fired above. Measured (#716): this is the ONLY gate that blocks
			// a real coexisting-facet false pair the similarity floor admits
			// (e.g. cosine 0.86 "telemetry fee" vs. the widgetflow chain).
			if e.currencyFacetConflict(ctx, ws, currencyFilterTypeLabels(a.Tags), currencyFilterTypeLabels(b.Tags), dfCache) {
				continue
			}
			// Asserted supersession always wins — never second-guess it with
			// the heuristic, even though every other gate cleared. R4 widens
			// this from "this PAIR carries an edge" to "EITHER SIDE is covered
			// by a declared chain at all, in either direction". The old,
			// narrower rule protected only the SUPERSEDED side: the declared
			// SUCCESSOR (the head of an evolve chain) carries no incoming
			// stamp, so it looked like an ordinary un-versioned engram and the
			// heuristic was free to annotate it possibly_superseded_by an
			// arbitrary newer, unverified memory — advising the exact opposite
			// of what the user explicitly asserted. When a human or an agent
			// has declared how a fact versions, the heuristic has nothing to
			// add and everything to get wrong; it stays out. See
			// TestCurrencyPrecision_DeclaredSuccessor_NotAdvisedSuperseded.
			if inDeclaredChain(a.ID) || inDeclaredChain(b.ID) {
				continue
			}
			union(i, j)
		}
	}

	groups := make(map[int][]int, examined)
	for i := 0; i < examined; i++ {
		r := find(i)
		groups[r] = append(groups[r], i)
	}

	now := time.Now()
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		// Deterministic cluster key: the smallest ULID among members.
		key := results[members[0]].Engram.ID
		for _, m := range members[1:] {
			if bytes.Compare(results[m].Engram.ID[:], key[:]) < 0 {
				key = results[m].Engram.ID
			}
		}
		keyStr := key.String()
		for _, m := range members {
			results[m].VersionCluster = keyStr
			results[m].ClusterSize = len(members)
		}

		// Newest = max EffectiveValidFrom among members whose EffectiveValidFrom
		// is not in the future. A future-dated member is never crowned.
		newestIdx := -1
		for _, m := range members {
			evf := results[m].Engram.EffectiveValidFrom()
			if evf.After(now) {
				continue
			}
			if newestIdx == -1 || evf.After(results[newestIdx].Engram.EffectiveValidFrom()) {
				newestIdx = m
			}
		}
		if newestIdx == -1 {
			// Every member is future-dated: annotate the cluster grouping but
			// crown nothing — degrade to silence on the "who's newest"
			// question rather than guess.
			continue
		}
		results[newestIdx].NewestOfCluster = true
		newestID := results[newestIdx].Engram.ID
		for _, m := range members {
			if m == newestIdx {
				continue
			}
			// COG-25: the asserted signal always wins and is NEVER duplicated or
			// second-guessed by the heuristic. Blocking the direct pair from
			// union-find above is not sufficient — union-find can rejoin an
			// explicitly-superseded member into the cluster through OTHER pairs,
			// so suppression MUST be re-checked here against the crown. Two guards:
			//  (a) the member already carries the authoritative SupersededBy/
			//      CurrentVersion (applySupersession ran before this phase) — the
			//      asserted path fully covers it; and
			//  (b) an explicit RelSupersedes edge exists between the member and the
			//      crown (covers the unit path where (a) was not pre-populated).
			if (results[m].SupersededBy != storage.ULID{}) || (results[m].CurrentVersion != storage.ULID{}) {
				continue
			}
			if e.currencyHasExplicitSupersedesEdge(ctx, ws, results[m].Engram.ID, newestID) {
				continue
			}
			results[m].PossiblySupersededBy = newestID
		}
	}

	// Base ordering: a TOTAL order (Score desc, ULID asc) — transitive and
	// run-to-run deterministic (#699). The newest-first reorder is NOT folded in
	// here: mixing EVF (within-cluster) and ULID (cross-cluster) in one comparator
	// is intransitive (X<Y by EVF, Y<Z by ULID, Z<X by ULID cycles), which makes
	// sort.SliceStable order-dependent. Keeping the base a pure total order and
	// doing the reorder as a localized post-pass restores both properties.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		a, b := results[i].Engram.ID, results[j].Engram.ID
		return bytes.Compare(a[:], b[:]) < 0
	})

	// Localized in-cluster tie reorder: within each maximal run of EQUAL score,
	// reorder ONLY each detected cluster's members among the slots they already
	// occupy, newest-EffectiveValidFrom-first. Non-members never move and the set
	// of results in the run is unchanged, so truncation membership is untouched.
	// This is the sole ordering change COG-25 permits.
	for start := 0; start < len(results); {
		end := start + 1
		for end < len(results) && results[end].Score == results[start].Score {
			end++
		}
		currencyReorderTieRun(results[start:end])
		start = end
	}
	return results
}

// currencyReorderTieRun reorders, within one equal-score run, each detected
// cluster's members among the slots they occupy — newest-EffectiveValidFrom-first,
// ULID-ascending as the final tiebreak — leaving every non-member and every other
// cluster's slots untouched. Clusters occupy disjoint slot sets, so the outcome is
// independent of the (map) iteration order: deterministic and membership-preserving.
func currencyReorderTieRun(run []activation.ScoredEngram) {
	byCluster := map[string][]int{}
	for i := range run {
		if run[i].VersionCluster != "" {
			byCluster[run[i].VersionCluster] = append(byCluster[run[i].VersionCluster], i)
		}
	}
	for _, idxs := range byCluster {
		if len(idxs) < 2 {
			continue
		}
		members := make([]activation.ScoredEngram, len(idxs))
		for k, idx := range idxs {
			members[k] = run[idx]
		}
		sort.SliceStable(members, func(a, b int) bool {
			ta, tb := members[a].Engram.EffectiveValidFrom(), members[b].Engram.EffectiveValidFrom()
			if !ta.Equal(tb) {
				return ta.After(tb)
			}
			return bytes.Compare(members[a].Engram.ID[:], members[b].Engram.ID[:]) < 0
		})
		for k, idx := range idxs {
			run[idx] = members[k]
		}
	}
}

// currencyTemporallySeparated reports whether a and b's EffectiveValidFrom
// differ by more than currencyTemporalFloor — two facts written in the same
// breath are a batch, not different generations of the same fact.
func (e *Engine) currencyTemporallySeparated(a, b *storage.Engram) bool {
	diff := a.EffectiveValidFrom().Sub(b.EffectiveValidFrom())
	if diff < 0 {
		diff = -diff
	}
	return diff > currencyTemporalFloor
}

// currencySimilarity returns the raw similarity between a and b, which
// signal produced it, and whether a signal was available at all. It prefers
// stored embedding cosine similarity (0x18 point reads); when either vector
// is absent OR has zero norm (no real embedding signal — e.g. a BM25-only
// vault, or a vault whose embedder never ran, as with this engine's
// noopEmbedder in tests), it falls back to Jaccard over term fingerprints.
// No signal available from either path → (0, false, false): degrade to
// silence, never guess.
func (e *Engine) currencySimilarity(ctx context.Context, ws [8]byte, a, b *storage.Engram) (sim float64, fromEmbedding bool, ok bool) {
	embA, errA := e.store.GetEmbedding(ctx, ws, a.ID)
	embB, errB := e.store.GetEmbedding(ctx, ws, b.ID)
	if errA == nil && errB == nil {
		if c, cok := cosineIfMeaningful(embA, embB); cok {
			return c, true, true
		}
	}
	fpA := novelty.BuildFingerprint(a.Concept, a.Content)
	fpB := novelty.BuildFingerprint(b.Concept, b.Content)
	j := novelty.Jaccard(fpA, fpB)
	if j == 0 {
		return 0, false, false
	}
	return j, false, true
}

// currencyPassesSimilarityGate reports whether a and b clear the similarity
// gate, comparing each signal source against ITS OWN calibrated bar:
// currencySimThreshold for embedding cosine, currencyJaccardThreshold (a
// different, lower-in-practice bar) for the token-fingerprint fallback.
func (e *Engine) currencyPassesSimilarityGate(ctx context.Context, ws [8]byte, a, b *storage.Engram) bool {
	sim, fromEmbedding, ok := e.currencySimilarity(ctx, ws, a, b)
	if !ok {
		return false
	}
	if fromEmbedding {
		return sim >= currencySimThreshold
	}
	return sim >= currencyJaccardThreshold
}

// cosineIfMeaningful computes cosine similarity, but reports ok=false when
// either vector is empty, mismatched in length, or has zero norm (a stub /
// never-embedded vector, e.g. this engine's noopEmbedder in tests) — that is
// "no real signal", not "similarity 0".
func cosineIfMeaningful(a, b []float32) (float64, bool) {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0, false
	}
	var na, nb float64
	for i := range a {
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0, false
	}
	return float64(hnsw.CosineSimilarity(a, b)), true
}

// currencySharedAnchor reports whether a and b are anchored to the same
// TOPIC via ANY of (R2) the three signals below. It is the "same subject"
// question ONLY — since R4 it is no longer the last word on clustering: the
// universal version-marker gate and the facet-conflict veto are applied by
// applyCurrencyAnnotation on every path, precisely because the first two
// branches here return early.
//
// The three anchors are: an existing RelRefines edge (write-time novelty
// already vouched for their near-duplicate relationship — KEPT from v1); at
// least one shared entity whose store-wide mention count is rare enough to
// carry idf weight (entityIDF/COG-18 — KEPT from v1, but measured (#716) to
// rarely fire: the real vault's widgetflow facts carry zero entities); or the
// R2 TAG ANCHOR (currencyTagAnchor) — the discriminator that actually fires
// on entity-sparse, tag-organized vaults.
func (e *Engine) currencySharedAnchor(ctx context.Context, ws [8]byte, a, b *storage.Engram, vaultSize, ubiquityCutpoint int64, dfCache map[string]int64) bool {
	if e.currencyHasAssocEdge(ctx, ws, a.ID, b.ID, storage.RelRefines) || e.currencyHasAssocEdge(ctx, ws, b.ID, a.ID, storage.RelRefines) {
		return true
	}
	if e.currencyEntityAnchor(ctx, ws, a.ID, b.ID, vaultSize) {
		return true
	}
	return e.currencyTagAnchor(ctx, ws, a, b, ubiquityCutpoint, dfCache)
}

// currencyEntityAnchor is the v1 entity-anchor gate, unchanged: at least one
// shared entity whose store-wide mention count is rare enough to carry idf
// weight (reuses entityIDF/COG-18 — a shared UBIQUITOUS entity anchors
// nothing). Kept as an additional-when-present signal (COG-18 composition);
// harmless on entity-sparse vaults, where it simply never fires (#716).
func (e *Engine) currencyEntityAnchor(ctx context.Context, ws [8]byte, a, b storage.ULID, vaultSize int64) bool {
	entsA := make(map[string]struct{}, 4)
	_ = e.store.ScanEngramEntities(ctx, ws, a, func(name string) error {
		entsA[name] = struct{}{}
		return nil
	})
	if len(entsA) == 0 {
		return false
	}

	found := false
	_ = e.store.ScanEngramEntities(ctx, ws, b, func(name string) error {
		if found {
			return nil
		}
		if _, shared := entsA[name]; !shared {
			return nil
		}
		rec, err := e.store.GetEntityRecord(ctx, name)
		if err != nil || rec == nil {
			return nil
		}
		if entityIDF(int64(rec.MentionCount), vaultSize) > 0 {
			found = true
		}
		return nil
	})
	return found
}

// currencyTagAnchor is the R2 discriminator (#716): measured on the real
// a real-vault clone, cosine similarity alone cannot separate a genuine
// version chain from a coexisting variant (ranges overlap 0.75-0.92 vs
// 0.68-0.86). Tags carry the real signal. a and b anchor as versions of the
// same fact iff ALL of:
//
// same topic iff they share at least currencyTagShareMin tags, after dropping
// type-label tags (currencyTypeLabelTags) and UBIQUITOUS tags (df >= the
// self-derived ubiquity cutpoint) — necessary, not sufficient (measured: shared
// topic tags alone appear on both real chains AND coexisting variants).
//
// R4: the version-marker requirement that used to live HERE is now a UNIVERSAL
// gate applied by the caller (currencyVersionMarkerGate), joining the
// facet-conflict veto. Both are applied regardless of which anchor fired, so
// they protect the entity/RelRefines anchor paths too — those paths return early
// from currencySharedAnchor and could never reach a gate buried in this
// function. This function is now purely the "same topic?" test.
func (e *Engine) currencyTagAnchor(ctx context.Context, ws [8]byte, a, b *storage.Engram, ubiquityCutpoint int64, dfCache map[string]int64) bool {
	tagsA := currencyFilterTypeLabels(a.Tags)
	tagsB := currencyFilterTypeLabels(b.Tags)
	if len(tagsA) == 0 || len(tagsB) == 0 {
		return false
	}

	setB := make(map[string]struct{}, len(tagsB))
	for _, t := range tagsB {
		setB[t] = struct{}{}
	}

	shared := 0
	for _, t := range tagsA {
		if _, ok := setB[t]; !ok {
			continue
		}
		df := e.currencyTagDF(ctx, ws, t, dfCache)
		if df >= ubiquityCutpoint {
			continue // ambient tag (self-derived cutpoint) — anchors nothing
		}
		shared++
	}
	return shared >= currencyTagShareMin
}

// currencyVersionMarkerGate is the UNIVERSAL version-vocabulary gate (R4). Two
// engrams may be treated as generations of one fact only if BOTH carry at least
// one version-marker tag (currencyIsVersionMarker) and their marker SETS DIFFER
// — identical marker sets mean same-version SIBLINGS (e.g. three separate
// "four-zone" facts comparing rates), which must not be ordered against each
// other.
//
// R4 moved this out of currencyTagAnchor and up into applyCurrencyAnnotation,
// alongside the facet-conflict veto, for the same reason the facet veto is
// universal: currencySharedAnchor RETURNS EARLY on the entity and RelRefines
// branches, so anything living inside the tag anchor is a gate those two paths
// simply never reach. That was a side door wide enough to drive the measured
// false advisories through — a shared rare entity (in a project vault, the
// project's own name, which every memory mentions) plus a 0.70 cosine floor was
// the entire evidentiary basis for telling an agent that a live fact might be
// stale. A supersession advisory with no version vocabulary on either side is
// not a weak hint; it is a guess dressed as evidence.
//
// The anchors still differ in what they substitute for: entity/RelRefines
// anchors stand in for the >=2-shared-tag requirement (useful on tag-poor,
// entity-rich vaults), but no anchor stands in for version vocabulary.
func currencyVersionMarkerGate(rawTagsA, rawTagsB []string) bool {
	markersA := currencyMarkerSet(currencyFilterTypeLabels(rawTagsA))
	markersB := currencyMarkerSet(currencyFilterTypeLabels(rawTagsB))
	if len(markersA) == 0 || len(markersB) == 0 {
		return false
	}
	// Identical marker sets => same-version siblings, not an ordered chain.
	return !currencyMarkerSetsEqual(markersA, markersB)
}

// currencyInDeclaredChain reports whether id participates in ANY explicit
// RelSupersedes edge, in either direction — i.e. whether a human or an agent has
// already DECLARED how this fact versions (via muninn_evolve or muninn_link).
//
// When they have, the heuristic must stay silent about that engram entirely:
// not just on the declared pair (the pre-R4 rule), and not just on the
// superseded side. The declared SUCCESSOR is the dangerous case — it carries no
// incoming supersession stamp, so under the pre-R4 rule it was indistinguishable
// from an un-versioned engram, and any newer same-topic memory could be crowned
// over it. Annotating the asserted head of a chain as possibly-superseded by an
// unverified newer claim is the single worst output this channel can produce:
// it contradicts an explicit assertion using a mechanical guess.
func (e *Engine) currencyInDeclaredChain(ctx context.Context, ws [8]byte, id storage.ULID) bool {
	m, err := e.store.GetAssociations(ctx, ws, []storage.ULID{id}, currencyAssocScan)
	if err != nil {
		// Degrade toward SILENCE (principle #2). The declared SUCCESSOR — the
		// engram this whole mechanism exists to protect — carries its
		// RelSupersedes edge in the FORWARD direction only, so swallowing this
		// error and falling through to a healthy-but-empty reverse scan would
		// re-open the exact leak: an asserted chain head advised as
		// possibly-superseded because one read transiently failed.
		slog.Warn("currency: forward-association lookup failed; treating engram as declared-chain-covered to fail toward silence", "err", err)
		return true
	}
	for _, assoc := range m[id] {
		if assoc.RelType == storage.RelSupersedes {
			return true
		}
	}
	revs, err := e.store.GetReverseAssociations(ctx, ws, id, currencyAssocScan)
	if err != nil {
		// Degrade toward SILENCE (principle #2): if we cannot tell whether an
		// asserted chain covers this engram, do not manufacture an advisory
		// that might contradict one.
		slog.Warn("currency: reverse-association lookup failed; treating engram as declared-chain-covered to fail toward silence", "err", err)
		return true
	}
	for _, assoc := range revs {
		if assoc.RelType == storage.RelSupersedes {
			return true
		}
	}
	return false
}

// currencyFilterTypeLabels drops MuninnDB's own type-label tags
// (currencyTypeLabelTags) from tags — a type label is a kind-of-memory
// marker, not a topic descriptor, and must never itself count as a shared
// topic tag or a facet tag.
func currencyFilterTypeLabels(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if _, isType := currencyTypeLabelTags[t]; isType {
			continue
		}
		out = append(out, t)
	}
	return out
}

// currencyIsVersionMarker reports whether tag is a version-marker token:
// a "v<N>" tag (currencyVersionMarkerRe), a counted-structure tag like
// "four-zone"/"three-zone" (currencyCountedStructureRe), or a
// lifecycle-status word (currencyVersionStatusMarkers: final/live/future/…).
//
// HONEST LIMITATION: this vocabulary was hand-derived from ONE measured
// vault (the real version-chain facts) and is a small set of constants, not a
// learned or general model. It will under-fire (silently, the safe
// direction) on any vault whose agents version facts with different words
// ("phase 2" instead of "v2", "sunset" instead of "deprecated", etc.). The
// principled generalization — deferred, not built here — is to stop
// requiring membership in a fixed vocabulary and instead treat ANY
// low-df, non-shared, non-facet tag that differs between two
// already-tag-anchored, temporally-separated, same-topic facts as a
// candidate version marker (i.e. flip the test from "is this word on my
// list" to "is this an exclusive low-df differentiator with no known facet
// meaning") — that generalizes past widgetflow without hand-tuning a new list
// per vault, at the cost of being harder to reason about precisely; it is
// left as a named follow-up because it needs its own measured validation
// pass to avoid quietly turning into the "any 2 shared tags" false-positive
// mode this design explicitly measured and rejected.
func currencyIsVersionMarker(tag string) bool {
	if currencyVersionMarkerRe.MatchString(tag) {
		return true
	}
	if currencyCountedStructureRe.MatchString(tag) {
		return true
	}
	_, ok := currencyVersionStatusMarkers[tag]
	return ok
}

// currencyMarkerSet returns the subset of tags that are version markers, as
// a set (for the differing-set-vs-identical-set comparison).
func currencyMarkerSet(tags []string) map[string]struct{} {
	var out map[string]struct{}
	for _, t := range tags {
		if !currencyIsVersionMarker(t) {
			continue
		}
		if out == nil {
			out = make(map[string]struct{}, 2)
		}
		out[t] = struct{}{}
	}
	return out
}

// currencyMarkerSetsEqual reports whether two marker sets contain exactly
// the same tags (order-independent) — an identical marker set means the two
// facts are same-version SIBLINGS, not different generations.
func currencyMarkerSetsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for t := range a {
		if _, ok := b[t]; !ok {
			return false
		}
	}
	return true
}

// currencyFacetConflict reports whether either side carries an EXCLUSIVE
// (not present on the other side) non-marker tag whose vault-wide document
// frequency is >= currencyFacetDF. This is THE gate that blocks a real
// coexisting facet (e.g. "telemetry" fee facts, df 6) from clustering
// with a same-topic, similarity-admitted version chain — measured to be the
// only signal that can (cosine alone admits pairs like this at 0.86).
func (e *Engine) currencyFacetConflict(ctx context.Context, ws [8]byte, tagsA, tagsB []string, dfCache map[string]int64) bool {
	setA := make(map[string]struct{}, len(tagsA))
	for _, t := range tagsA {
		setA[t] = struct{}{}
	}
	setB := make(map[string]struct{}, len(tagsB))
	for _, t := range tagsB {
		setB[t] = struct{}{}
	}
	check := func(tags []string, other map[string]struct{}) bool {
		for _, t := range tags {
			if _, shared := other[t]; shared {
				continue // shared tags cannot be an "exclusive" facet signal
			}
			if currencyIsVersionMarker(t) {
				continue // markers are handled by the marker-set gate, not this one
			}
			if e.currencyTagDF(ctx, ws, t, dfCache) >= currencyFacetDF {
				return true
			}
		}
		return false
	}
	return check(tagsA, setB) || check(tagsB, setA)
}

// currencyDFRangeStart/currencyDFRangeEnd bound the tag-df scan to "all
// time" without risking a zero time.Time (whose Unix() is negative and
// wraps to a huge value under ulid.Timestamp's uint64 cast). currencyTagDF
// counts ALL engrams carrying tag, reusing the existing 0x0C tag index via
// ListByTagInRange — no new keyspace, no new counter.
var currencyDFRangeStart = time.Unix(0, 0)

func currencyDFRangeEnd() time.Time {
	return time.Now().Add(100 * 365 * 24 * time.Hour)
}

// currencyTagDF returns tag's vault-local document frequency, memoized in
// dfCache for the duration of one applyCurrencyAnnotation call (so a tag
// shared by many examined pairs costs exactly one range scan, not one per
// pair).
func (e *Engine) currencyTagDF(ctx context.Context, ws [8]byte, tag string, dfCache map[string]int64) int64 {
	if df, ok := dfCache[tag]; ok {
		return df
	}
	ids, err := e.store.ListByTagInRange(ctx, ws, tag, currencyDFRangeStart, currencyDFRangeEnd(), currencyTagDFScanCap)
	var df int64
	if err != nil {
		// Degrade LOUDLY and toward SILENCE (principle #2): a df-lookup failure
		// must never manufacture a cluster. A large df makes the tag look ambient
		// (dropped from the shared-tag count) AND facet-eligible (vetoes) — both
		// suppress clustering rather than permit a false annotation.
		slog.Warn("currency: tag df lookup failed; treating tag as ambient to fail toward silence", "err", err)
		df = currencyTagDFScanCap
	} else {
		df = int64(len(ids))
	}
	dfCache[tag] = df
	return df
}

// currencyIsUbiquitous reports whether a tag's document frequency exceeds
// currencyUbiquityRatio of the vault — a shared ubiquitous tag (e.g. a
// vault-wide process label) anchors nothing, mirroring entityIDF's
// "df >= n -> 0" guard for entities.
func currencyIsUbiquitous(df, vaultSize int64) bool {
	return df >= currencyUbiquityCutpoint(vaultSize)
}

// currencyUbiquityAbsMin is the absolute floor for ubiquity: a tag on fewer than
// this many engrams is NEVER "ambient", regardless of vault size. This is the
// #11 fix. The fixed 10%-of-N ratio alone false-silenced genuine chains on small
// vaults (a 4-member chain's topic tag is 25% of a 16-row vault but only 4
// memories). A pure tag-df-distribution derivation was tried and rejected: it
// cannot distinguish a small chain's tag (few memories, large fraction of a tiny
// vault) from a genuinely ambient tag (many memories on ~all engrams) without the
// absolute count, and it over-filters content tags on large vaults. The correct,
// hermetic answer is the fraction cut floored by an absolute minimum.
const currencyUbiquityAbsMin = 10

// currencyUbiquityCutpoint returns the df at/above which a tag is "ambient"
// (anchors nothing) for a vault of vaultSize: the LARGER of the fraction cut
// (currencyUbiquityRatio*N) and currencyUbiquityAbsMin. On small vaults the
// absolute floor dominates, so the cutpoint is insensitive to the exact,
// async-coalesced N; on large vaults the fraction dominates, so a genuine ambient
// tag (~27% of a 1357-row vault) is filtered while a content tag (~3%) is kept.
func currencyUbiquityCutpoint(vaultSize int64) int64 {
	c := int64(currencyUbiquityRatio * float64(vaultSize))
	if c < currencyUbiquityAbsMin {
		return currencyUbiquityAbsMin
	}
	return c
}

// currencyHasAssocEdge reports whether a forward association of relType
// exists from -> to.
func (e *Engine) currencyHasAssocEdge(ctx context.Context, ws [8]byte, from, to storage.ULID, relType storage.RelType) bool {
	m, err := e.store.GetAssociations(ctx, ws, []storage.ULID{from}, currencyAssocScan)
	if err != nil {
		return false
	}
	for _, assoc := range m[from] {
		if assoc.RelType == relType && assoc.TargetID == to {
			return true
		}
	}
	return false
}

// currencyHasExplicitSupersedesEdge reports whether an explicit RelSupersedes
// edge exists between a and b in either direction. When one does, the
// heuristic must never annotate that pair — the asserted signal always wins.
func (e *Engine) currencyHasExplicitSupersedesEdge(ctx context.Context, ws [8]byte, a, b storage.ULID) bool {
	return e.currencyHasAssocEdge(ctx, ws, a, b, storage.RelSupersedes) ||
		e.currencyHasAssocEdge(ctx, ws, b, a, storage.RelSupersedes)
}
