package engine

import (
	"context"
	"math"
	"sort"

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
// caller's threshold AND they pass the caller's meta filters — the pipeline
// applies filters in phase 6, so anything appended afterwards must honor the
// same contract or a tags_all query returns records without the required
// tags (issue #569). Both carry the boost in Components.EntityBoost so the
// adjustment is auditable — injected results never appear with an empty
// component trace (issue #569).
//
// Results are re-sorted by score descending.
//
// vaultSize is the recalled vault's engram count (activateCore already holds
// it); it feeds entityIDF's n so rarity is judged against the vault being
// recalled from, not the whole deployment — see entityIDF.
func (e *Engine) applyEntityBoost(ctx context.Context, ws [8]byte, vaultSize int64, results []activation.ScoredEngram, threshold float64, filters []activation.Filter) []activation.ScoredEngram {
	if len(results) == 0 {
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
		if r.Score < threshold {
			continue
		}
		seeds = append(seeds, r)
	}

	seedIDs := make(map[storage.ULID]struct{}, len(seeds))
	for _, s := range seeds {
		seedIDs[s.Engram.ID] = struct{}{}
	}

	n := vaultSize

	// Pass 1: accumulate rarity-weighted boosts per target engram.
	type boostAcc struct {
		total   float64
		counted map[string]struct{} // entities already credited to this target
	}
	boosts := make(map[storage.ULID]*boostAcc)
	idfCache := make(map[string]float64, 8)

	for _, seed := range seeds {
		_ = e.store.ScanEngramEntities(ctx, ws, seed.Engram.ID, func(entityName string) error {
			idf, cached := idfCache[entityName]
			if !cached {
				idf = 0
				if rec, err := e.store.GetEntityRecord(ctx, entityName); err == nil && rec != nil {
					idf = entityIDF(int64(rec.MentionCount), n)
				}
				idfCache[entityName] = idf
			}
			contribution := entityBoostFactor * idf
			if contribution < entityBoostNoiseFloor {
				return nil // ubiquitous entity — no evidence; skip its fan-out
			}
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
				acc.total = math.Min(acc.total+contribution, entityBoostCap)
				return nil
			})
		})
	}

	if len(boosts) == 0 {
		return results
	}

	// Pass 2a: boost engrams already in the result set.
	seenInResults := make(map[storage.ULID]int, len(results))
	for i, r := range results {
		seenInResults[r.Engram.ID] = i
	}
	for id, acc := range boosts {
		if idx, found := seenInResults[id]; found {
			results[idx].Score += acc.total
			results[idx].Components.EntityBoost = acc.total
			// Keep the reported Final consistent with the adjusted Score.
			results[idx].Components.Final = results[idx].Score
			delete(boosts, id)
		}
	}

	// Pass 2b: inject engrams the pipeline did not retrieve, iff their
	// entity evidence alone clears the caller's threshold. A result below
	// threshold stays below threshold regardless of how it was found.
	for id, acc := range boosts {
		if acc.total < threshold {
			continue
		}
		eng, err := e.store.GetEngram(ctx, ws, id)
		if err != nil || eng == nil {
			continue
		}
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
			continue
		}
		if !activation.PassesMetaFilter(eng, filters) {
			continue // injected results obey the caller's filters like any pipeline result
		}
		results = append(results, activation.ScoredEngram{
			Engram: eng,
			Score:  acc.total,
			Components: activation.ScoreComponents{
				EntityBoost: acc.total,
				Confidence:  float64(eng.Confidence),
				Final:       acc.total,
			},
		})
	}

	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	return results
}
