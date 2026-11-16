package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Inline supersedes (COG-38).
//
// A write may carry edges to existing memories: `associations` (REST, gRPC,
// MBP, the embedded library) and `relationships` (what muninn_remember sends).
// When such an edge says `supersedes`, the write is making the declaration
// Link(relation=supersedes) makes — "this memory replaces that one" — and it
// must mean the same thing. Before this file it did not: the write path stored
// the edge and nothing else, so the old memory stayed live and ranked as
// current, a second declaration forked it, and an append-mode credential could
// do both.
//
// Everything here routes the declaration through the mechanism Link and evolve
// already use, rather than a parallel one:
//
//   - the per-target evolveLock and the fork guard (refuseSupersededEvolveTarget),
//     the same pair Link and evolve take, so a target gains at most one successor
//     however the declarations race;
//   - Link's endpoint rule (the target exists and is neither soft-deleted nor
//     archived);
//   - the guarded edge writer (StoreBatch.WriteAssociation — STO-12 liveness and
//     the STO-15 relation-type collision check, repeated under the pair's lock at
//     commit);
//   - Link's retirement, a ValidUntil stamp that leaves the state active and
//     never moves an earlier stamp (StoreBatch.StampValidUntil, the batch form of
//     the primitive Link calls);
//   - refuseAppend, because retiring a memory modifies existing state (SEC-15).
//
// What is NOT Link's: the new engram, its supersedes edge(s) and the stamp(s)
// are ONE StoreBatch commit — the machinery evolve uses — so no crash or error
// can leave the edge without the stamp, or the stamp without the edge. (Link
// itself writes the edge and then stamps, best-effort.)
//
// SCOPE OF THIS INCREMENT: a write that CREATES a memory — Engine.Write on new
// content, and each such item of WriteBatch. A write that RESOLVES TO A MEMORY
// THAT ALREADY EXISTS is deliberately left exactly as it was, apart from the
// append gate, which applies to every write that declares a supersession:
//
//   - content-hash dedup hit: the existing id is returned with
//     `duplicate_content` and the declaration is not applied (as before);
//   - upsert_mode: a create still writes the supersedes edge inline with the
//     engram — no guard, no stamp — and a hit on an existing key still ignores
//     every inline edge (as before).
//
// Those paths need an answer to "is this declaration already in effect through
// this memory's own history?", which a create never has to ask; routing them
// through the guard without it would refuse a re-synced document on its own
// earlier success. They are the next increment.

// declaresInlineSupersedes reports whether a write request carries a supersedes
// declaration in either inline edge field. It is the cheap, request-only test
// the credential gate runs before anything else: no id is parsed and no vault
// setting is read, so a write that declares nothing pays one loop over fields
// that are almost always empty.
func declaresInlineSupersedes(req *mbp.WriteRequest) bool {
	for i := range req.Associations {
		if storage.RelType(req.Associations[i].RelType) == storage.RelSupersedes {
			return true
		}
	}
	for i := range req.Relationships {
		if storage.RelType(relTypeFromString(req.Relationships[i].Relation)) == storage.RelSupersedes {
			return true
		}
	}
	return false
}

// refuseInlineSupersedesForAppend is the SEC-15 half of COG-38. A write is an
// additive operation and an append credential may make one — but a write that
// declares a supersession retires the memory it names, and that is a
// modification of existing state, refused for append exactly as Link is. It is
// applied to EVERY write that carries the declaration, whichever path the write
// then takes, so no path is a way around it.
func (e *Engine) refuseInlineSupersedesForAppend(ctx context.Context) error {
	if err := e.refuseAppend(ctx); err != nil {
		return fmt.Errorf("write refused, nothing was stored: it declares an inline supersedes, which retires the memory it names: %w", err)
	}
	return nil
}

// inlineSupersedesError is a write refused because a supersedes declaration it
// carries cannot be honoured. The cause stays matchable (errors.Is /
// errors.As through Unwrap) so every transport can classify it, and the text
// says the two things a caller needs: nothing was stored, and why.
type inlineSupersedesError struct {
	target storage.ULID
	cause  error
}

