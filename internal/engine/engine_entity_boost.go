package engine

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

const (
	// entityBoostFactor is the maximum score contribution of one shared
	// entity at peak rarity. The effective contribution of an entity is
	// entityBoostFactor × idf(entity) — see entityIDF — so ubiquitous
	// entities contribute ~0 and only rare shared entities carry weight
	// (issue #569).
	entityBoostFactor = float64(0.15)

	// entityBoostTopN is the number of top BFS results whose entity links are
	// used as seeds for the spread-activation pass.
	entityBoostTopN = 5

	// entityBoostCap bounds the total entity boost a single engram can
	// accumulate in one recall, no matter how many entities it shares with
	// the seeds. Entity co-mention is associative evidence; the cap keeps it
	// from outranking content evidence (issue #569).
	//
	// The cap deliberately sits at the LOW end of the BFS association weight
	// range (~0.3–0.9): two or more maximally-rare shared entities are allowed
	// to tie the weakest genuine graph edge, never a mid-strength one.
	//
	// CALIBRATION CAVEAT: factor, cap, and noise floor are calibrated to the
	// ACT-R score scale ([0, 1]). Under RRF fusion (scoring_fusion="rrf"),
	// content scores are rank-based and typically land in [0, 0.05]. On the
	// activateCore path the request threshold is defaulted to 0.1 before
	// activation.Run, so under RRF defaults no result clears the seed rule
	// and the boost pass is effectively a no-op. A caller-explicit small
	// threshold (e.g. 0.01) re-enables it, and there an injected engram can
	// enter well above genuine RRF content matches while the threshold gate
	// loses most of its bite. Scaling the boost to the active fusion mode is
	// a scoped follow-up (see PR #570 review); until then RRF callers should
	// treat entity-boosted scores as cross-scale.
	entityBoostCap = float64(0.30)

	// entityBoostNoiseFloor is the smallest per-entity contribution worth
	// pursuing. Entities below it (i.e. near-ubiquitous ones) are skipped
	// before their reverse index is scanned — they cannot meaningfully move
	// a score, and hub entities are exactly the ones with the largest scan
	// fan-out.
	entityBoostNoiseFloor = 0.001
)

// entityIDF returns the inverse-document-frequency weight for an entity:
// ln(n/df) / ln(n), clamped to [0, 1]. n is the RECALLED VAULT's engram count
// (vault-local — the flood #569 guards against is a vault-local phenomenon)
// while df is the entity's store-wide mention count (EntityRecords are deduped
// by name across vaults) — both maintained counters, no scans.
//
// The n-local/df-global split is deliberate and degrades safely: an entity
// unique to the vault gets an exactly-correct idf (the common case), while an
// entity shared across vaults gets UNDER-credited — df can approach or exceed
// the local n, shrinking idf or zeroing it via the df >= n guard. That is a
// false negative (rare shared entity missing some credit), never a re-flood,
// and unlike a global n it cannot grow with unrelated vaults added to the
// deployment. A true vault-scoped df needs per-vault mention counters on
// EntityRecord (a schema change touching every increment/decrement site) and
// is deliberately left to a follow-up.
//
// A rare entity (df ≪ n) approaches 1; an entity mentioned by most engrams
// approaches 0, so sharing it carries no associative evidence.
func entityIDF(df, n int64) float64 {
	if n < 2 || df < 1 || df >= n {
		return 0
	}
	return math.Log(float64(n)/float64(df)) / math.Log(float64(n))
}

