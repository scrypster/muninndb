package replication

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// HebbianStoreWriter allows writing individual association weights during reconciliation.
type HebbianStoreWriter interface {
	UpdateAssocWeight(ctx context.Context, id [16]byte, weight float64) error
}

// ReconcileResult holds the outcome of a reconciliation run.
type ReconcileResult struct {
	StartedAt        time.Time
	CompletedAt      time.Time
	EngramsChecked   int
	EngramsDivergent int
	WeightsSynced    int
	NodeResults      map[string]NodeReconcileResult
}

// NodeReconcileResult holds per-node reconciliation outcome.
type NodeReconcileResult struct {
	NodeID    string
	Divergent int
	Synced    int
	Failed    int
	Error     string
}

// reconTimeout is the per-Lobe timeout for reconciliation probe/reply/ack exchanges.
const reconTimeout = 5 * time.Second

// reconDivergenceThreshold is the minimum difference to consider a weight divergent.
const reconDivergenceThreshold = 1e-6

// ErrReconciliationInProgress is returned when Run() is called while another reconciliation is active.
var ErrReconciliationInProgress = errors.New("reconciliation already in progress")

// Reconciler runs post-partition cognitive reconciliation.
type Reconciler struct {
	sampler    HebbianSampler
	store      HebbianStoreWriter
	coord      *ClusterCoordinator
	topK       int // number of engrams to check (default 500)
	mu         sync.Mutex
	lastResult ReconcileResult
	running    atomic.Bool

	// pending tracks in-flight reconciliation rounds: requestID -> channels.
	pendingMu    sync.Mutex
	pendingReply map[string]chan mbp.ReconReplyMsg
	pendingAck   map[string]chan mbp.ReconAckMsg
}

// NewReconciler creates a Reconciler.
func NewReconciler(sampler HebbianSampler, store HebbianStoreWriter, coord *ClusterCoordinator) *Reconciler {
	return &Reconciler{
		sampler:      sampler,
		store:        store,
		coord:        coord,
		topK:         500,
		pendingReply: make(map[string]chan mbp.ReconReplyMsg),
		pendingAck:   make(map[string]chan mbp.ReconAckMsg),
	}
}

// LastResult returns the most recently completed ReconcileResult (thread-safe).
func (r *Reconciler) LastResult() ReconcileResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastResult
}

// Run executes one reconciliation round against the given Lobe node IDs.
// Only one Run may be active at a time; concurrent calls return ErrReconciliationInProgress.
func (r *Reconciler) Run(ctx context.Context, lobeNodeIDs []string) (ReconcileResult, error) {
	if !r.running.CompareAndSwap(false, true) {
		return ReconcileResult{}, ErrReconciliationInProgress
	}
	defer r.running.Store(false)

	result := ReconcileResult{
		StartedAt:   time.Now(),
		NodeResults: make(map[string]NodeReconcileResult),
	}

	if r.sampler == nil || len(lobeNodeIDs) == 0 {
		result.CompletedAt = time.Now()
		r.mu.Lock()
		r.lastResult = result
		r.mu.Unlock()
		return result, nil
	}

	// Step 1: Sample top-K keys from local store.
	keys, err := r.sampler.SampleKeys(r.topK)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("reconcile: sample keys: %w", err)
	}
	if len(keys) == 0 {
		result.CompletedAt = time.Now()
		r.mu.Lock()
		r.lastResult = result
		r.mu.Unlock()
		return result, nil
	}

	// Step 2: Get Cortex's weights for sampled keys.
	cortexWeights, err := r.sampler.GetAssocWeightsForKeys(keys)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("reconcile: get cortex weights: %w", err)
	}

	result.EngramsChecked = len(keys)

	// Build parallel weight array for the probe message.
	weightArr := make([]float64, len(keys))
	for i, k := range keys {
		weightArr[i] = cortexWeights[k] // 0.0 if not found
	}

	rid := uuid.New().String()

	// Step 3: Send TypeReconProbe to each Lobe and collect replies.
	probeMsg := mbp.ReconProbeMsg{
		RequestID: rid,
		Keys:      keys,
		Weights:   weightArr,
	}
	probePayload, err := msgpack.Marshal(probeMsg)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("reconcile: marshal probe: %w", err)
	}

	// Register reply channel.
	replyCh := make(chan mbp.ReconReplyMsg, len(lobeNodeIDs))
	r.pendingMu.Lock()
	r.pendingReply[rid] = replyCh
	r.pendingMu.Unlock()
	defer func() {
		r.pendingMu.Lock()
		delete(r.pendingReply, rid)
		r.pendingMu.Unlock()
	}()

	// Send probes to all lobes.
	for _, lobeID := range lobeNodeIDs {
		peer, ok := r.coord.mgr.GetPeer(lobeID)
		if !ok {
			result.NodeResults[lobeID] = NodeReconcileResult{
				NodeID: lobeID,
				Error:  "peer not found",
			}
			continue
		}
		if err := peer.Send(mbp.TypeReconProbe, probePayload); err != nil {
			result.NodeResults[lobeID] = NodeReconcileResult{
				NodeID: lobeID,
				Error:  fmt.Sprintf("send probe: %v", err),
			}
		}
	}

	// Step 4: Wait for replies with timeout.
	replyDeadline := time.Now().Add(reconTimeout)
	replyCtx, replyCancel := context.WithDeadline(ctx, replyDeadline)
	defer replyCancel()

	replies := make(map[string]mbp.ReconReplyMsg)
	expectedReplies := 0
	for _, lobeID := range lobeNodeIDs {
		if _, hasError := result.NodeResults[lobeID]; !hasError {
			expectedReplies++
		}
	}

	for len(replies) < expectedReplies {
		select {
		case <-replyCtx.Done():
			goto reconcile
		case reply := <-replyCh:
			replies[reply.NodeID] = reply
		}
	}

