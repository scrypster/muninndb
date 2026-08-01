package engine

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// substitutionBlock records a shadow whose declared chain produced no
// substitutable head. Silence is what produced #763, so the refusal is carried
// out of the phase rather than dropped: when the whole response ends up empty,
// it becomes an abstention reason an agent can act on ("there is a version
// chain here and I will not guess which branch is current — read <id>").
type substitutionBlock struct {
	PredecessorID storage.ULID
	Reason        string
}

// applyVersionHeadSubstitution is COG-28 (#763): if a query's evidence reached
// ANY member of a DECLARED version chain with admission-worthy strength, recall
// resolves that evidence to the chain's authoritative current head before
// ranking, and says so.
//
// THE BUG IT CLOSES is an ordering bug, not a scoring one. EvolveAt
// soft-deletes its predecessor, but nothing removes that predecessor from HNSW
// or FTS — its vector and its postings are exactly the ones the user's OLD
// wording matches. Phase 6's lifecycle cut then discards it before it is ever
// scored, so its earned relevance is thrown away rather than redirected, and
// applySupersession (which already injects an absent head for the VISIBLE stale
// case) can never see it. When the rewrite changed the vocabulary, a query
// phrased against the old wording reaches only the predecessor and the current
// truth is stored, current, and invisible.
//
// THE SPLIT. Phase 6 keeps the evidence — it is the only place with the query
// embedding, the corpus IDF statistics and the abstention formula, so it scores
// the refused predecessor with the SAME functions at the SAME threshold and
// emits it as an activation.ShadowMatch (Activations stays byte-identical).
// This phase resolves and injects — it is the only place with
// reverse-association reads, the visibility gate and resolveSupersessionHead,
// which already implements every chain-walk property #763 asks for. No new
// scoring math is invented anywhere.
//
// HONESTY RULES, all mandatory:
//   - The head ranks at the PREDECESSOR's Final. That is not a new idea: it is
//     the doctrine already written into applySupersession — "the score belongs
//     to the TOPIC, and the head is the correct answer for it" — extended to
//     the case where the stale member happens to be invisible. Re-scoring the
//     head against the query instead would gate the successor on wording it
//     deliberately no longer has, reproducing the bug one layer deeper; an
//     ε-discount would be a silent statement that we trust the author's
//     declaration less than we say we do.
//   - ScoreComponents are the predecessor's real measurements, verbatim, and
//     the substituted_for / substitution_basis attribution that says so is NOT
//     optional. Under include_why the Why string leads with it in plain
//     language.
//   - The ONLY admission path is a predecessor that cleared req.Threshold on
//     the unchanged formula with the tag-pool bypass explicitly disabled.
//     Substitution REDIRECTS admission-worthy evidence; it never CREATES it.
//
// Runs immediately BEFORE applySupersession on the same clock, so a head
// injected here is an ordinary result row by the time that phase runs and a
// head that is itself further superseded gets the existing promote/demote
// treatment for free. Read-only: reverse-assoc, engram, lease and (only for an
// injected head) one embedding read. No writes, no reinforcement — a shadow
// never enters the activation log, so a superseded fact is not STRENGTHENED by
// having been matched.
func (e *Engine) applyVersionHeadSubstitution(
	ctx context.Context,
	ws [8]byte,
	results []activation.ScoredEngram,
	shadows []activation.ShadowMatch,
	req *activation.ActivateRequest,
	gate *visibilityGate,
	nameableCache map[storage.ULID]bool,
) (out []activation.ScoredEngram, injected int, blocked []substitutionBlock) {
	if len(shadows) == 0 {
		return results, 0, nil
	}

	seen := make(map[storage.ULID]int, len(results))
	for i, r := range results {
		seen[r.Engram.ID] = i
	}
	// Same cached-existence closure applySupersession uses, over the SAME cache
	// instance: a node already in the result pool cleared its own admission, so
	// it is nameable by construction and costs no store read.
	nameable := func(eng *storage.Engram) bool {
		if _, inPool := seen[eng.ID]; inPool {
			return true
		}
		if a, decided := nameableCache[eng.ID]; decided {
			return a
		}
		a := gate.Nameable(ctx, e.store, ws, eng)
		nameableCache[eng.ID] = a
		return a
	}

	// One row per resolved head, attributed to its HIGHEST-scoring contributing
	// shadow. Several shadows in one chain (A and B both matched, both resolve
	// to C) therefore produce one row, one attribution, deterministically.
	type headSub struct {
		head      *storage.Engram
		basis     activation.ShadowMatch
		truncated bool
	}
	var order []storage.ULID
	subs := make(map[storage.ULID]*headSub, len(shadows))

	for _, sh := range shadows {
		if err := ctx.Err(); err != nil {
			break
		}
		walk := e.resolveSupersessionHead(ctx, ws, sh.Engram.ID, gate, nameable)
		if !walk.OK {
			// "not superseded" is the overwhelmingly common case (any expired
			// or forgotten engram that happened to match) and is not a block —
			// nothing was refused, there was simply no declared chain.
			if walk.Reason != supersessionBlockNotSuperseded {
				blocked = append(blocked, substitutionBlock{PredecessorID: sh.Engram.ID, Reason: walk.Reason})
			}
			continue
		}
		// Admits, not Nameable: a substituted head is presented as CURRENT, so
		// validity is load-bearing here in a way it is not for a lineage
		// annotation. resolveSupersessionHead already enforced ValidForView on
		// what it returned; this is the explicit contract statement at the
		// injection site, matching how the entity-boost and supersession
		// injectors each state their own gate call.
		if !gate.Admits(ctx, e.store, ws, walk.Head) {
			blocked = append(blocked, substitutionBlock{PredecessorID: sh.Engram.ID, Reason: supersessionBlockHidden})
			continue
		}
		// The predecessor must be nameable AS LINEAGE before we may attribute
		// the substitution to it (COG-22's amendment). If it is not, the row
		// would be silently substituted, and a silent substitution is worse
		// than no substitution — so refuse the whole thing rather than inject
		// an unattributed row.
		if !gate.NameableAsLineage(ctx, e.store, ws, sh.Engram) {
			blocked = append(blocked, substitutionBlock{PredecessorID: sh.Engram.ID, Reason: supersessionBlockHidden})
			continue
		}
		if walk.Truncated {
			slog.Warn("recall: version-head substitution truncated at the chain depth cap; the injected head may not be the terminus",
				"predecessor", sh.Engram.ID.String(), "head", walk.Head.ID.String(), "max_depth", supersessionMaxDepth)
		}
		cur, ok := subs[walk.Head.ID]
		if !ok {
			subs[walk.Head.ID] = &headSub{head: walk.Head, basis: sh, truncated: walk.Truncated}
			order = append(order, walk.Head.ID)
			continue
		}
		cur.truncated = cur.truncated || walk.Truncated
		if sh.Final > cur.basis.Final {
			cur.basis = sh
		}
	}
	if len(subs) == 0 {
		return results, 0, blocked
	}

	// Copy before mutating. Every write below lands in the backing array Run()
	// returned, and the activation engine's async log-drain goroutine may still
	// be reading that array — the same live hazard applySupersession documents
	// and the final COG-19 gate avoids by filtering into a NEW slice. Paid only
	// when a substitution actually happens.
	results = append(make([]activation.ScoredEngram, 0, len(results)+len(subs)), results...)

	for _, headID := range order {
		s := subs[headID]
		if idx, inPool := seen[headID]; inPool {
			// The head earned its own place. Raise it to the topic's score by
			// the same MAX rule applySupersession applies — and when the raise
			// actually fires, ATTRIBUTE it: the displayed Score is now the
			// PREDECESSOR's measurement sitting beside the row's own
			// ScoreComponents, and COG-28's rule is that attribution is never
			// optional on a row whose shown number is not its own aboutness.
			// substitution_basis names the predecessor evidence that produced
			// the score; without it the caller sees score=0.9 beside
			// components.final=0.2, unexplained. Nothing was INJECTED, so
			// injected is not incremented and TotalFound stays honest; a head
			// that matched at or above the shadow keeps its own score and no
			// annotation (nothing about it was substituted).
			if s.basis.Final > results[idx].Score {
				basis := s.basis.Components
				results[idx].Score = s.basis.Final
				results[idx].SubstitutedFor = s.basis.Engram.ID
				results[idx].SubstitutionBasis = &basis
				results[idx].ChainTruncated = results[idx].ChainTruncated || s.truncated
				if req.IncludeWhy {
					clause := raiseWhy(s.basis.Engram.ID, s.truncated)
					if results[idx].Why != "" {
						results[idx].Why += "; " + clause
					} else {
						results[idx].Why = clause
					}
				}
			}
			continue
		}
		basis := s.basis.Components
		row := activation.ScoredEngram{
			Engram:            s.head,
			Score:             s.basis.Final,
			Components:        basis,
			SubstitutedFor:    s.basis.Engram.ID,
			SubstitutionBasis: &basis,
			// #773: this row's Components ARE the predecessor's MEASURED
			// evidence against this query (that is what substitution_basis
			// attributes), and the shadow that produced them cleared the same
			// req.Threshold on the same gated quantity as any live row. So it
			// bands on real measurement, not on the zero value an unmeasured
			// injector would leave here.
			Admission:         activation.AdmissionScored,
			ChainTruncated:    s.truncated,
			HeadNotIndexedYet: e.headLacksEmbedding(ctx, ws, s.head),
		}
		if req.IncludeWhy {
			row.Why = substitutionWhy(s.basis.Engram.ID, row.HeadNotIndexedYet, s.truncated)
		}
		results = append(results, row)
		seen[headID] = len(results) - 1
		injected++
	}

	// Deterministic order: stable sort with a ULID tiebreak, matching
	// applySupersession, so an injected head that ties a returned row lands
	// reproducibly. applySupersession re-sorts after this, but it returns early
	// when no stale row is present — so this phase must leave the set ordered.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		a, b := results[i].Engram.ID, results[j].Engram.ID
		return bytes.Compare(a[:], b[:]) < 0
	})
	return results, injected, blocked
}

