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
//   - RepairAssocWeightKeys (assoc_weight_repair.go) — RELOCATES an existing
//     full-weight row from the pre-fix key position to the correct one and
//     deletes the legacy position in the same batch, so it is net-zero on the
//     dangling-row count. It is also unreachable for a hard-deleted endpoint:
//     DeleteEngram's 0x03/0x04 passes scan by prefix across ALL weights, which
//     includes the legacy weight-0.0 position.
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
