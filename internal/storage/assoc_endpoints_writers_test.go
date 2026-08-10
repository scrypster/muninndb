package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// The STO-12 writer set, enumerated FROM THE KEY BUILDERS rather than from
// prose or method names.
//
// `grep -e AssocFwdKey -e AssocRevKey -e AssocWeightIndexKey -e ArchiveAssocKey`
// over non-test Go files is the only enumeration that is honest here, because
// four of the writers do not have "Association" anywhere in their name: the
// three inline-`Associations` loops inside the engram writers, and the singular
// UpdateAssocWeight. The first version of this guard was written from the
// method names and missed all four — WriteEngram, WriteEngramBatch,
// pebbleStoreBatch.WriteEngram and UpdateAssocWeight each Set 0x03/0x04/0x14
// directly, and the first three are reachable by any ordinary client over REST,
// gRPC, MBP and the embedded library via WriteRequest.Associations.
//
// This table is the machine check for the whole CREATOR set. Keep it exhaustive:
// if the grep grows a row, this table grows a case.
//
// The non-creators the same grep returns, and why they are not here:
//
//   - DeleteEngram (engram.go) and deleteLegacyFullWeightKeys — Delete only.
//   - DecayAssocWeights (association.go ~800) — rewrites/archives rows it just
//     read from the live index; it cannot conjure a pair that was not there.
//   - RepairLegacyFullWeightAssocKeys (assoc_weight_repair.go) — RELOCATES an
//     existing full-weight row from the pre-fix key position to the correct one
//     and deletes the legacy position in the same batch, so it is net-zero on
//     the dangling-row count.
//
//     Net-zero is the WHOLE of the exemption, and the earlier version of this
//     note added a second, false supporting claim: that the pass is unreachable
//     for a hard-deleted endpoint because "DeleteEngram's 0x03/0x04 passes scan
//     by prefix across ALL weights". They did not. Both scans bounded themselves
//     with an appended 0xFF sentinel (STO-11), which sorts at or below every key
//     whose weight complement starts 0xFF — that is every weight at or below
//     ~1/256, and complement 0xFFFFFFFF, which is exactly the legacy position.
//     So the cascade left the legacy row behind, this pass scans with a correct
//     carry-propagating bound and therefore SAW it, and relocating it produced a
//     live, correctly-positioned, fully-reachable dangling edge. Net-zero held;
//     "unreachable" was the opposite of true, and the pass was a creator.
//
//     Both halves are now closed and both are pinned: the cascade bounds use
//     keys.PrefixUpperBound, and flushChunk's live re-validation additionally
//     requires both endpoints to have a 0x01 record — needed on its own, because
//     the vaults this pass exists for were hard-deleted by a PRE-FIX binary
//     whose cascade could not reach the legacy position either. See
//     TestSTO12_DeleteEngramCascadeReachesEveryWeightPosition and
//     TestSTO12_LegacyFullWeightRepairNeverCreatesADanglingEdge.
//   - index/adjacency.Graph.WriteAssociation — a parallel 0x03/0x04 writer with
//     no non-test importer in the tree at all (`grep -rn index/adjacency`
//     returns only its own test). Left alone deliberately rather than guarded:
//     adding a Pebble read to dead code buys nothing, and if it is ever wired up
//     it must route through PebbleStore anyway. Recorded here so the next
//     enumeration does not have to rediscover it.

// assertNoAssocRows fails if any of the three live association keys exist for
// the pair. A refused write must be a no-op, not a partial one.
func assertNoAssocRows(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) {
	t.Helper()
	for label, k := range map[string][]byte{
		"0x03 fwd":          keys.AssocFwdKey(ws, [16]byte(src), 0.8, [16]byte(dst)),
		"0x04 rev":          keys.AssocRevKey(ws, [16]byte(dst), 0.8, [16]byte(src)),
		"0x14 weight-index": keys.AssocWeightIndexKey(ws, [16]byte(src), [16]byte(dst)),
	} {
		if _, closer, err := ps.db.Get(k); err == nil {
			_ = closer.Close()
			t.Errorf("refused write still left a %s row", label)
		}
	}
}

