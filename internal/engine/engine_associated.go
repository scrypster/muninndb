package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/rankorder"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// The `associated` channel (COG-35). Content-first order stops an
// association-only row — one the entity boost INJECTED because it shares a
// rare named entity with a top match, though it matched none of the query's
// words — from displacing a match; once the matches fill the limit it was
// simply gone. Measured by the adversarial review on a 1,000-memory fixture
// with realistic entity density: 46 of 150 limit-10 responses carried such a
// row before content-first order and 0 after. Those rows were displacing
// better matches, but dropping them silently removes "pushes to you when it
// matters" from recall. They come back here: separate from `memories`, never
// counted against the limit or in total, never a match, bounded.
const (
	// associatedMax is the most association-only rows one response carries.
	associatedMax = 3
	// associatedMaxBytes bounds the block: the JSON array as encoding/json
	// encodes it (HTML-escaped: <, > and & cost six bytes each), the
	// `"associated":` key not counted. An item too long on its own is
	// shortened to fit (associatedShorten); items that would push the array
	// past the bound are dropped, weakest first, and counted in
	// AssociatedOmitted.
	associatedMaxBytes = 600
	// associatedConceptRunes / associatedEntityRunes truncate the two
	// free-text fields so one long concept cannot spend the whole budget.
	associatedConceptRunes = 60
	associatedEntityRunes  = 40
	// associatedBasis is the only band-like statement an associated item
	// makes: associated with the matches, not matched.
	associatedBasis = "associated_not_matched"
)

type associatedCandidate struct {
	row      activation.ScoredEngram
	strength float64
	entity   string
	via      storage.ULID
}

// buildAssociated selects the response's `associated` block from the full
// post-pipeline list (`full`, before the cut) and the rows actually returned.
// A candidate is a row the entity boost injected (it carries AssocEntities and
// no content score) that did not make the cut. Its evidence counts only
// entities it shares with injecting seeds that were RETURNED — the seed rule
// (moderate/strong seeds on a calibrated response, entitySeedPolicy) already
// decided which seeds may inject, and the visibility gate already admitted the
// row exactly as injection does (lifecycle, meta filters, trust, lease,
// valid-time; #569), so nothing here re-implements either. Strength is the
// summed weight of those entities, capped at entityBoostCap; strongest first,
// exact ties newest-first; at most associatedMax items within
// associatedMaxBytes. nil when there is nothing — never an empty slice.
// omitted counts the candidates that qualified but are not shown (item or
// byte cap), so "more existed" never looks like "there were no more".
func buildAssociated(full, returned []activation.ScoredEngram) (items []mbp.AssociatedItem, ids []storage.ULID, omitted int) {
	if len(returned) == 0 {
		return nil, nil, 0
	}
	served := make(map[storage.ULID]bool, len(returned))
	for i := range returned {
		served[returned[i].Engram.ID] = true
	}
	var cands []associatedCandidate
	for i := range full {
		r := full[i]
		if len(r.AssocEntities) == 0 || served[r.Engram.ID] || r.Components.AbsoluteScore > 0 ||
			r.Admission != activation.AdmissionInjected {
			continue
		}
		c := associatedCandidate{row: r}
		bestW := -1.0
		for _, ae := range r.AssocEntities {
			var via storage.ULID
			for _, sid := range ae.Seeds {
				if served[sid] {
					via = sid
					break
				}
			}
			if via == (storage.ULID{}) {
				continue
			}
			c.strength += ae.Weight
			if ae.Weight > bestW || (ae.Weight == bestW && ae.Name < c.entity) {
				bestW, c.entity, c.via = ae.Weight, ae.Name, via
			}
		}
		if c.strength <= 0 {
			continue
		}
		c.strength = math.Min(c.strength, entityBoostCap)
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		return nil, nil, 0
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].strength != cands[j].strength {
			return cands[i].strength > cands[j].strength
		}
		return rankorder.Before(cands[i].row.Engram.ID, cands[j].row.Engram.ID)
	})
	shown := cands
	if len(shown) > associatedMax {
		shown = shown[:associatedMax]
	}
	for _, c := range shown {
		items = append(items, associatedShorten(c))
		ids = append(ids, c.row.Engram.ID)
	}
	for len(items) > 1 {
		if b, err := json.Marshal(items); err == nil && len(b) <= associatedMaxBytes {
			break
		}
		items, ids = items[:len(items)-1], ids[:len(ids)-1]
	}
	return items, ids, len(cands) - len(items)
}

// associatedShorten renders one candidate as an item, shortening its two
// free-text fields (concept first, then the entity name in why — never the
// ids) rune by rune until the item ALONE fits associatedMaxBytes as an
// array. The fixed part (id, why's frame and returned id, strength, basis)
// is ~180 bytes, so this always terminates within the cap. Truncated is set
// whenever either field was shortened.
func associatedShorten(c associatedCandidate) mbp.AssociatedItem {
	conceptN, entityN := associatedConceptRunes, associatedEntityRunes
	for {
		concept, ct := associatedTruncate(c.row.Engram.Concept, conceptN)
		entity, et := associatedTruncate(c.entity, entityN)
		it := mbp.AssociatedItem{
			ID:        c.row.Engram.ID.String(),
			Concept:   concept,
			Why:       fmt.Sprintf("shares entity %q with returned %s", entity, c.via.String()),
			Strength:  float32(math.Round(c.strength*1000) / 1000),
			Basis:     associatedBasis,
			Truncated: ct || et,
		}
		b, err := json.Marshal([]mbp.AssociatedItem{it})
		if err != nil || len(b) <= associatedMaxBytes || (conceptN == 0 && entityN == 0) {
			return it
		}
		if conceptN > 0 && (utf8.RuneCountInString(concept) >= utf8.RuneCountInString(entity) || entityN == 0) {
			conceptN = min(conceptN, utf8.RuneCountInString(concept)) - 1
		} else {
			entityN = min(entityN, utf8.RuneCountInString(entity)) - 1
		}
	}
}

// associatedTruncate returns s cut to at most n runes, the last of them
// "…" when it was cut (n == 0: empty), and whether it was cut. Cuts fall on
// rune boundaries, so valid UTF-8 stays valid.
func associatedTruncate(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	if n <= 0 {
		return "", true
	}
	r := []rune(s)
	return string(r[:n-1]) + "…", true
}
