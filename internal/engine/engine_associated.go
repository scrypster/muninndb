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
	// associatedMaxBytes bounds the JSON-encoded block. Items that would
	// push it past the bound are dropped, weakest first.
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
func buildAssociated(full, returned []activation.ScoredEngram) (items []mbp.AssociatedItem, ids []storage.ULID) {
	if len(returned) == 0 {
		return nil, nil
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
		return nil, nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].strength != cands[j].strength {
			return cands[i].strength > cands[j].strength
		}
		return rankorder.Before(cands[i].row.Engram.ID, cands[j].row.Engram.ID)
	})
	if len(cands) > associatedMax {
		cands = cands[:associatedMax]
	}
	for _, c := range cands {
		items = append(items, mbp.AssociatedItem{
			ID:       c.row.Engram.ID.String(),
			Concept:  truncateRunes(c.row.Engram.Concept, associatedConceptRunes),
			Why:      fmt.Sprintf("shares entity %q with returned %s", truncateRunes(c.entity, associatedEntityRunes), c.via.String()),
			Strength: float32(math.Round(c.strength*1000) / 1000),
			Basis:    associatedBasis,
		})
		ids = append(ids, c.row.Engram.ID)
	}
	for len(items) > 0 {
		if b, err := json.Marshal(items); err == nil && len(b) <= associatedMaxBytes {
			break
		}
		items, ids = items[:len(items)-1], ids[:len(ids)-1]
	}
	if len(items) == 0 {
		return nil, nil
	}
	return items, ids
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