// TestSTO12_EveryAssociationCreatorRefusesDeadEndpoints is the enumerated
// machine check over the creator set.
func TestSTO12_EveryAssociationCreatorRefusesDeadEndpoints(t *testing.T) {
	ctx := context.Background()

	// Each case writes ONE edge whose target has no 0x01 record and must be
	// refused with ErrDanglingEndpoint, leaving no row behind.
	cases := []struct {
		name string
		// write drives one creator. src is a live engram; dst has no engram.
		write func(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) error
	}{
		{
			// The primitive. Already guarded before this round; kept in the
			// table so the enumeration is complete in one place.
			name: "PebbleStore.WriteAssociation",
			write: func(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) error {
				return ps.WriteAssociation(ctx, ws, src, dst, danglingProbeAssoc(dst))
			},
		},
		{
			// impl.go: the inline-Associations loop. Reachable from every
			// transport as WriteRequest.Associations.
			name: "PebbleStore.WriteEngram inline Associations",
			write: func(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) error {
				_, err := ps.WriteEngram(ctx, ws, &Engram{
					ID:           src,
					Concept:      "inline assoc probe",
					Content:      "an engram naming a target that no longer exists",
					Associations: []Association{*danglingProbeAssoc(dst)},
				})
				return err
			},
		},
		{
			// impl.go: the same loop inside the batch writer, which is what
			// Engine.WriteBatch (and therefore REST /engrams/batch, the gRPC
			// batch RPC and muninn_remember_batch) actually calls.
			name: "PebbleStore.WriteEngramBatch inline Associations",
			write: func(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) error {
				_, errs := ps.WriteEngramBatch(ctx, []EngramBatchItem{{
					WSPrefix: ws,
					Engram: &Engram{
						ID:           src,
						Concept:      "inline assoc probe",
						Content:      "a batched engram naming a target that no longer exists",
						Associations: []Association{*danglingProbeAssoc(dst)},
					},
				}})
				return errs[0]
			},
		},
		{
			// batch.go: the same loop again inside the StoreBatch handle.
			name: "pebbleStoreBatch.WriteEngram inline Associations",
			write: func(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) error {
				b := ps.NewBatch()
				defer b.Discard()
				if err := b.WriteEngram(ctx, ws, &Engram{
					ID:           src,
					Concept:      "inline assoc probe",
					Content:      "a queued engram naming a target that no longer exists",
					Associations: []Association{*danglingProbeAssoc(dst)},
				}); err != nil {
					return err
				}
				return b.Commit()
			},
		},
		{
			// batch.go: the StoreBatch association writer. Guarded before this
			// round; kept for completeness.
			name: "pebbleStoreBatch.WriteAssociation",
			write: func(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) error {
				b := ps.NewBatch()
				defer b.Discard()
				if err := b.WriteAssociation(ctx, ws, src, dst, danglingProbeAssoc(dst)); err != nil {
					return err
				}
				return b.Commit()
			},
		},
		{
			// association.go: the SINGULAR weight update. Same shape as the
			// batch form measured in #803 — read ABSENCE returns (zero, nil)
			// and falls straight through into the Set, so this creates an edge
			// out of nothing. Live callers: consolidation phase 5 and the
			// Hebbian store adapter.
			name: "PebbleStore.UpdateAssocWeight",
			write: func(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) error {
				return ps.UpdateAssocWeight(ctx, ws, src, dst, 0.8, 1)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := newTestStore(t)
			ws := ps.VaultPrefix("sto12-writers")
			src, dst := NewULID(), NewULID()
			seedEndpoints(t, ps, ws, src)
			// dst deliberately has no 0x01 record: the state a client reaches
			// by holding an ID across a hard delete.

			err := tc.write(t, ps, ws, src, dst)
			if !errors.Is(err, ErrDanglingEndpoint) {
				t.Fatalf("want ErrDanglingEndpoint, got %v", err)
			}
			assertNoAssocRows(t, ps, ws, src, dst)
			if w, _ := ps.GetAssocWeight(ctx, ws, src, dst); w != 0 {
				t.Fatalf("an edge was created to an engram-less ULID: w=%v", w)
			}
		})
	}
}