reconcile:
	// Step 5: Compare weights and identify divergent keys per Lobe.
	totalDivergent := 0

	for _, lobeID := range lobeNodeIDs {
		if _, hasError := result.NodeResults[lobeID]; hasError {
			continue
		}

		reply, ok := replies[lobeID]
		if !ok {
			result.NodeResults[lobeID] = NodeReconcileResult{
				NodeID: lobeID,
				Error:  "timeout waiting for reply",
			}
			continue
		}

		// Find divergent keys.
		var divergentKeys [][16]byte
		var divergentWeights []float64

		for i, k := range keys {
			cortexW := weightArr[i]
			lobeW := 0.0
			if i < len(reply.Weights) {
				lobeW = reply.Weights[i]
			}
			if math.Abs(cortexW-lobeW) > reconDivergenceThreshold {
				divergentKeys = append(divergentKeys, k)
				divergentWeights = append(divergentWeights, cortexW)
			}
		}

		nr := NodeReconcileResult{
			NodeID:    lobeID,
			Divergent: len(divergentKeys),
		}
		totalDivergent += len(divergentKeys)

		if len(divergentKeys) == 0 {
			result.NodeResults[lobeID] = nr
			continue
		}

		// Step 6: Send TypeReconSync with corrected weights.
		syncRID := uuid.New().String()
		syncMsg := mbp.ReconSyncMsg{
			RequestID: syncRID,
			Keys:      divergentKeys,
			Weights:   divergentWeights,
		}
		syncPayload, err := msgpack.Marshal(syncMsg)
		if err != nil {
			nr.Error = fmt.Sprintf("marshal sync: %v", err)
			nr.Failed = len(divergentKeys)
			result.NodeResults[lobeID] = nr
			continue
		}

		// Register ack channel.
		ackCh := make(chan mbp.ReconAckMsg, 1)
		r.pendingMu.Lock()
		r.pendingAck[syncRID] = ackCh
		r.pendingMu.Unlock()

		peer, ok := r.coord.mgr.GetPeer(lobeID)
		if !ok {
			nr.Error = "peer not found for sync"
			nr.Failed = len(divergentKeys)
			r.pendingMu.Lock()
			delete(r.pendingAck, syncRID)
			r.pendingMu.Unlock()
			result.NodeResults[lobeID] = nr
			continue
		}

		if err := peer.Send(mbp.TypeReconSync, syncPayload); err != nil {
			nr.Error = fmt.Sprintf("send sync: %v", err)
			nr.Failed = len(divergentKeys)
			r.pendingMu.Lock()
			delete(r.pendingAck, syncRID)
			r.pendingMu.Unlock()
			result.NodeResults[lobeID] = nr
			continue
		}

		// Wait for ack.
		ackDeadline := time.NewTimer(reconTimeout)
		select {
		case ack := <-ackCh:
			applied := ack.Applied
			if applied > len(divergentKeys) {
				applied = len(divergentKeys)
			}
			nr.Synced = applied
			nr.Failed = len(divergentKeys) - applied
		case <-ackDeadline.C:
			nr.Error = "timeout waiting for sync ack"
			nr.Failed = len(divergentKeys)
		case <-ctx.Done():
			nr.Error = "context cancelled"
			nr.Failed = len(divergentKeys)
		}
		ackDeadline.Stop()

		r.pendingMu.Lock()
		delete(r.pendingAck, syncRID)
		r.pendingMu.Unlock()

		result.NodeResults[lobeID] = nr
	}

	result.EngramsDivergent = totalDivergent

	// Sum synced across all nodes.
	totalSynced := 0
	for _, nr := range result.NodeResults {
		totalSynced += nr.Synced
	}
	result.WeightsSynced = totalSynced
	result.CompletedAt = time.Now()

	r.mu.Lock()
	r.lastResult = result
	r.mu.Unlock()

	slog.Debug("reconcile: complete",
		"checked", result.EngramsChecked,
		"divergent", result.EngramsDivergent,
		"synced", result.WeightsSynced,
		"lobes", len(lobeNodeIDs),
	)

	return result, nil
}

