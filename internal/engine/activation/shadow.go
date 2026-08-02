package activation

import (
	"bytes"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// shadowMatchCap bounds how many shadow matches one query may emit. Each one
// costs the engine layer a reverse-association iterator on the hot recall path
// in exchange for at most one injected row, so the cap plays the same role
// supersessionMargin already plays for the existing chain walk. Sorted
// score-descending with a ULID tiebreak before the cut, so which 16 survive is
// deterministic.
const shadowMatchCap = 16

// ShadowMatch is a candidate that earned admission on RELEVANCE — same scoring
// functions, same gate quantity, same req.Threshold as every returned row — but
// was refused by one of the two currency predicates while carrying the declared
// supersession signature.
//
// It is NOT a result and never becomes one. It never enters Activations, never
// enters TotalFound, never reaches the activation log, and so never contributes
// a Hebbian edge or an access-count bump: a superseded fact must not be
// STRENGTHENED by having been matched. It is evidence, offered to the engine
// layer, which may resolve it to the head of its DECLARED RelSupersedes chain
// and inject that head instead (COG-28, issue #763). If the engine layer does
// nothing with it, nothing about the response changes.
//
// The distinction that makes this safe: substitution REDIRECTS admission-worthy
// evidence, it never CREATES it. The only way to produce a ShadowMatch is to
// clear the caller's own threshold on the unchanged formula.
type ShadowMatch struct {
	// Engram is the refused predecessor, fully loaded — the engine layer needs
	// its ID (to walk the chain) and its State/ValidUntil (to name it as
	// lineage under the COG-22 NameableAsLineage clause).
	Engram *storage.Engram
	// Final is the ranking score on THIS query's live scale, so a head injected
	// at this value lands comparably against every row that was returned.
	Final float64
	// Gated is the quantity that was compared against req.Threshold —
	// AbsoluteScore on the ACT-R/CGDN paths, Final on weighted_sum. It is the
	// number reported as substitution_basis.absolute_score.
	Gated float64
	// Components are the predecessor's MEASURED evidence against this query,
	// verbatim. Nothing here is fabricated or copied from another query; they
	// are the measurements that admitted the substitution, which is exactly
	// what a score card is for. They are never presented as the injected head's
	// own aboutness — the substituted_for / substitution_basis attribution is
	// mandatory on any row that carries them.
	Components ScoreComponents
}

// shadowsEnabled reports whether this request may produce shadow matches at
// all. Four hard exclusions, each a design decision rather than an
// optimization:
//
//   - as_of / include_invalid: HISTORICAL queries never substitute. Under
//     as_of=T the predecessor is admitted normally and IS the right answer;
//     substituting its successor would answer a different question than the one
//     asked, and under as_of the successor may not exist yet in the view at
//     all. One branch, not a filter later on (COG-28 / design §6.4).
//   - RRF fusion: RRF finals are rank-based and its coerced default threshold
//     is ~0.001 (COG-6), so "cleared the bar" carries almost no information —
//     substitution would fire on essentially any superseded engram any index
//     returned. Skipped for the structurally identical calibration reason
//     COG-18's R1 amendment skips the entity boost under RRF. Named deferral.
//   - Threshold < 0: Explain's diagnostic bypass (query.go, "gate nothing").
//     At threshold -1 every superseded predecessor in the pool "clears the
//     bar", so Explain would report a would_return recall never produces.
//   - The whole path is otherwise zero-cost: with no signature-carrying refusal
//     the shadow map is never allocated and the second scoring pass never runs.
func shadowsEnabled(req *ActivateRequest, w resolvedWeights) bool {
	if req.AsOf != nil || req.IncludeInvalid {
		return false
	}
	if w.UseRRFFusion {
		return false
	}
	return req.Threshold >= 0
}

// hasSupersessionSignature reports whether eng carries the exact pair of stored
// states a DECLARED supersession produces. It is a cheap pre-filter that costs
// no store read; the authoritative test is still the RelSupersedes edge the
// engine layer walks. A signature-carrying engram with no declared successor
// simply produces nothing.
//
//   - soft-deleted + a CLOSED ValidUntil is the evolve signature (EvolveAt
//     commits SupersedeEngram = soft-delete AND the stamp in one batch).
//   - still-active + a closed, already-elapsed ValidUntil is the
//     Link(relation=supersedes) / forget(not_true_since) signature.
//
// StateArchived is never a shadow, on exactly the reasoning PassesLifecycle
// already gives: archival is a storage-tier eviction, not a statement about
// truth, and the payload is not guaranteed resident. A plain soft-delete with
// an OPEN ValidUntil (muninn_forget without not_true_since) is trash, not
// history, and is never a shadow.
func hasSupersessionSignature(eng *storage.Engram, now time.Time) bool {
	if eng == nil {
		return false
	}
	switch eng.State {
	case storage.StateSoftDeleted:
		return !eng.ValidUntil.IsZero()
	case storage.StateActive:
		return !eng.ValidUntil.IsZero() && eng.IsExpired(now)
	default:
		return false
	}
}

// collectShadowMatches scores every shadow candidate with scoreOne — the
// caller's own mode-specific closure over the LIVE query's normalization
// constants — and returns the ones that cleared req.Threshold, score-descending
// and capped.
//
// scoreOne returns (final, gated, components). The caller supplies it so that
// the shadow pass uses the identical formula the live path just used; the one
// thing this function enforces on its own is the gate, because that is where
// honesty is either preserved or lost.
//
// THE TAG-POOL BYPASS IS DELIBERATELY NOT APPLIED. On the live path,
// c.inTagPool lets a filter-defined candidate skip the relevance bar because
// THE FILTER DEFINES THE SET. A shadow is not in the returned set; it is
// evidence. Letting a tag hit manufacture a substitution would admit a chain
// head on zero aboutness — the promiscuity this design must not have.
func collectShadowMatches(
	all []scoringCandidate,
	shadowEngrams map[storage.ULID]*storage.Engram,
	req *ActivateRequest,
	scoreOne func(c scoringCandidate, eng *storage.Engram) (final, gated float64, comp ScoreComponents),
) []ShadowMatch {
	if len(shadowEngrams) == 0 {
		return nil
	}
	out := make([]ShadowMatch, 0, len(shadowEngrams))
	for _, c := range all {
		eng := shadowEngrams[c.id]
		if eng == nil {
			continue
		}
		// Every other phase-6 admission predicate applies unchanged. Trust,
		// standing exclude-tags and the lease were already enforced upstream
		// (a candidate refused by any of them never became a shadow); the meta
		// filter and the structured (MQL WHERE) predicate are enforced here,
		// where the live paths enforce them.
		if !PassesMetaFilter(eng, req.Filters) {
			continue
		}
		if req.StructuredFilter != nil && !req.StructuredFilter.Match(eng) {
			continue
		}
		final, gated, comp := scoreOne(c, eng)
		if gated < req.Threshold {
			continue
		}
		out = append(out, ShadowMatch{Engram: eng, Final: final, Gated: gated, Components: comp})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Final != out[j].Final {
			return out[i].Final > out[j].Final
		}
		a, b := out[i].Engram.ID, out[j].Engram.ID
		return bytes.Compare(a[:], b[:]) < 0
	})
	if len(out) > shadowMatchCap {
		out = out[:shadowMatchCap]
	}
	return out
}