func danglingProbeAssoc(dst ULID) *Association {
	return &Association{
		TargetID:   dst,
		RelType:    RelRelatesTo,
		Weight:     0.8,
		Confidence: 1,
		CreatedAt:  time.Now(),
	}
}

// TestSTO12_EngramWritersAcceptSelfAndSameCallEndpoints is the data-loss guard
// on the three inline-Associations guards.
//
// The engram being written is itself an endpoint of every one of its inline
// associations, and its own 0x01 key is Set in the SAME uncommitted batch — so
// a naive "both endpoints must be readable from Pebble" predicate would reject
// every single engram that carries inline associations, which is the entire
// feature. WriteEngramBatch must additionally accept a target that is another
// engram in the same call, and pebbleStoreBatch one queued earlier in the batch.
func TestSTO12_EngramWritersAcceptSelfAndSameCallEndpoints(t *testing.T) {
	ctx := context.Background()

	t.Run("WriteEngram: the new engram is its own live source", func(t *testing.T) {
		ps := newTestStore(t)
		ws := ps.VaultPrefix("sto12-self")
		target := NewULID()
		seedEndpoints(t, ps, ws, target)

		fresh := NewULID()
		if _, err := ps.WriteEngram(ctx, ws, &Engram{
			ID:           fresh,
			Concept:      "brand new",
			Content:      "carries an inline association to an existing engram",
			Associations: []Association{*danglingProbeAssoc(target)},
		}); err != nil {
			t.Fatalf("a new engram with a live inline target must be written, got %v", err)
		}
		if w, _ := ps.GetAssocWeight(ctx, ws, fresh, target); w == 0 {
			t.Fatal("the inline association was dropped")
		}
	})

	t.Run("WriteEngramBatch: a target elsewhere in the same call is live", func(t *testing.T) {
		ps := newTestStore(t)
		ws := ps.VaultPrefix("sto12-samecall")
		first, second := NewULID(), NewULID()

		// Deliberately the HARDER order: the engram that names the target is
		// queued FIRST. WriteEngramBatch sees the whole slice up front, so its
		// membership check is order-independent by construction.
		_, errs := ps.WriteEngramBatch(ctx, []EngramBatchItem{
			{WSPrefix: ws, Engram: &Engram{
				ID: first, Concept: "referrer", Content: "names its sibling",
				Associations: []Association{*danglingProbeAssoc(second)},
			}},
			{WSPrefix: ws, Engram: &Engram{ID: second, Concept: "sibling", Content: "the target"}},
		})
		for i, err := range errs {
			if err != nil {
				t.Fatalf("item %d must commit, got %v", i, err)
			}
		}
		if w, _ := ps.GetAssocWeight(ctx, ws, first, second); w == 0 {
			t.Fatal("the intra-batch association was dropped")
		}
		// The acceptance is only sound if the sibling ACTUALLY committed.
		// Asserting the referrer's nil error alone would stay green even if the
		// sibling were skipped, which is precisely the hole
		// TestSTO12_BatchSiblingThatFailsItsOwnValidationIsNotALiveEndpoint pins.
		if eng, err := ps.GetEngram(ctx, ws, second); err != nil || eng == nil {
			t.Fatalf("the sibling accepted as a live endpoint did not commit: %v", err)
		}
	})

	t.Run("pebbleStoreBatch.WriteEngram: an engram queued earlier is live", func(t *testing.T) {
		ps := newTestStore(t)
		ws := ps.VaultPrefix("sto12-queued")
		parent, child := NewULID(), NewULID()

		b := ps.NewBatch()
		defer b.Discard()
		if err := b.WriteEngram(ctx, ws, &Engram{ID: parent, Concept: "parent", Content: "parent content"}); err != nil {
			t.Fatalf("queue parent: %v", err)
		}
		if err := b.WriteEngram(ctx, ws, &Engram{
			ID: child, Concept: "child", Content: "child content",
			Associations: []Association{*danglingProbeAssoc(parent)},
		}); err != nil {
			t.Fatalf("an inline target queued earlier in the same batch must be accepted, got %v", err)
		}
		if err := b.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if w, _ := ps.GetAssocWeight(ctx, ws, child, parent); w == 0 {
			t.Fatal("the same-batch inline association was dropped")
		}
	})
}