func (e *inlineSupersedesError) Error() string {
	return fmt.Sprintf("write refused, nothing was stored: it declares that it supersedes %s, and that cannot be honoured: %v", e.target.String(), e.cause)
}

func (e *inlineSupersedesError) Unwrap() error { return e.cause }

func inlineSupersedesRefused(target storage.ULID, cause error) error {
	return &inlineSupersedesError{target: target, cause: cause}
}

// sameAssocKeyWeight reports whether two weights occupy the same position in an
// association key. The key stores keys.WeightComplement(weight), which
// saturates: every weight >= 1 is one position, every weight <= 0 another. So
// 1 and 2 are different floats and the SAME key, and comparing the floats
// would let two edges that share a key through as if they did not.
func sameAssocKeyWeight(a, b float32) bool {
	return keys.WeightComplement(a) == keys.WeightComplement(b)
}

// declaredSupersedes extracts the supersedes declarations a write carries
// inline, converted exactly as Write converts every inline edge (each caller's
// weight is kept as supplied), one per distinct target.
//
// Two things are refused here rather than resolved quietly:
//
//   - relationships[] on a vault whose inline_enrichment is background_only.
//     That mode ignores caller-supplied relationships, so the declaration would
//     be dropped without a word — the outcome COG-38 exists to remove. The vault
//     setting is explicit and so is the declaration; neither is overridden.
//   - one target declared more than once at weights that are different key
//     positions: one declaration, two answers.
func (e *Engine) declaredSupersedes(req *mbp.WriteRequest, vaultName string) ([]storage.Association, error) {
	var out []storage.Association
	add := func(a storage.Association) error {
		for i := range out {
			if out[i].TargetID != a.TargetID {
				continue
			}
			if !sameAssocKeyWeight(out[i].Weight, a.Weight) {
				return fmt.Errorf("%w: write refused, nothing was stored: it declares that it supersedes %s more than once, at weights %v and %v",
					ErrInvalidRequest, a.TargetID.String(), out[i].Weight, a.Weight)
			}
			return nil // the same declaration, repeated
		}
		out = append(out, a)
		return nil
	}
	for _, a := range req.Associations {
		if storage.RelType(a.RelType) != storage.RelSupersedes {
			continue
		}
		targetID, err := storage.ParseULID(a.TargetID)
		if err != nil {
			return nil, fmt.Errorf("%w: association target_id %q: %v", ErrInvalidID, a.TargetID, err)
		}
		if err := add(storage.Association{
			TargetID:      targetID,
			RelType:       storage.RelSupersedes,
			Weight:        a.Weight,
			Confidence:    a.Confidence,
			CreatedAt:     time.Unix(0, a.CreatedAt),
			LastActivated: a.LastActivated,
		}); err != nil {
			return nil, err
		}
	}
	relationshipsIgnored := e.ResolveVaultPlasticity(vaultName).InlineEnrichment == "background_only"
	for _, rel := range req.Relationships {
		if storage.RelType(relTypeFromString(rel.Relation)) != storage.RelSupersedes {
			continue
		}
		if relationshipsIgnored {
			return nil, fmt.Errorf("%w: write refused, nothing was stored: it declares that it supersedes %s in `relationships`, but vault %q has inline_enrichment=background_only, which ignores caller-supplied relationships — the declaration would be dropped. Declare it with a supersedes link (muninn_link / POST /api/link) after the write, or evolve the old memory instead",
				ErrInvalidRequest, rel.TargetID, vaultName)
		}
		targetID, err := storage.ParseULID(rel.TargetID)
		if err != nil {
			return nil, fmt.Errorf("%w: relationship target_id %q: %v", ErrInvalidID, rel.TargetID, err)
		}
		if err := add(storage.Association{
			TargetID:   targetID,
			RelType:    storage.RelSupersedes,
			Weight:     rel.Weight,
			Confidence: 1.0,
			CreatedAt:  time.Now(),
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// splitInlineSupersedes separates a write's converted inline edges into the
// supersedes declarations (which take the guarded path) and everything else
// (written inline with the engram, as before). It refuses the one combination
// that cannot be stored: another relation to a declared target at the SAME KEY
// POSITION. An association key is ws|source|weight|target with no relation in
// it (STO-15), so the two edges are one key and the later Set would erase the
// earlier — either the supersedes edge (a stamped target with no successor) or
// the other declaration. Link refuses that collision; so does the write.
//
// "Same key position" is decided by the key encoding, not by comparing the
// floats (sameAssocKeyWeight): the encoding saturates, so contradicts@1 and
// supersedes@2 are one key although 1 != 2.
func splitInlineSupersedes(assocs []storage.Association) (rest []storage.Association, err error) {
	rest = make([]storage.Association, 0, len(assocs))
	for i := range assocs {
		if assocs[i].RelType != storage.RelSupersedes {
			rest = append(rest, assocs[i])
		}
	}
	for i := range assocs {
		if assocs[i].RelType != storage.RelSupersedes {
			continue
		}
		for j := range rest {
			if rest[j].TargetID == assocs[i].TargetID && sameAssocKeyWeight(rest[j].Weight, assocs[i].Weight) {
				return nil, fmt.Errorf("%w: write refused, nothing was stored: it declares both supersedes (weight %v) and %s (weight %v) toward %s, and those two weights are the same position in an association key, which has no relation in it — one edge would silently replace the other (%w). Give the two relations weights that differ between 0 and 1, or declare one",
					ErrInvalidRequest, assocs[i].Weight, relTypeName(rest[j].RelType), rest[j].Weight, assocs[i].TargetID.String(), storage.ErrAssocRelTypeCollision)
			}
		}
	}
	return rest, nil
}

// relTypeName is the relation name a caller wrote for a relation type, for
// error text: a refusal that says "relation 2" makes the caller look it up.
// The inverse of relTypeFromString over the names it knows.
func relTypeName(rt storage.RelType) string {
	for _, name := range []string{
		"supports", "contradicts", "depends_on", "supersedes", "relates_to", "is_part_of",
		"causes", "preceded_by", "followed_by", "created_by_person", "belongs_to_project",
		"references", "implements", "blocks", "resolves", "refines",
	} {
		if storage.RelType(relTypeFromString(name)) == rt {
			return name
		}
	}
	return fmt.Sprintf("relation type %d", uint16(rt))
}

// lockSupersedeTargets takes the evolveLock of every declared target and returns
// the function that releases them. Several targets may share a stripe and a
// stripe mutex is not reentrant, so the stripes are de-duplicated; they are
// taken in ascending order so two writes that each declare several targets
// cannot deadlock each other. Evolve and Link hold exactly one stripe and wait
// for no other while they do, so they cannot take part in a cycle either. (There
// are 256 stripes, so a write naming hundreds of targets holds most of them for
// its commit — about 220 for 500 targets — and every evolve and supersedes link
// on those stripes waits for it.)
func (e *Engine) lockSupersedeTargets(ws [8]byte, declared []storage.Association) (unlock func()) {
	seen := make(map[uint32]struct{}, len(declared))
	stripes := make([]uint32, 0, len(declared))
	for i := range declared {
		s := evolveStripe(ws, declared[i].TargetID)
		if _, dup := seen[s]; !dup {
			seen[s] = struct{}{}
			stripes = append(stripes, s)
		}
	}
	sort.Slice(stripes, func(i, j int) bool { return stripes[i] < stripes[j] })
	for _, s := range stripes {
		e.evolveLocks[s].Lock()
	}
	return func() {
		for i := len(stripes) - 1; i >= 0; i-- {
			e.evolveLocks[stripes[i]].Unlock()
		}
	}
}

// checkSupersedeTarget is what a declared supersession must establish about its
// target, under the target's evolveLock, before anything is written. It is
// Link's own sequence for relation=supersedes, in Link's order: the fork guard
// first — so a target that already has a successor is refused with the error
// that names its current version, even when that target is itself soft-deleted
// (an evolved predecessor is) — then the endpoint rule.
//
// source, when non-zero, is the declaring memory when it already exists:
// re-declaring the supersession it already holds is not a fork.
func (e *Engine) checkSupersedeTarget(ctx context.Context, ws [8]byte, target, source storage.ULID) error {
	if err := e.refuseSupersededEvolveTarget(ctx, ws, target, source); err != nil {
		return err
	}
	eng, err := e.store.GetEngram(ctx, ws, target)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("check supersedes target %s: %w", target.String(), err)
	}
	if err != nil || eng == nil {
		// The class every other bad inline edge target is in (STO-12): the
		// caller named a memory this vault does not hold. 400, not 500.
		return fmt.Errorf("%w: supersedes target %s does not exist in this vault", ErrInvalidID, target.String())
	}
	return linkEndpointStateErr(eng.State)
}

// linkEndpointStateErr is the lifecycle rule for the endpoint of a declared
// link, shared by Link and the inline supersedes path: a soft-deleted or
// archived memory cannot be linked.
func linkEndpointStateErr(state storage.LifecycleState) error {
	switch state {
	case storage.StateSoftDeleted:
		return ErrEngramSoftDeleted
	case storage.StateArchived:
		return ErrEngramArchived
	}
	return nil
}

// commitNewEngramSuperseding writes a NEW engram that declares, inline, that it
// supersedes every target in declared — in ONE atomic batch: the engram (with
// its other inline edges), one guarded supersedes edge per target, and one
// ValidUntil stamp per target.
//
// Order, and what a failure at each step leaves behind:
//
//  1. Lock every target's evolveLock. Nothing is written.
//  2. Check every target (fork guard, endpoint rule). A refusal returns here:
//     nothing is written, for any target.
//  3. Queue the engram, the edges and the stamps into one StoreBatch. A queue-
//     time refusal (a dangling target among the write's OTHER inline edges,
//     STO-12) discards the batch: nothing is written.
//  4. Commit. The stamps are applied to the targets' committed records under
//     their stripe locks and the collision check is repeated under the pairs'
//     guard locks; if either fails the batch is not committed. Otherwise Pebble
//     applies the whole batch atomically: after a crash it is entirely present
//     or entirely absent.
//
// So this write never leaves a supersedes edge without its stamp, or a stamp
// without its edge. The stamp is Link's: it closes an OPEN validity window at
// the moment of the write and never moves a window that is already closed —
// so a target whose valid_until was already set, including one set in the
// future, keeps it (and, in the future case, stays current until then). The
// caller's post-commit bookkeeping (content-hash index, idempotency receipt,
// entity links, FTS) is outside the batch exactly as it is for every other
// write.
func (e *Engine) commitNewEngramSuperseding(ctx context.Context, ws [8]byte, eng *storage.Engram, declared []storage.Association) error {
	unlock := e.lockSupersedeTargets(ws, declared)
	defer unlock()

	for i := range declared {
		if err := e.checkSupersedeTarget(ctx, ws, declared[i].TargetID, storage.ULID{}); err != nil {
			return inlineSupersedesRefused(declared[i].TargetID, err)
		}
	}

	batch := e.store.NewBatch()
	defer batch.Discard()
	if err := batch.WriteEngram(ctx, ws, eng); err != nil {
		return err
	}
	// One instant for every target: the moment this memory replaced them.
	// Link stamps time.Now() too, and never moves an earlier stamp.
	now := time.Now()
	for i := range declared {
		edge := declared[i]
		if err := batch.WriteAssociation(ctx, ws, eng.ID, edge.TargetID, &edge); err != nil {
			return inlineSupersedesRefused(edge.TargetID, err)
		}
		if err := batch.StampValidUntil(ctx, ws, edge.TargetID, now, true); err != nil {
			return inlineSupersedesRefused(edge.TargetID, err)
		}
	}
	return batch.Commit()
}