// headLacksEmbedding reports whether an injected head has no stored vector, so
// the response can say "not indexed yet" instead of leaving it
// indistinguishable from "not relevant" (the loud-degradation doctrine applied
// to the exact fresh-evolve window #763 names). One point read, taken only for
// a head that is actually being injected. A read error is reported as "not
// indexed": on this path an unknown is closer to absent than to present, and
// the field is advisory.
func (e *Engine) headLacksEmbedding(ctx context.Context, ws [8]byte, head *storage.Engram) bool {
	if len(head.Embedding) > 0 {
		return false
	}
	vec, err := e.store.GetEmbedding(ctx, ws, head.ID)
	return err != nil || len(vec) == 0
}

// substitutionWhy is the include_why clause. It leads, in plain language, with
// the one thing a reader cannot infer from the score card: this row's OWN
// wording did not match.
func substitutionWhy(predecessor storage.ULID, notIndexed, truncated bool) string {
	why := fmt.Sprintf("returned because your query matched the earlier version %s, which this replaces — "+
		"this memory's own wording did not match", predecessor.String())
	if notIndexed {
		why += "; this memory has no embedding yet (indexing pending), so it could not match semantically on its own"
	}
	if truncated {
		why += "; the version chain was longer than the walk limit, so this may not be the very latest version"
	}
	return why
}