// TestSTO12_BatchSiblingThatFailsItsOwnValidationIsNotALiveEndpoint closes the
// gap between WriteEngramBatch's sameCall set and its per-item validations.
//
// sameCall is built from the WHOLE slice up front — deliberately, so the guard
// is order-independent (see TestSTO12_BatchAssociationGuardIsQueueOrderDependent
// for why pebbleStoreBatch cannot do the same). But the per-item validations run
// afterwards inside the loop and each `continue`s without queueing that item's
// 0x01 key, so the same call could promise a sibling is live and then refuse it.
// A referrer naming that sibling committed its three association rows with a nil
// error, pointing at an engram that never existed.
//
// Latent rather than live today — Engine.WriteBatch builds each storage.Engram
// fresh with no ID, so sameCall is always empty on every transport path — but it
// is a hole in the guard on exactly the axis this invariant covers, and it would
// open the moment any caller assigns IDs.
//
// The fix must NOT be to accumulate sameCall as the loop walks the slice: that
// reintroduces the queue-order dependence and turns the referrer-first
// acceptance subtest above red. Instead the referrer's edges to a refused
// sibling are Deleted into the same batch after the loop; Pebble applies in
// batch order, so the Delete wins over the Set regardless of item order.
//
// The predicate for that post-loop Delete is ENGRAM EXISTENCE, not the batch
// outcome. A refused item is not the same thing as a dead endpoint: a refused
// UPDATE leaves its 0x01 record exactly where it was, so the sibling is still
// live and the referrer's edge to it is legitimate. Keying the Delete on
// `errs[i] != nil` alone destroys that live edge — the same reachability axis as
// the hole this block closes, but the wrong direction of loss: the hole leaks a
// dangling row, which the deferred integrity pass can repair, while this
// destroys an edge nothing can recover. The third case below is that case.
//
// The FOURTH case is the half `engramExists` alone cannot see. When the same id
// appears twice in one call — once committing, once refused — the successful
// occurrence's 0x01 Set is sitting in the UNCOMMITTED batch, so the DB read
// `engramExists` performs returns false and the refused occurrence is classified
// dead. The referrer's three rows to a sibling that is about to commit are then
// Deleted, in the same batch, after the Set that made it live. That is the
// `committedIDs` set's entire job, and nothing else in the package covers it:
// neutralising it leaves the whole of internal/storage green.
//
// NOTE for anyone editing the invalid-tag fixtures: the tag MUST contain a ':'.
// ValidateRawTagValue splits on ':' and returns nil when there is no separator,
// so a bare "bad\x00tag" is ACCEPTED and the sibling commits — the refusal this
// whole test is built on never happens. The `errs[refused] == nil` precondition
// below is what keeps that a loud failure instead of a green run proving
// nothing, which is exactly why it is a t.Fatal and not an implicit assumption.
func TestSTO12_BatchSiblingThatFailsItsOwnValidationIsNotALiveEndpoint(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		sibling func(id ULID, deadTarget ULID) *Engram
		// preexisting seeds the sibling as a LIVE engram before the batch, so
		// the refusal is a refused update rather than a refused create.
		preexisting bool
		// committingTwin inserts a SECOND item carrying the same id as the
		// refused sibling, which passes every validation and commits. The
		// sibling ends the call live, but only via an uncommitted Set that no
		// DB read can observe while the guard runs.
		committingTwin bool
	}{
		{
			// Fails at the tag validation (impl.go), which continues without
			// queueing anything for the item.
			name: "sibling rejected by raw-tag validation",
			sibling: func(id ULID, _ ULID) *Engram {
				return &Engram{ID: id, Concept: "sibling", Content: "the target",
					Tags: []string{"project:plan\x00ning"}}
			},
		},
		{
			// Fails at STO-12 itself: the sibling's own inline target is dead.
			name: "sibling rejected by its own STO-12 check",
			sibling: func(id ULID, deadTarget ULID) *Engram {
				return &Engram{ID: id, Concept: "sibling", Content: "the target",
					Associations: []Association{*danglingProbeAssoc(deadTarget)}}
			},
		},
		{
			// The other direction. The sibling is ALREADY a live engram and the
			// batch merely tries to rewrite it; the rewrite is refused, the
			// existing 0x01 record is untouched, and the referrer's edge points
			// at a perfectly live engram. Deleting it is unrecoverable data loss.
			name:        "refused sibling that is ALREADY a live engram keeps the referrer's edge",
			preexisting: true,
			sibling: func(id ULID, _ ULID) *Engram {
				return &Engram{ID: id, Concept: "sibling", Content: "a refused rewrite of a live engram",
					Tags: []string{"project:plan\x00ning"}}
			},
		},
		{
			// The committed-sibling half. The same id is queued twice: a valid
			// item that commits, and a refused one. engramExists cannot see the
			// valid item's 0x01 — it is an uncommitted batch Set — so without
			// committedIDs the refused occurrence is classified dead and the
			// referrer's edge to a sibling that DOES commit is destroyed.
			name:           "the same id also committing elsewhere in the call keeps the referrer's edge",
			committingTwin: true,
			sibling: func(id ULID, _ ULID) *Engram {
				return &Engram{ID: id, Concept: "sibling", Content: "a refused duplicate of a committing item",
					Tags: []string{"project:plan\x00ning"}}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := newTestStore(t)
			ws := ps.VaultPrefix("sto12-batch-sibling")
			referrer, sibling, dead := NewULID(), NewULID(), NewULID()
			if tc.preexisting {
				seedEndpoints(t, ps, ws, sibling)
			}

			items := []EngramBatchItem{
				{WSPrefix: ws, Engram: &Engram{
					ID: referrer, Concept: "referrer", Content: "names its sibling",
					Associations: []Association{*danglingProbeAssoc(sibling)},
				}},
			}
			twin := -1
			if tc.committingTwin {
				twin = len(items)
				items = append(items, EngramBatchItem{WSPrefix: ws, Engram: &Engram{
					ID: sibling, Concept: "sibling", Content: "the occurrence that commits",
				}})
			}
			refused := len(items)
			items = append(items, EngramBatchItem{WSPrefix: ws, Engram: tc.sibling(sibling, dead)})

			_, errs := ps.WriteEngramBatch(ctx, items)

			if errs[refused] == nil {
				t.Fatal("precondition: the sibling was expected to be refused")
			}
			if errs[0] != nil {
				t.Fatalf("precondition: the referrer was expected to commit, got %v", errs[0])
			}
			if twin >= 0 && errs[twin] != nil {
				t.Fatalf("precondition: the committing twin was expected to commit, got %v", errs[twin])
			}

			// Live either because it was already there (a refused update) or
			// because a twin occurrence in this very call committed it.
			wantSiblingLive := tc.preexisting || tc.committingTwin

			eng, err := ps.GetEngram(ctx, ws, sibling)
			siblingLive := err == nil && eng != nil
			if siblingLive != wantSiblingLive {
				t.Fatalf("precondition: sibling live = %v, want %v", siblingLive, wantSiblingLive)
			}

			w, _ := ps.GetAssocWeight(ctx, ws, referrer, sibling)
			if !wantSiblingLive {
				assertNoAssocRows(t, ps, ws, referrer, sibling)
				if w != 0 {
					t.Errorf("STO-12: a live edge (w=%v) from the committed referrer points at an "+
						"engram that never committed", w)
				}
				return
			}
			// The sibling is live, so every one of the three rows is legitimate.
			for label, k := range map[string][]byte{
				"0x03 fwd":          keys.AssocFwdKey(ws, [16]byte(referrer), 0.8, [16]byte(sibling)),
				"0x04 rev":          keys.AssocRevKey(ws, [16]byte(sibling), 0.8, [16]byte(referrer)),
				"0x14 weight-index": keys.AssocWeightIndexKey(ws, [16]byte(referrer), [16]byte(sibling)),
			} {
				if _, closer, gErr := ps.db.Get(k); gErr != nil {
					t.Errorf("STO-12: the post-loop delete destroyed the %s row of a LIVE edge between "+
						"two LIVE engrams, because it keyed on the batch outcome instead of engram existence", label)
				} else {
					_ = closer.Close()
				}
			}
			if w == 0 {
				t.Error("DATA LOSS: GetAssocWeight = 0 — a legitimate edge to a still-live engram " +
					"was deleted because that engram's unrelated rewrite was refused")
			}
		})
	}
}

