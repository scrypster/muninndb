package cognitive

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// contraMat is the precomputed contradiction boolean matrix.
// contraMat[relA][relB] = true means those two relation types contradict each other.
var contraMat [64][64]bool

func init() {
	// RelSupports(1) contradicts RelContradicts(2)
	setContra(1, 2)
	// RelDependsOn(3) contradicts RelSupersedes(4) in some cases
	// RelPrecededBy(8) contradicts RelFollowedBy(9)
	setContra(8, 9)
}

func setContra(a, b uint16) {
	if a < 64 && b < 64 {
		contraMat[a][b] = true
		contraMat[b][a] = true
	}
}

// ContradictionSeverity returns the severity of a contradiction between two rel types.
func ContradictionSeverity(relA, relB uint16) float64 {
	if relA < 64 && relB < 64 && contraMat[relA][relB] {
		if (relA == 1 && relB == 2) || (relA == 2 && relB == 1) {
			return 1.0 // direct negation
		}
		return 0.9 // incompatible relation types
	}
	return 0.0
}

// ContradictionStore is the storage interface for contradiction detection.
// Only FlagContradiction is required; detection logic operates over the
// associations supplied with each ContradictItem.
type ContradictionStore interface {
	FlagContradiction(ctx context.Context, ws [8]byte, engramA, engramB [16]byte) error
}

// ContradictAssoc is an association used for contradiction checking.
type ContradictAssoc struct {
	EngramID   [16]byte
	TargetID   [16]byte
	TargetHash uint32
	RelType    uint16
}

// ContradictionEvent is emitted when a contradiction is found.
type ContradictionEvent struct {
	VaultID  uint32
	EngramA  [16]byte
	EngramB  [16]byte
	Severity float64
	Type     string
}

// ContradictItem is submitted to the contradiction worker.
type ContradictItem struct {
	WS           [8]byte
	EngramID     [16]byte
	ConceptHash  uint32
	Associations []ContradictAssoc
	OnFound      func(ContradictionEvent)
}

// ContradictWorker detects contradictions between engrams.
type ContradictWorker struct {
	*Worker[ContradictItem]
	store ContradictionStore
}

// NewContradictWorker creates a new contradiction detection worker.
func NewContradictWorker(store ContradictionStore) *ContradictWorker {
	cw := &ContradictWorker{store: store}
	cw.Worker = NewWorker[ContradictItem](
		2000, 50, 30*time.Second,
		cw.processBatch,
	)
	return cw
}

// processBatch checks for contradictions within each item's own association set.
// Two associations on the same engram contradict when their RelTypes are
// semantically incompatible (e.g. Supports vs Contradicts), or when the same
// RelType points at targets with different concept hashes (conflicting conclusions).
func (cw *ContradictWorker) processBatch(ctx context.Context, batch []ContradictItem) error {
	for _, item := range batch {
		n := len(item.Associations)
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				a, b := item.Associations[i], item.Associations[j]
				severity := ContradictionSeverity(a.RelType, b.RelType)
				if severity <= 0 && a.RelType == b.RelType && a.TargetHash != b.TargetHash {
					// Same relation type pointing at different-concept targets.
					severity = 0.8
				}
				if severity > 0 {
					if err := cw.store.FlagContradiction(ctx, item.WS, a.TargetID, b.TargetID); err != nil {
						slog.Error("contradict: failed to flag contradiction",
							"ws", fmt.Sprintf("%x", item.WS),
							"engram_a", fmt.Sprintf("%x", a.TargetID),
							"engram_b", fmt.Sprintf("%x", b.TargetID),
							"error", err)
					}
					if item.OnFound != nil {
						item.OnFound(ContradictionEvent{
							EngramA:  a.TargetID,
							EngramB:  b.TargetID,
							Severity: severity,
							Type:     "incompatible_relations",
						})
					}
				}
			}
		}
	}
	return nil
}
