package replication

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// mockHebbianStoreWriter records UpdateAssocWeight calls for test assertions.
type mockHebbianStoreWriter struct {
	mu      sync.Mutex
	weights map[[16]byte]float64
}

func newMockHebbianStoreWriter() *mockHebbianStoreWriter {
	return &mockHebbianStoreWriter{
		weights: make(map[[16]byte]float64),
	}
}

func (m *mockHebbianStoreWriter) UpdateAssocWeight(_ context.Context, id [16]byte, weight float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.weights[id] = weight
	return nil
}

func (m *mockHebbianStoreWriter) getWeight(id [16]byte) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.weights[id]
	return w, ok
}

// mockLobeHandler simulates a Lobe that reads ReconProbe/ReconSync frames
// from the lobe side of a net.Pipe and responds by calling back into the
// cortex reconciler's HandleReconReply/HandleReconAck methods.
// lobeWeights is the lobe's local weight map; lobeStore is where synced weights go.
func mockLobeHandler(
	t *testing.T,
	lobeSide net.Conn,
	lobeID string,
	lobeWeights map[[16]byte]float64,
	lobeStore *mockHebbianStoreWriter,
	cortexReconciler *Reconciler,
	done chan<- error,
) {
	t.Helper()

	for {
		f, err := mbp.ReadFrame(lobeSide)
		if err != nil {
			done <- nil // pipe closed or EOF
			return
		}

		switch f.Type {
		case mbp.TypeReconProbe:
			var probe mbp.ReconProbeMsg
			if err := msgpack.Unmarshal(f.Payload, &probe); err != nil {
				done <- err
				return
			}

			// Build reply with lobe's local weights.
			replyWeights := make([]float64, len(probe.Keys))
			for i, k := range probe.Keys {
				if w, ok := lobeWeights[k]; ok {
					replyWeights[i] = w
				} // else 0.0 (missing)
			}

			reply := mbp.ReconReplyMsg{
				RequestID: probe.RequestID,
				NodeID:    lobeID,
				Weights:   replyWeights,
			}
			replyPayload, _ := msgpack.Marshal(reply)

			// Deliver directly to cortex reconciler.
			if err := cortexReconciler.HandleReconReply(lobeID, replyPayload); err != nil {
				done <- err
				return
			}

		case mbp.TypeReconSync:
			var syncMsg mbp.ReconSyncMsg
			if err := msgpack.Unmarshal(f.Payload, &syncMsg); err != nil {
				done <- err
				return
			}

			// Apply weights to lobe store.
			applied := 0
			for i, k := range syncMsg.Keys {
				if i < len(syncMsg.Weights) {
					lobeStore.mu.Lock()
					lobeStore.weights[k] = syncMsg.Weights[i]
					lobeStore.mu.Unlock()
					lobeWeights[k] = syncMsg.Weights[i]
					applied++
				}
			}

			ack := mbp.ReconAckMsg{
				RequestID: syncMsg.RequestID,
				NodeID:    lobeID,
				Applied:   applied,
			}
			ackPayload, _ := msgpack.Marshal(ack)

			if err := cortexReconciler.HandleReconAck(lobeID, ackPayload); err != nil {
				done <- err
				return
			}

			done <- nil
			return

		default:
			done <- fmt.Errorf("unexpected frame type: 0x%02x", f.Type)
			return
		}
	}
}