// TestSTO12_GuardFailsOpenButNotSilently pins the OTHER half of the fail-open
// decision.
//
// Failing open on a read fault is right and stays: #809 refuses on a read
// failure because proceeding fabricates metadata over live data, while this
// guard permits because refusing destroys a live edge that can never be
// recovered — opposite loss directions, the same doctrine. But a bare
// fail-open reverts to pre-#803 behaviour under a real Pebble read fault with
// nothing whatsoever in the logs, which is the silently-wrong class this
// invariant exists to close. #809 set the standard by reporting through
// AssocBatchSkipError with named indices; this matches it with a counted,
// rate-limited WARN.
//
// Uses the readFault seam rather than a corrupted .sst because the behaviour
// under test is precisely "the read fails while the write still succeeds",
// which closing the DB or deleting the key cannot reproduce.
func TestSTO12_GuardFailsOpenButNotSilently(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("sto12-failopen")

	src, dst := NewULID(), NewULID()
	seedEndpoints(t, ps, ws, src)
	// dst has no engram, so a HEALTHY guard would refuse this write.

	before := ps.GuardReadFaults()
	ps.readFault = failReadsWithPrefix(prefix.Engram)
	err := ps.WriteAssociation(ctx, ws, src, dst, danglingProbeAssoc(dst))
	ps.readFault = nil

	if err != nil {
		t.Fatalf("the guard must FAIL OPEN on a read fault, not refuse a possibly-live edge: %v", err)
	}
	if got := ps.GuardReadFaults() - before; got == 0 {
		t.Fatal("the guard failed open and did not count it — a Pebble read fault silently reverts " +
			"the whole invariant to pre-#803 behaviour with nothing in the logs")
	}
}