// applyEntityBoost performs a post-BFS spread-activation pass using named
// entities. It takes the top-N results from the BFS activation, collects
// every entity linked to those engrams via the 0x20 forward index, then
// finds all other engrams in the same vault that mention those entities via
// the 0x23 reverse index.
//
// Each distinct shared entity contributes entityBoostFactor × idf(entity)
// exactly once per target engram (regardless of how many seeds carry it —
// the evidence is the shared entity, not the seed count), and the total
// boost per engram is capped at entityBoostCap. Engrams already in the
// result set have the boost added to their score; engrams outside it are
// injected only when their accumulated entity evidence alone clears the
// caller's threshold AND they pass the rest of phase 6's result contract:
// meta filters, the ExcludeUntrusted trust filter, lease visibility (#548),
// and the valid-time gate. The pipeline applies all of these in phase 6, so
// anything appended afterwards must honor the same contract — otherwise a
// tags_all query returns records without the required tags, an
// ExcludeUntrusted vault gets flagged-unreliable memory re-injected through
// any shared rare entity, and a work-queue engram checked out by another
// agent leaks back into recall (issue #569). Boosted and injected results
// both carry the boost in Components.EntityBoost so the adjustment is
// auditable — injected results never appear with an empty component trace
// (issue #569).
//
// Results are re-sorted by score descending (activation.ResultBefore). Under
// content-first order (COG-35, the default ACT-R path) the boost does not
// change any row's Score: it is reported in Components.EntityBoost, orders
// injected rows (Score 0, after every scored row) and breaks exact ties.
//
// vaultSize is the recalled vault's engram count (activateCore already holds
// it); it feeds entityIDF's n so rarity is judged against the vault being
// recalled from, not the whole deployment — see entityIDF.
// entitySeedPolicy decides which seeds may INJECT new rows (pass 2b). Pass 2a
// (boosting rows already in the result set) is unaffected by it.
//
// calibrated=false: the response has no calibrated relevance band (rrf is
// skipped anyway; weighted_sum, CGDN, a degraded semantic channel, or an
// embed model with no measured noise baseline). There is no noise-relative
// bar to judge a seed by, so every seed may inject, as before.
//
// calibrated=true: only seeds whose band is moderate or strong (in canInject)
// may inject. A weak row carries too little evidence to vouch for memories
// that matched nothing at all, and on a weak-only response — the shape a
// made-up query produces — nothing is injected. Honest clock, increment 1:
// with age no longer gating old weak rows out cold, weak seeds injecting
// entity neighbours turned 74 weak rows into 111 more not_scored ones on 60
// made-up-word queries on a production install.
type entitySeedPolicy struct {
	calibrated bool
	canInject  map[storage.ULID]struct{}
}

// mayInject reports whether seed id may inject new rows under the policy.
func (p entitySeedPolicy) mayInject(id storage.ULID) bool {
	if !p.calibrated {
		return true
	}
	_, ok := p.canInject[id]
	return ok
}

// entitySeedPolicyFor computes the policy from the run's own bands. The band
// of each row is computed exactly as the response will present it
// (applyRelevanceBands, from the row's AbsoluteScore — phase 6's measured
// evidence, which entity boost never changes), BEFORE the boost runs.
func entitySeedPolicyFor(rows []activation.ScoredEngram, cal activation.RelevanceCalibration, calibrationGate float64, semanticDegraded bool) entitySeedPolicy {
	if responseBandBasis(cal, semanticDegraded) != "" {
		return entitySeedPolicy{}
	}
	bands, _ := applyRelevanceBands(rows, cal, calibrationGate, semanticDegraded)
	p := entitySeedPolicy{calibrated: true, canInject: make(map[storage.ULID]struct{}, len(rows))}
	for i, b := range bands {
		if b == activation.RelevanceModerate || b == activation.RelevanceStrong {
			p.canInject[rows[i].Engram.ID] = struct{}{}
		}
	}
	return p
}

// applyEntityBoost is applyEntityBoostSeeded with the uncalibrated policy
// (every seed may inject) — the behaviour unit tests of the boost arithmetic
// exercise. activateCore calls applyEntityBoostSeeded with the run's policy.
func (e *Engine) applyEntityBoost(ctx context.Context, ws [8]byte, vaultSize int64, results []activation.ScoredEngram, req *activation.ActivateRequest) []activation.ScoredEngram {
	return e.applyEntityBoostSeeded(ctx, ws, vaultSize, results, req, entitySeedPolicy{})
}