func TestReconciler_NoDeviation(t *testing.T) {
	keys := make([][16]byte, 100)
	weights := make(map[[16]byte]float64, 100)
	for i := 0; i < 100; i++ {
		keys[i] = [16]byte{byte(i)}
		weights[keys[i]] = float64(i+1) * 0.01
	}

	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord, 1)

	lobeID := "lobe-recon"
	coord.joinHandler.mu.Lock()
	coord.joinHandler.members[lobeID] = NodeInfo{NodeID: lobeID, Addr: "127.0.0.1:9600"}
	coord.joinHandler.mu.Unlock()

	cortexSide, lobeSide := net.Pipe()
	t.Cleanup(func() { cortexSide.Close(); lobeSide.Close() })

	peer := &PeerConn{nodeID: lobeID, conn: cortexSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[lobeID] = peer
	coord.mgr.mu.Unlock()

	sampler := &mockHebbianSampler{keys: keys, weights: weights}
	store := newMockHebbianStoreWriter()
	reconciler := NewReconciler(sampler, store, coord)
	coord.SetReconciler(reconciler)

	// Lobe has identical weights.
	lobeWeights := make(map[[16]byte]float64, len(weights))
	for k, v := range weights {
		lobeWeights[k] = v
	}
	lobeStore := newMockHebbianStoreWriter()

	done := make(chan error, 1)
	go mockLobeHandler(t, lobeSide, lobeID, lobeWeights, lobeStore, reconciler, done)

	result, err := reconciler.Run(context.Background(), []string{lobeID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The lobe handler exits after probe (no sync needed) when pipe closes.
	cortexSide.Close()
	select {
	case lobeErr := <-done:
		if lobeErr != nil {
			t.Fatalf("lobe handler: %v", lobeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lobe handler did not complete")
	}

	if result.EngramsDivergent != 0 {
		t.Errorf("expected EngramsDivergent=0, got %d", result.EngramsDivergent)
	}
	if result.WeightsSynced != 0 {
		t.Errorf("expected WeightsSynced=0, got %d", result.WeightsSynced)
	}
	if result.EngramsChecked != 100 {
		t.Errorf("expected EngramsChecked=100, got %d", result.EngramsChecked)
	}

	nr, ok := result.NodeResults[lobeID]
	if !ok {
		t.Fatal("expected NodeResult for lobe")
	}
	if nr.Divergent != 0 {
		t.Errorf("expected node Divergent=0, got %d", nr.Divergent)
	}
	if nr.Synced != 0 {
		t.Errorf("expected node Synced=0, got %d", nr.Synced)
	}
}

func TestReconciler_PartialDeviation(t *testing.T) {
	const total = 500
	const divergent = 50

	keys := make([][16]byte, total)
	cortexWeights := make(map[[16]byte]float64, total)
	lobeWeights := make(map[[16]byte]float64, total)

	for i := 0; i < total; i++ {
		keys[i] = [16]byte{byte(i >> 8), byte(i)}
		cortexWeights[keys[i]] = float64(i+1) * 0.001
		if i < divergent {
			lobeWeights[keys[i]] = float64(i+1) * 0.002
		} else {
			lobeWeights[keys[i]] = cortexWeights[keys[i]]
		}
	}

	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord, 1)

	lobeID := "lobe-partial"
	coord.joinHandler.mu.Lock()
	coord.joinHandler.members[lobeID] = NodeInfo{NodeID: lobeID, Addr: "127.0.0.1:9601"}
	coord.joinHandler.mu.Unlock()

	cortexSide, lobeSide := net.Pipe()
	t.Cleanup(func() { cortexSide.Close(); lobeSide.Close() })

	peer := &PeerConn{nodeID: lobeID, conn: cortexSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[lobeID] = peer
	coord.mgr.mu.Unlock()

	sampler := &mockHebbianSampler{keys: keys, weights: cortexWeights}
	store := newMockHebbianStoreWriter()
	reconciler := NewReconciler(sampler, store, coord)
	coord.SetReconciler(reconciler)

	lobeStore := newMockHebbianStoreWriter()
	for k, w := range lobeWeights {
		lobeStore.weights[k] = w
	}

	done := make(chan error, 1)
	go mockLobeHandler(t, lobeSide, lobeID, lobeWeights, lobeStore, reconciler, done)

	result, err := reconciler.Run(context.Background(), []string{lobeID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case lobeErr := <-done:
		if lobeErr != nil {
			t.Fatalf("lobe handler: %v", lobeErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lobe handler did not complete")
	}

	if result.EngramsChecked != total {
		t.Errorf("expected EngramsChecked=%d, got %d", total, result.EngramsChecked)
	}
	if result.EngramsDivergent != divergent {
		t.Errorf("expected EngramsDivergent=%d, got %d", divergent, result.EngramsDivergent)
	}
	if result.WeightsSynced != divergent {
		t.Errorf("expected WeightsSynced=%d, got %d", divergent, result.WeightsSynced)
	}

	nr := result.NodeResults[lobeID]
	if nr.Divergent != divergent {
		t.Errorf("expected node Divergent=%d, got %d", divergent, nr.Divergent)
	}
	if nr.Synced != divergent {
		t.Errorf("expected node Synced=%d, got %d", divergent, nr.Synced)
	}

	// Verify lobe's weights were updated to Cortex's values.
	for i := 0; i < divergent; i++ {
		key := keys[i]
		expected := cortexWeights[key]
		got, ok := lobeStore.getWeight(key)
		if !ok {
			t.Errorf("expected lobe to have weight for key %d", i)
			continue
		}
		if got != expected {
			t.Errorf("key %d: expected weight=%f, got %f", i, expected, got)
		}
	}
}

func TestReconciler_MissingKeys(t *testing.T) {
	const total = 100
	const missing = 20

	keys := make([][16]byte, total)
	cortexWeights := make(map[[16]byte]float64, total)
	lobeWeights := make(map[[16]byte]float64, total-missing)

	for i := 0; i < total; i++ {
		keys[i] = [16]byte{byte(i)}
		cortexWeights[keys[i]] = 0.5
		if i >= missing {
			lobeWeights[keys[i]] = 0.5
		}
	}

	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord, 1)

	lobeID := "lobe-missing"
	coord.joinHandler.mu.Lock()
	coord.joinHandler.members[lobeID] = NodeInfo{NodeID: lobeID, Addr: "127.0.0.1:9602"}
	coord.joinHandler.mu.Unlock()

	cortexSide, lobeSide := net.Pipe()
	t.Cleanup(func() { cortexSide.Close(); lobeSide.Close() })

	peer := &PeerConn{nodeID: lobeID, conn: cortexSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[lobeID] = peer
	coord.mgr.mu.Unlock()

	sampler := &mockHebbianSampler{keys: keys, weights: cortexWeights}
	reconciler := NewReconciler(sampler, newMockHebbianStoreWriter(), coord)
	coord.SetReconciler(reconciler)

	lobeStore := newMockHebbianStoreWriter()
	for k, w := range lobeWeights {
		lobeStore.weights[k] = w
	}

	done := make(chan error, 1)
	go mockLobeHandler(t, lobeSide, lobeID, lobeWeights, lobeStore, reconciler, done)

	result, err := reconciler.Run(context.Background(), []string{lobeID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case lobeErr := <-done:
		if lobeErr != nil {
			t.Fatalf("lobe handler: %v", lobeErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lobe handler did not complete")
	}

	if result.EngramsDivergent != missing {
		t.Errorf("expected EngramsDivergent=%d, got %d", missing, result.EngramsDivergent)
	}
	if result.WeightsSynced != missing {
		t.Errorf("expected WeightsSynced=%d, got %d", missing, result.WeightsSynced)
	}

	// Verify the missing keys were synced.
	for i := 0; i < missing; i++ {
		got, ok := lobeStore.getWeight(keys[i])
		if !ok {
			t.Errorf("expected lobe to have weight for missing key %d", i)
			continue
		}
		if got != 0.5 {
			t.Errorf("key %d: expected weight=0.5, got %f", i, got)
		}
	}
}

func TestReconciler_Timeout(t *testing.T) {
	keys := [][16]byte{{1}, {2}, {3}}
	weights := map[[16]byte]float64{{1}: 0.5, {2}: 0.8, {3}: 0.3}

	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord, 1)

	silentLobeID := "lobe-silent"
	respondingLobeID := "lobe-responding"

	coord.joinHandler.mu.Lock()
	coord.joinHandler.members[silentLobeID] = NodeInfo{NodeID: silentLobeID, Addr: "127.0.0.1:9700"}
	coord.joinHandler.members[respondingLobeID] = NodeInfo{NodeID: respondingLobeID, Addr: "127.0.0.1:9701"}
	coord.joinHandler.mu.Unlock()

	// Silent lobe: drains but never responds.
	silentCortexSide, silentLobeSide := net.Pipe()
	t.Cleanup(func() { silentCortexSide.Close(); silentLobeSide.Close() })

	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := silentLobeSide.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	silentPeer := &PeerConn{nodeID: silentLobeID, conn: silentCortexSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[silentLobeID] = silentPeer
	coord.mgr.mu.Unlock()

	// Responding lobe.
	respondCortexSide, respondLobeSide := net.Pipe()
	t.Cleanup(func() { respondCortexSide.Close(); respondLobeSide.Close() })

	respondPeer := &PeerConn{nodeID: respondingLobeID, conn: respondCortexSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[respondingLobeID] = respondPeer
	coord.mgr.mu.Unlock()

	sampler := &mockHebbianSampler{keys: keys, weights: weights}
	reconciler := NewReconciler(sampler, newMockHebbianStoreWriter(), coord)
	coord.SetReconciler(reconciler)

	// Same weights on responding lobe → no divergence.
	respondWeights := make(map[[16]byte]float64, len(weights))
	for k, v := range weights {
		respondWeights[k] = v
	}

	done := make(chan error, 1)
	go mockLobeHandler(t, respondLobeSide, respondingLobeID, respondWeights, newMockHebbianStoreWriter(), reconciler, done)

	result, err := reconciler.Run(context.Background(), []string{silentLobeID, respondingLobeID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Close responding lobe pipe to let handler exit.
	respondCortexSide.Close()
	select {
	case lobeErr := <-done:
		if lobeErr != nil {
			t.Logf("lobe handler (may be expected): %v", lobeErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lobe handler did not complete")
	}

	// Silent lobe should have an error.
	silentResult, ok := result.NodeResults[silentLobeID]
	if !ok {
		t.Fatal("expected NodeResult for silent lobe")
	}
	if silentResult.Error == "" {
		t.Error("expected error for silent lobe (timeout)")
	}

	// Responding lobe should be fine.
	respondResult, ok := result.NodeResults[respondingLobeID]
	if !ok {
		t.Fatal("expected NodeResult for responding lobe")
	}
	if respondResult.Error != "" {
		t.Errorf("expected no error for responding lobe, got: %s", respondResult.Error)
	}
	if respondResult.Divergent != 0 {
		t.Errorf("expected 0 divergent for responding lobe (same weights), got %d", respondResult.Divergent)
	}
}

func TestReconciler_ConcurrentRunPrevented(t *testing.T) {
	keys := [][16]byte{{1}}
	weights := map[[16]byte]float64{{1}: 0.5}

	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord, 1)

	lobeID := "lobe-concurrent"
	coord.joinHandler.mu.Lock()
	coord.joinHandler.members[lobeID] = NodeInfo{NodeID: lobeID, Addr: "127.0.0.1:9800"}
	coord.joinHandler.mu.Unlock()

	cortexSide, lobeSide := net.Pipe()
	t.Cleanup(func() { cortexSide.Close(); lobeSide.Close() })

	// Drain lobe side so Send doesn't block, but never reply.
	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := lobeSide.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	peer := &PeerConn{nodeID: lobeID, conn: cortexSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[lobeID] = peer
	coord.mgr.mu.Unlock()

	sampler := &mockHebbianSampler{keys: keys, weights: weights}
	reconciler := NewReconciler(sampler, newMockHebbianStoreWriter(), coord)
	coord.SetReconciler(reconciler)

	// Start first Run in background (will block waiting for reply timeout).
	firstDone := make(chan error, 1)
	go func() {
		_, err := reconciler.Run(context.Background(), []string{lobeID})
		firstDone <- err
	}()

	// Give it a moment to start.
	time.Sleep(50 * time.Millisecond)

	// Second concurrent Run should fail immediately.
	_, err := reconciler.Run(context.Background(), []string{lobeID})
	if err != ErrReconciliationInProgress {
		t.Errorf("expected ErrReconciliationInProgress, got: %v", err)
	}

	// Wait for the first run to finish (will timeout on reply after 5s).
	select {
	case <-firstDone:
	case <-time.After(10 * time.Second):
		t.Fatal("first Run did not complete")
	}
}

func TestReconciler_CoordinatorTrigger(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord, 1)

	// Without reconciler wired, TriggerReconciliation should error.
	_, err := coord.TriggerReconciliation(context.Background(), []string{"lobe-1"})
	if err == nil {
		t.Fatal("expected error when reconciler not configured")
	}

	// Wire a reconciler.
	sampler := &mockHebbianSampler{}
	reconciler := NewReconciler(sampler, newMockHebbianStoreWriter(), coord)
	coord.SetReconciler(reconciler)

	// With no keys, reconciliation should succeed quickly.
	result, err := coord.TriggerReconciliation(context.Background(), []string{})
	if err != nil {
		t.Fatalf("TriggerReconciliation: %v", err)
	}
	if result.EngramsChecked != 0 {
		t.Errorf("expected EngramsChecked=0, got %d", result.EngramsChecked)
	}
}

// TestReconciler_HandleReconSync_ViaFrameDispatch verifies that when the
// coordinator dispatches a TypeReconSync frame it routes to the reconciler's
// HandleReconSync, which applies each key-weight pair via UpdateAssocWeight.
func TestReconciler_HandleReconSync_ViaFrameDispatch(t *testing.T) {
	coord, _ := newTestCoordinator(t, "replica")

	store := newMockHebbianStoreWriter()
	sampler := &mockHebbianSampler{}
	reconciler := NewReconciler(sampler, store, coord)
	coord.SetReconciler(reconciler)

	// Wire a pipe-based peer for the Cortex so HandleReconSync can send ack back.
	cortexNodeID := "cortex-dispatch"
	cortexSide, lobeSide := net.Pipe()
	t.Cleanup(func() {
		cortexSide.Close()
		lobeSide.Close()
	})

	// Drain the ack frame from cortexSide so Send doesn't block.
	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := cortexSide.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	peer := &PeerConn{nodeID: cortexNodeID, conn: lobeSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[cortexNodeID] = peer
	coord.mgr.mu.Unlock()

	// Build a ReconSyncMsg with 5 key-weight pairs.
	const n = 5
	keys := make([][16]byte, n)
	wantWeights := make([]float64, n)
	for i := 0; i < n; i++ {
		keys[i] = [16]byte{byte(i + 1)}
		wantWeights[i] = float64(i+1) * 0.1
	}

	syncMsg := mbp.ReconSyncMsg{
		RequestID: "sync-dispatch-1",
		Keys:      keys,
		Weights:   wantWeights,
	}
	payload, err := msgpack.Marshal(syncMsg)
	if err != nil {
		t.Fatalf("marshal ReconSyncMsg: %v", err)
	}

	// Dispatch via the coordinator frame handler.
	if err := coord.HandleIncomingFrame(cortexNodeID, mbp.TypeReconSync, payload); err != nil {
		t.Fatalf("HandleIncomingFrame TypeReconSync: %v", err)
	}

	// Assert the mock store received exactly n UpdateAssocWeight calls.
	store.mu.Lock()
	got := len(store.weights)
	store.mu.Unlock()

	if got != n {
		t.Errorf("UpdateAssocWeight called %d times, want %d", got, n)
	}

	// Verify each key-weight pair was applied correctly.
	for i, k := range keys {
		w, ok := store.getWeight(k)
		if !ok {
			t.Errorf("key[%d] %x not found in store", i, k)
			continue
		}
		if w != wantWeights[i] {
			t.Errorf("key[%d] weight = %f, want %f", i, w, wantWeights[i])
		}
	}
}

func TestReconciliationTriggeredOnReconnect(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord, 1)

	// Use a short delay so the test doesn't wait 2s.
	coord.reconDelay = 50 * time.Millisecond

	sampler := &mockHebbianSampler{}
	reconciler := NewReconciler(sampler, newMockHebbianStoreWriter(), coord)
	coord.SetReconciler(reconciler)

	lobeID := "lobe-recon-trigger"
	coord.msp.AddPeer(lobeID, "127.0.0.1:9650", RoleReplica)

	// Mark peer as SDOWN to simulate a partition.
	coord.msp.mu.Lock()
	if p, ok := coord.msp.peers[lobeID]; ok {
		p.SDown = true
	}
	coord.msp.mu.Unlock()

	// Wire a pipe-based peer so startStreamerForLobe can Send.
	cortexSide, lobeSide := net.Pipe()
	t.Cleanup(func() { cortexSide.Close(); lobeSide.Close() })

	peer := &PeerConn{nodeID: lobeID, conn: cortexSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[lobeID] = peer
	coord.mgr.mu.Unlock()

	// Drain the streamer's replication frames so Send doesn't block.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := lobeSide.Read(buf); err != nil {
				return
			}
		}
	}()

	// Verify reconciler hasn't run yet.
	if r := reconciler.LastResult(); !r.StartedAt.IsZero() {
		t.Fatal("expected no reconciliation before reconnect")
	}

	// Simulate recovery: HandlePing clears SDOWN and fires OnRecover.
	coord.msp.HandlePing(lobeID, nil)

	// Wait for reconDelay + reconciliation + buffer.
	time.Sleep(300 * time.Millisecond)

	result := reconciler.LastResult()
	if result.StartedAt.IsZero() {
		t.Fatal("expected reconciliation to be triggered after reconnect")
	}
	if !result.CompletedAt.After(result.StartedAt) {
		t.Error("expected CompletedAt to be after StartedAt")
	}
}

func TestReconciler_FrameDispatch(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord, 1)

	sampler := &mockHebbianSampler{
		keys:    [][16]byte{{1}},
		weights: map[[16]byte]float64{{1}: 0.5},
	}
	store := newMockHebbianStoreWriter()
	reconciler := NewReconciler(sampler, store, coord)
	coord.SetReconciler(reconciler)

	// TypeReconReply with no pending request should be silently ignored.
	reply := mbp.ReconReplyMsg{RequestID: "nonexistent", NodeID: "lobe-x", Weights: []float64{0.5}}
	payload, _ := msgpack.Marshal(reply)
	if err := coord.HandleIncomingFrame("lobe-x", mbp.TypeReconReply, payload); err != nil {
		t.Errorf("HandleIncomingFrame ReconReply: %v", err)
	}

	// TypeReconAck with no pending request should be silently ignored.
	ack := mbp.ReconAckMsg{RequestID: "nonexistent", NodeID: "lobe-x", Applied: 0}
	payload, _ = msgpack.Marshal(ack)
	if err := coord.HandleIncomingFrame("lobe-x", mbp.TypeReconAck, payload); err != nil {
		t.Errorf("HandleIncomingFrame ReconAck: %v", err)
	}
}
