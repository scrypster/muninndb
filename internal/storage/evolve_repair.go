package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// GetEvolveRepairMark reads the per-vault evolve entity-link repair watermark
// (0x2B). Returns 0 if the vault has never completed a clean repair pass.
func (ps *PebbleStore) GetEvolveRepairMark(ctx context.Context, ws [8]byte) (uint8, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	val, closer, err := ps.db.Get(keys.EvolveRepairMarkKey(ws))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("get evolve repair mark: %w", err)
	}
	defer closer.Close()
	if len(val) == 0 {
		return 0, nil
	}
	return val[0], nil
}

// SetEvolveRepairMark records that a repair pass at the given version
// completed cleanly over the vault, so later boots can skip the full
// soft-deleted scan.
func (ps *PebbleStore) SetEvolveRepairMark(ctx context.Context, ws [8]byte, version uint8) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ps.db.Set(keys.EvolveRepairMarkKey(ws), []byte{version}, pebble.NoSync)
}

// RepairEvolveEntityCarry atomically writes the full carried entity set for a
// supersede-successor stripped by the pre-fix Evolve path (#622): all 0x20/0x23
// link keys, all 0x21/0x26 relationship keys, and the digest flag, in ONE
// Pebble batch. A successor is repaired completely or not at all — a kill
// mid-repair can never strand a partial link set that the "has any link"
// skip test would then treat as intact forever.
//
// The per-engram stripe lock (casLocks.For(succID)) is held across the
// existence re-check and the batch commit. DeleteEngram holds the same lock
// while removing the same key families, so a repair write cannot land after a
// concurrent hard delete and leave keys pointing at a deleted engram.
//
// The batch is deliberately NOT replicated: the repair is a deterministic
// local backfill derived from data every replica already has, and every node
// running this build performs its own pass at startup. Replicating the writes
// would double-apply them on followers that will also repair.
//
// Post-commit, the mention-count and co-occurrence ledgers are funded for the
// carried links, mirroring Evolve's carry (and DeleteEngram's post-commit
// decrements): a crash between commit and funding leaves counts slightly low
// with the links intact.
//
// Returns false if the successor no longer exists or already has links (both
// re-checked under the lock) — nothing was written.
func (ps *PebbleStore) RepairEvolveEntityCarry(ctx context.Context, ws [8]byte, succID ULID, entityNames []string, rels []RelationshipRecord, digestFlag uint8) (bool, error) {
	if len(entityNames) == 0 {
		return false, nil
	}

	mu := ps.casLocks.For(succID[:])
	mu.Lock()
	defer mu.Unlock()

	// Re-check under the lock: the successor may have been hard-deleted since
	// the scan that selected it, or (defensively) acquired links.
	eng, err := ps.GetEngram(ctx, ws, succID)
	if err != nil {
		return false, fmt.Errorf("repair evolve carry: re-read successor: %w", err)
	}
	if eng == nil {
		return false, nil
	}
	hasLinks := false
	if err := ps.ScanEngramEntities(ctx, ws, succID, func(string) error {
		hasLinks = true
		return errStopRepairScan
	}); err != nil && !errors.Is(err, errStopRepairScan) {
		return false, fmt.Errorf("repair evolve carry: re-check links: %w", err)
	}
	if hasLinks {
		return false, nil
	}

	batch := ps.NewBatch()
	defer batch.Discard()
	pb, ok := batch.(*pebbleStoreBatch)
	if !ok {
		return false, fmt.Errorf("repair evolve carry: unexpected batch type %T", batch)
	}
	for _, name := range entityNames {
		if err := pb.WriteEntityEngramLink(ctx, ws, succID, name); err != nil {
			return false, err
		}
	}
	for _, rec := range rels {
		if err := pb.WriteRelationshipRecord(ctx, ws, succID, rec); err != nil {
			return false, err
		}
	}
	// Digest flag rides the same batch so a healed successor is always marked
	// against re-extraction. Read-modify-write is safe under the stripe lock.
	raw, err := ps.getDigestFlagsRaw([16]byte(succID))
	if err != nil && !errors.Is(err, pebble.ErrNotFound) {
		return false, fmt.Errorf("repair evolve carry: read digest flags: %w", err)
	}
	if err := pb.batch.Set(keys.DigestFlagsKey([16]byte(succID)), []byte{raw | digestFlag}, nil); err != nil {
		return false, fmt.Errorf("repair evolve carry: set digest flag: %w", err)
	}
	if err := batch.Commit(); err != nil {
		return false, fmt.Errorf("repair evolve carry: commit: %w", err)
	}

	// Fund the ledgers for the carried links (see Evolve's carry for the full
	// accounting argument; DeleteEngram will decrement these when the repaired
	// successor is eventually deleted). Funding failures warn and continue,
	// matching Evolve's own funding: once the links are committed the repair
	// cannot be retried (the has-links skip fires on the next boot), so
	// aborting the round would cost the rest of the sweep without ever
	// recovering these counts — counts are best-effort across crashes on both
	// sides of the ledger.
	for _, name := range entityNames {
		if err := ps.IncrementEntityMentionCount(ctx, name); err != nil {
			slog.Warn("storage: repair evolve carry: failed to fund mention count", "entity", name, "engram", succID.String(), "err", err)
		}
	}
	// Cap mirrors DeleteEngram's maxCoOccurrenceEntities.
	const maxCoOccurrenceEntities = 50
	coNames := entityNames
	if len(coNames) > maxCoOccurrenceEntities {
		coNames = coNames[:maxCoOccurrenceEntities]
	}
	for i := 0; i < len(coNames); i++ {
		for j := i + 1; j < len(coNames); j++ {
			if err := ps.IncrementEntityCoOccurrence(ctx, ws, coNames[i], coNames[j]); err != nil {
				slog.Warn("storage: repair evolve carry: failed to fund co-occurrence", "a", coNames[i], "b", coNames[j], "engram", succID.String(), "err", err)
			}
		}
	}
	return true, nil
}

// errStopRepairScan ends the has-any-link scan early without signalling error.
var errStopRepairScan = errors.New("stop repair scan")