// raiseWhy is the include_why clause for a head that WAS retrieved on its own
// merit but whose score was raised to the predecessor's stronger match. It
// differs from substitutionWhy on the one fact that matters: this row's own
// wording DID match — just more weakly than the version it replaces.
func raiseWhy(predecessor storage.ULID, truncated bool) string {
	why := fmt.Sprintf("score raised to the match of the earlier version %s, which this replaces — "+
		"your query matched that superseded version more strongly than this memory's own wording", predecessor.String())
	if truncated {
		why += "; the version chain was longer than the walk limit, so this may not be the very latest version"
	}
	return why
}

// versionHeadAbstainReason converts the blocks recorded by a substitution phase
// that injected nothing into the abstention reason for an otherwise-empty
// response. A fork wins over everything else: "I will not guess which branch is
// current" is more actionable than "no current version exists", and a caller
// that sees it knows to read the predecessor and resolve the fork. Returns ""
// when there is nothing to say.
//
// Every other block — retracted, hidden, no_current_version and CYCLE — folds
// into superseded_only, deliberately: what the caller can act on is identical
// ("your evidence is in a declared chain with no reachable current head; read
// the predecessor"), and a per-block wire value would expand the AbstainedReason
// contract for conditions the caller cannot handle differently. A cycle in
// particular is a pathological authoring error the walk's WARN log already
// names precisely; pinned by TestVersionHead_SupersessionCycleAbstainsSupersededOnly.
func versionHeadAbstainReason(blocked []substitutionBlock) string {
	if len(blocked) == 0 {
		return ""
	}
	for _, b := range blocked {
		if b.Reason == supersessionBlockAmbiguous {
			return activation.AbstainAmbiguousVersion
		}
	}
	return activation.AbstainSupersededOnly
}