// TestSTO12_BatchAssociationGuardIsQueueOrderDependent pins a documented,
// deliberately-unfixed limitation rather than a behaviour anyone should rely on.
//
// pebbleStoreBatch.checkEndpointsLive scans only the items ALREADY queued, so
// the same-batch exception requires the engram to be queued BEFORE its edge.
// Both in-tree callers (RememberChild, Evolve) do that, so this is not a live
// defect — but it is invisible until the next caller queues the edge first and
// gets a hard error on a perfectly valid commit. The doc comment on
// WriteAssociation states the requirement; this is what enforces that the
// requirement stays TRUE, so a future reordering of the batch API surfaces here
// instead of in a caller.
//
// The alternative — deferring the check to Commit and Delete-ing the queued
// rows for endpoints that never materialised — was considered and rejected: it
// moves a caller error from the call that made it to a commit that may carry
// unrelated work, for a caller that does not yet exist.
func TestSTO12_BatchAssociationGuardIsQueueOrderDependent(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("sto12-order")

	parent, child := NewULID(), NewULID()
	b := ps.NewBatch()
	defer b.Discard()

	// Edge first, engram second — the unsupported order.
	err := b.WriteAssociation(ctx, ws, child, parent, danglingProbeAssoc(parent))
	if !errors.Is(err, ErrDanglingEndpoint) {
		t.Fatalf("queue-order contract changed: an edge queued BEFORE its engram is expected to be "+
			"refused (both in-tree callers queue the engram first); got %v", err)
	}
}