func (e *Engine) applyEntityBoostSeeded(ctx context.Context, ws [8]byte, vaultSize int64, results []activation.ScoredEngram, req *activation.ActivateRequest, policy entitySeedPolicy) []activation.ScoredEngram {
	if len(results) == 0 {
		return results
	}

	// COG-18: the boost pass is explicitly skipped under rrf fusion. Its
	// factor/cap/noise-floor (see the CALIBRATION CAVEAT above) are calibrated
	// to the ACT-R [0, 1] scale; rrf finals are rank-based and typically land
	// in [0, 0.05]. Before R1 this was an accidental no-op — the pre-#590-fix
	// 0.1 threshold coerce meant no rrf result ever cleared the seed rule.
	// Now that default rrf recall reaches its genuine ~0.001 threshold
	// (activation.Run()'s mode-aware default), leaving this pass live would
	// let a +0.30-scale boost dominate and outrank real ~0.05-scale content
	// matches — an injected engram could out-score every genuine hit. Skip it
	// deliberately until the boost is recalibrated for rrf's scale (#570
	// follow-up); today's effective (accidental) behavior is preserved on
	// purpose.
	if req.Weights != nil && req.Weights.UseRRFFusion {
		slog.Debug("applyEntityBoost: skipped under rrf fusion (scale mismatch, #570 follow-up)")
		return results
	}

	// Seed the boost from at most entityBoostTopN top results, but ONLY from
	// results that genuinely cleared the relevance threshold. A result whose
	// Score is below the threshold is in the set only because it matched an
	// explicit tag filter (the S1 threshold bypass surfaces due:<=today style
	// reminders regardless of content relevance) — its named entities must not
	// become spread-activation seeds, or a content-unrelated reminder would drag
	// arbitrary entity-linked engrams into recall through a side door. results is
	// score-sorted descending, so above-threshold results are the prefix; we take
	// the top-N of those.
	seeds := make([]activation.ScoredEngram, 0, entityBoostTopN)
	for _, r := range results {
		if len(seeds) >= entityBoostTopN {
			break
		}
		if r.Score < req.Threshold {
			continue
		}
		seeds = append(seeds, r)
	}

	// Seeds in RANK order (the stable sort below reorders them injecting-first)
	// and the rare entities each carries — the provenance an association-only
	// row's `why` is built from (COG-35 associated channel).
	seedsInRank := make([]storage.ULID, len(seeds))
	for i, sd := range seeds {
		seedsInRank[i] = sd.Engram.ID
	}
	seedEntities := make(map[storage.ULID]map[string]struct{}, len(seeds))
	seedIDs := make(map[storage.ULID]struct{}, len(seeds))
	for _, s := range seeds {
		seedIDs[s.Engram.ID] = struct{}{}
	}

	n := vaultSize

	// Pass 1: accumulate rarity-weighted boosts per target engram.
	type boostAcc struct {
		total   float64
		counted map[string]struct{} // entities already credited to this target
		order   []string            // credited entities, in credit order
		// injectable is the part of total contributed by seeds the policy
		// lets inject; pass 2b compares THIS against the threshold.
		injectable float64
	}
	boosts := make(map[storage.ULID]*boostAcc)
	idfCache := make(map[string]float64, 8)
	scanned := make(map[string]struct{}, 8) // entities whose reverse index was already walked

	// Injecting seeds first: credit is once per (entity, target), so an entity
	// shared by an injecting and a non-injecting seed must be credited by the
	// injecting one, or its evidence would be lost to the injectable total.
	sort.SliceStable(seeds, func(i, j int) bool {
		return policy.mayInject(seeds[i].Engram.ID) && !policy.mayInject(seeds[j].Engram.ID)
	})
	for _, seed := range seeds {
		seedInjects := policy.mayInject(seed.Engram.ID)
		_ = e.store.ScanEngramEntities(ctx, ws, seed.Engram.ID, func(entityName string) error {
			idf, cached := idfCache[entityName]
			if !cached {
				idf = 0
				if rec, err := e.store.GetEntityRecord(ctx, ws, entityName); err == nil && rec != nil {
					idf = entityIDF(int64(rec.MentionCount), n)
				}
				idfCache[entityName] = idf
			}
			contribution := entityBoostFactor * idf
			if contribution < entityBoostNoiseFloor {
				return nil // ubiquitous entity — no evidence; skip its fan-out
			}
			if seedInjects {
				if seedEntities[seed.Engram.ID] == nil {
					seedEntities[seed.Engram.ID] = make(map[string]struct{}, 2)
				}
				seedEntities[seed.Engram.ID][entityName] = struct{}{}
			}
			// Credit is once-per-entity per target, so a second seed carrying
			// the same entity cannot change any accumulator — skip the rescan.
			if _, done := scanned[entityName]; done {
				return nil
			}
			scanned[entityName] = struct{}{}
			// For each engram mentioning this entity (0x23 reverse index).
			return e.store.ScanEntityEngrams(ctx, entityName, func(entityWS [8]byte, engramID storage.ULID) error {
				if entityWS != ws {
					return nil // skip other vaults
				}
				if _, isSeed := seedIDs[engramID]; isSeed {
					return nil // seeds keep their BFS scores
				}
				acc := boosts[engramID]
				if acc == nil {
					acc = &boostAcc{counted: make(map[string]struct{}, 2)}
					boosts[engramID] = acc
				}
				if _, done := acc.counted[entityName]; done {
					return nil // this entity already credited to this target
				}
				acc.counted[entityName] = struct{}{}
				acc.order = append(acc.order, entityName)
				acc.total = math.Min(acc.total+contribution, entityBoostCap)
				if seedInjects {
					acc.injectable = math.Min(acc.injectable+contribution, entityBoostCap)
				}
				return nil
			})
		})
	}

	if len(boosts) == 0 {
		return results
	}

	// COG-35: under content-first order the boost is association EVIDENCE —
	// recorded on the row, used to order rows with no content score and to
	// break exact content ties, never added to the score that orders matched
	// rows. rrf never reaches here; weighted_sum and CGDN keep the additive
	// boost (their order is deliberately unchanged).
	contentFirst := activation.ContentFirstOrder(req.Weights)

	// Pass 2a: boost engrams already in the result set.
	seenInResults := make(map[storage.ULID]int, len(results))
	for i, r := range results {
		seenInResults[r.Engram.ID] = i
	}
	for id, acc := range boosts {
		if idx, found := seenInResults[id]; found {
			results[idx].Components.EntityBoost = acc.total
			if !contentFirst {
				results[idx].Score += acc.total
				// Keep the reported Final consistent with the adjusted Score.
				results[idx].Components.Final = results[idx].Score
			}
			delete(boosts, id)
		}
	}

	// Pass 2b: inject engrams the pipeline did not retrieve, iff their
	// entity evidence alone clears the caller's threshold AND the visibility
	// gate admits them. A result below threshold stays below threshold
	// regardless of how it was found (the threshold comparison is boost
	// semantics, so it stays here), and an engram phase 6 would have hidden
	// (filters, trust, lease, validity) must not ride in through the boost
	// side door — that decision is the gate's, shared with every other
	// injector. Injection-side only: pass 2a boosts engrams the pipeline
	// already returned, which cleared the full contract in phase 6 —
	// re-gating those would be a no-op. Injections are typically few, so the
	// gate's per-candidate lease read costs less than batching would save.
	gate := newVisibilityGate(req, time.Now())
	for id, acc := range boosts {
		// Only evidence from seeds the policy lets inject counts toward
		// admitting a row the pipeline never retrieved (entitySeedPolicy).
		if acc.injectable <= 0 || acc.injectable < req.Threshold {
			continue
		}
		if c := recallCostFrom(ctx); c != nil {
			c.boostInjectReads.Add(1)
		}
		eng, err := e.store.GetEngram(ctx, ws, id)
		if err != nil || eng == nil {
			continue
		}
		if !gate.Admits(ctx, e.store, ws, eng) {
			continue
		}
		// Content-first: an injected row has NO content score, so its score
		// is 0 and it sorts after every scored row, ordered among the other
		// injections by its entity evidence (activation.ResultBefore).
		score := acc.total
		if contentFirst {
			score = 0
		}
		var evidence []activation.AssocEntity
		for _, name := range acc.order {
			ae := activation.AssocEntity{Name: name, Weight: entityBoostFactor * idfCache[name]}
			for _, sid := range seedsInRank {
				if _, carries := seedEntities[sid][name]; carries {
					ae.Seeds = append(ae.Seeds, sid)
				}
			}
			if len(ae.Seeds) > 0 {
				evidence = append(evidence, ae)
			}
		}
		results = append(results, activation.ScoredEngram{
			AssocEntities: evidence,
			Engram:        eng,
			Score:         score,
			Components: activation.ScoreComponents{
				EntityBoost: acc.total,
				Confidence:  float64(eng.Confidence),
				Final:       score,
			},
		})
	}

	// Deterministic order (#698): SliceStable on Score alone is NOT enough here —
	// pass 2b appends injections in Go map-iteration order, so tied boost totals
	// (equal EntityBoost·idf) would survive MaxResults truncation in an arbitrary
	// subset. Break ties newest-first (rankorder) so the output is independent of map order.
	// Mirrors applySupersession's tiebreak (engine_supersession.go).
	sort.SliceStable(results, func(i, j int) bool {
		return activation.ResultBefore(&results[i], &results[j], contentFirst)
	})
	return results
}