// HandleReconReply is called when a TypeReconReply frame arrives from a Lobe (Cortex path).
func (r *Reconciler) HandleReconReply(fromNodeID string, payload []byte) error {
	var reply mbp.ReconReplyMsg
	if err := msgpack.Unmarshal(payload, &reply); err != nil {
		return fmt.Errorf("reconcile: unmarshal ReconReply: %w", err)
	}

	r.pendingMu.Lock()
	ch, ok := r.pendingReply[reply.RequestID]
	if !ok {
		r.pendingMu.Unlock()
		return nil // stale or timed-out reply
	}
	select {
	case ch <- reply:
	default:
	}
	r.pendingMu.Unlock()
	return nil
}

// HandleReconAck is called when a TypeReconAck frame arrives from a Lobe (Cortex path).
func (r *Reconciler) HandleReconAck(fromNodeID string, payload []byte) error {
	var ack mbp.ReconAckMsg
	if err := msgpack.Unmarshal(payload, &ack); err != nil {
		return fmt.Errorf("reconcile: unmarshal ReconAck: %w", err)
	}

	r.pendingMu.Lock()
	ch, ok := r.pendingAck[ack.RequestID]
	if !ok {
		r.pendingMu.Unlock()
		return nil
	}
	select {
	case ch <- ack:
	default:
	}
	r.pendingMu.Unlock()
	return nil
}

// HandleReconProbe is called when a TypeReconProbe frame arrives on a Lobe.
// It reads local weights for the requested keys and sends back a TypeReconReply.
func (r *Reconciler) HandleReconProbe(fromNodeID string, payload []byte) error {
	var probe mbp.ReconProbeMsg
	if err := msgpack.Unmarshal(payload, &probe); err != nil {
		return fmt.Errorf("reconcile: unmarshal ReconProbe: %w", err)
	}

	// Get local weights for the requested keys.
	var localWeights []float64
	if r.sampler != nil {
		weightMap, err := r.sampler.GetAssocWeightsForKeys(probe.Keys)
		if err != nil {
			slog.Warn("reconcile: lobe failed to get weights", "err", err)
			localWeights = make([]float64, len(probe.Keys))
		} else {
			localWeights = make([]float64, len(probe.Keys))
			for i, k := range probe.Keys {
				localWeights[i] = weightMap[k] // 0.0 if not found
			}
		}
	} else {
		localWeights = make([]float64, len(probe.Keys))
	}

	reply := mbp.ReconReplyMsg{
		RequestID: probe.RequestID,
		NodeID:    r.coord.cfg.NodeID,
		Weights:   localWeights,
	}
	replyPayload, err := msgpack.Marshal(reply)
	if err != nil {
		return fmt.Errorf("reconcile: marshal ReconReply: %w", err)
	}

	peer, ok := r.coord.mgr.GetPeer(fromNodeID)
	if !ok {
		return nil // Cortex unreachable
	}
	return peer.Send(mbp.TypeReconReply, replyPayload)
}

// HandleReconSync is called when a TypeReconSync frame arrives on a Lobe.
// It applies the corrected weights and sends back a TypeReconAck.
func (r *Reconciler) HandleReconSync(fromNodeID string, payload []byte) error {
	var syncMsg mbp.ReconSyncMsg
	if err := msgpack.Unmarshal(payload, &syncMsg); err != nil {
		return fmt.Errorf("reconcile: unmarshal ReconSync: %w", err)
	}

	applied := 0
	if r.store != nil {
		ctx := context.Background()
		for i, k := range syncMsg.Keys {
			if i >= len(syncMsg.Weights) {
				break
			}
			if err := r.store.UpdateAssocWeight(ctx, k, syncMsg.Weights[i]); err != nil {
				slog.Warn("reconcile: lobe failed to apply weight", "key", fmt.Sprintf("%x", k), "err", err)
				continue
			}
			applied++
		}
	}

	ack := mbp.ReconAckMsg{
		RequestID: syncMsg.RequestID,
		NodeID:    r.coord.cfg.NodeID,
		Applied:   applied,
	}
	ackPayload, err := msgpack.Marshal(ack)
	if err != nil {
		return fmt.Errorf("reconcile: marshal ReconAck: %w", err)
	}

	peer, ok := r.coord.mgr.GetPeer(fromNodeID)
	if !ok {
		return nil
	}
	return peer.Send(mbp.TypeReconAck, ackPayload)
}
