package engine

import (
	"github.com/scrypster/muninndb/internal/cognitive/contradictext"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// annotateContradictions is the "agent feels it" conflict surface (increment 1
// of the self-knowledge wiring). It runs the model-free contradictext detector
// over every pair of the RETURNED result set and stamps each conflicting item
// with the other's ID in ContradictsIDs.
//
// Cost: O(k^2) pair comparisons where k is the (already truncated) result
// count, so k <= MaxResults — at the default 20 that is at most ~190 pairs, and
// each contradictext.Conflict call is deterministic, allocation-light and
// sub-microsecond, so the whole pass runs in well under a millisecond. The
// caller gates it behind req.SelfKnowledge so recalls that don't ask never pay.
//
// INVARIANT — annotations never reorder results: this pass mutates only each
// item's ContradictsIDs slice. It never reorders, inserts, or removes an item,
// so the response's result order and membership are byte-for-byte identical to
// a recall that skipped it (the ContradictsIDs field is omitempty). It must run
// POST-TRUNCATION so the pairs it compares are exactly the results the caller
// sees — a conflict is only meaningful against another surfaced result.
//
// Comparison text is concept + content: the concept binds the shared subject
// (contradictext's same-subject gate needs >=2 shared subject words) and the
// content carries the asserted value/polarity the detector flips on.
//
// This is the pure-text, returned-set-bounded detection. Merging already-
// persisted explicit RelContradicts edges (both ends in the returned set) is a
// separate, store-reading concern still served by the annotate=true path's
// ConflictsWith; folding it into this flat surface is a follow-on increment.
//
// TODO(self-knowledge): vault-wide predictive staleness — for each returned
// result, is there a NEWER, highly-similar result ANYWHERE in the vault that
// supersedes it — is a later increment. Increment 1 ships explicit-supersession
// staleness only (SupersededBy != "", always-on from applySupersession) plus
// this returned-set contradiction pass; neither does a vault-wide lookup.
func annotateContradictions(items []mbp.ActivationItem) {
	for i := 0; i < len(items); i++ {
		ti := items[i].Concept + " " + items[i].Content
		for j := i + 1; j < len(items); j++ {
			tj := items[j].Concept + " " + items[j].Content
			if conflict, _, _ := contradictext.Conflict(ti, tj); conflict {
				items[i].ContradictsIDs = append(items[i].ContradictsIDs, items[j].ID)
				items[j].ContradictsIDs = append(items[j].ContradictsIDs, items[i].ID)
			}
		}
	}
}
