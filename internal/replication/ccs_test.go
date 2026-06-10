package replication

import (
	"bytes"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// mockHebbianSampler implements HebbianSampler for tests.
type mockHebbianSampler struct {
	keys    [][16]byte
	weights map[[16]byte]float64
}

func (m *mockHebbianSampler) SampleKeys(n int) ([][16]byte, error) {
	if n >= len(m.keys) {
		return m.keys, nil
	}
	return m.keys[:n], nil
}

func (m *mockHebbianSampler) GetAssocWeightsForKeys(keys [][16]byte) (map[[16]byte]float64, error) {
	result := make(map[[16]byte]float64, len(keys))
	for _, k := range keys {
		if w, ok := m.weights[k]; ok {
			result[k] = w
		}
	}
	return result, nil
}

// TestCCS_SingleNode_PerfectScore verifies that a single-node cluster (no Lobes)
// always returns score=1.0 and assessment="excellent".
func TestCCS_SingleNode_PerfectScore(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord, 1)

	store := &mockHebbianSampler{
		keys:    [][16]byte{{1}, {2}, {3}},
		weights: map[[16]byte]float64{{1}: 0.5, {2}: 0.8, {3}: 0.3},
	}
	probe := NewCCSProbe(store, coord)
	coord.SetCCSProbe(probe)

	// No lobes registered → single-node, probe directly.
	probe.probe(nil)

	result := probe.LastResult()
	if result.Score != 1.0 {
		t.Errorf("single-node: expected score=1.0, got %f", result.Score)
	}
	if result.Assessment != "excellent" {
		t.Errorf("single-node: expected assessment=excellent, got %q", result.Assessment)
	}
	if len(result.NodeScores) != 0 {
		t.Errorf("single-node: expected empty NodeScores, got %v", result.NodeScores)
	}
}

// TestCCS_Assessment_Thresholds verifies all 4 assessment thresholds.
func TestCCS_Assessment_Thresholds(t *testing.T) {
	cases := []struct {
		score      float64
		assessment string
	}{
		{1.00, "excellent"},
		{0.999, "excellent"},
		{0.96, "good"},
		{0.951, "good"},
		{0.91, "degraded"},
		{0.901, "degraded"},
		{0.90, "critical"},
		{0.50, "critical"},
		{0.00, "critical"},
	}

	for _, tc := range cases {
		got := ccsAssessment(tc.score)
		if got != tc.assessment {
			t.Errorf("score %.3f: expected assessment=%q, got %q", tc.score, tc.assessment, got)
		}
	}
}

// TestCCS_HashComputation verifies that the same (key, weight) pairs produce the
// same hash regardless of insertion order.
func TestCCS_HashComputation(t *testing.T) {
	keys := [][16]byte{{1}, {2}, {3}, {4}}
	weights := map[[16]byte]float64{
		{1}: 0.5,
		{2}: 0.8,
		{3}: 0.3,
		{4}: 0.9,
	}

	// Same keys, same order.
	hash1 := ComputeCCSHash(keys, weights)

	// Reversed order.
	reversed := [][16]byte{{4}, {3}, {2}, {1}}
	hash2 := ComputeCCSHash(reversed, weights)

	if !bytes.Equal(hash1, hash2) {
		t.Errorf("hash mismatch: insertion order should not affect result\nhash1=%x\nhash2=%x", hash1, hash2)
	}

	// Shuffled order.
	shuffled := [][16]byte{{2}, {4}, {1}, {3}}
	hash3 := ComputeCCSHash(shuffled, weights)

	if !bytes.Equal(hash1, hash3) {
		t.Errorf("hash mismatch with shuffled keys\nhash1=%x\nhash3=%x", hash1, hash3)
	}

	// Different weights → different hash.
	differentWeights := map[[16]byte]float64{
		{1}: 0.1,
		{2}: 0.8,
		{3}: 0.3,
		{4}: 0.9,
	}
	hash4 := ComputeCCSHash(keys, differentWeights)
	if bytes.Equal(hash1, hash4) {
		t.Error("expected different hash for different weights, got same")
	}
}

// TestCCS_Protocol_MockLobe tests the full CCS protocol using net.Pipe to simulate
// a Lobe that responds to CCSProbe frames. It tests both matching (score=1.0) and
// non-matching (score=0.0) hash scenarios.
func TestCCS_Protocol_MockLobe(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord, 1)

	lobeID := "lobe-ccs-test"

	// Register lobe in join handler so probe sees it.
	coord.joinHandler.mu.Lock()
	coord.joinHandler.members[lobeID] = NodeInfo{NodeID: lobeID, Addr: "127.0.0.1:9500"}
	coord.joinHandler.mu.Unlock()

	// Use a net.Pipe as the peer connection.
	cortexSide, lobeSide := net.Pipe()
	t.Cleanup(func() {
		cortexSide.Close()
		lobeSide.Close()
	})

	peer := &PeerConn{nodeID: lobeID, conn: cortexSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[lobeID] = peer
	coord.mgr.mu.Unlock()

	// Shared store data.
	keys := [][16]byte{{10}, {20}, {30}}
	weights := map[[16]byte]float64{{10}: 0.5, {20}: 0.8, {30}: 0.3}
	store := &mockHebbianSampler{keys: keys, weights: weights}
	probe := NewCCSProbe(store, coord)
	coord.SetCCSProbe(probe)

	// --- Test 1: lobe responds with matching hash → score=1.0 ---
	t.Run("matching_hash", func(t *testing.T) {
		// Run the lobe responder in a goroutine.
		lobeResponderDone := make(chan error, 1)
		go func() {
			// Read the CCSProbe frame.
			f, err := mbp.ReadFrame(lobeSide)
			if err != nil {
				lobeResponderDone <- err
				return
			}
			if f.Type != mbp.TypeCCSProbe {
				lobeResponderDone <- nil // wrong type → signal done anyway
				return
			}

			var probeMsg mbp.CCSProbeMsg
			if err := msgpack.Unmarshal(f.Payload, &probeMsg); err != nil {
				lobeResponderDone <- err
				return
			}

			// Compute matching hash (same store).
			matchingHash := ComputeCCSHash(probeMsg.SampledKeys, weights)

			resp := mbp.CCSResponseMsg{
				RequestID: probeMsg.RequestID,
				NodeID:    lobeID,
				Hash:      matchingHash,
				KeyCount:  len(probeMsg.SampledKeys),
			}
			payload, err := msgpack.Marshal(resp)
			if err != nil {
				lobeResponderDone <- err
				return
			}

			// Send TypeCCSResponse back to Cortex.
			// We call HandleCCSResponse directly since we're within the same process.
			lobeResponderDone <- probe.HandleCCSResponse(lobeID, payload)
		}()

		probe.probe(nil) // this blocks until responses arrive or 5s timeout

		if err := <-lobeResponderDone; err != nil {
			t.Fatalf("lobe responder error: %v", err)
		}

		result := probe.LastResult()
		if result.Score != 1.0 {
			t.Errorf("matching hash: expected score=1.0, got %f", result.Score)
		}
		if result.Assessment != "excellent" {
			t.Errorf("matching hash: expected assessment=excellent, got %q", result.Assessment)
		}
		if score, ok := result.NodeScores[lobeID]; !ok || score != 1.0 {
			t.Errorf("matching hash: expected NodeScores[%q]=1.0, got %v", lobeID, result.NodeScores)
		}
	})

	// --- Test 2: lobe responds with wrong hash → score=0.0 ---
	t.Run("wrong_hash", func(t *testing.T) {
		lobeResponderDone := make(chan error, 1)
		go func() {
			f, err := mbp.ReadFrame(lobeSide)
			if err != nil {
				lobeResponderDone <- err
				return
			}
			if f.Type != mbp.TypeCCSProbe {
				lobeResponderDone <- nil
				return
			}

			var probeMsg mbp.CCSProbeMsg
			if err := msgpack.Unmarshal(f.Payload, &probeMsg); err != nil {
				lobeResponderDone <- err
				return
			}

			// Different weights → different hash.
			differentWeights := map[[16]byte]float64{{10}: 0.1, {20}: 0.2, {30}: 0.9}
			wrongHash := ComputeCCSHash(probeMsg.SampledKeys, differentWeights)

			resp := mbp.CCSResponseMsg{
				RequestID: probeMsg.RequestID,
				NodeID:    lobeID,
				Hash:      wrongHash,
				KeyCount:  len(probeMsg.SampledKeys),
			}
			payload, err := msgpack.Marshal(resp)
			if err != nil {
				lobeResponderDone <- err
				return
			}
			lobeResponderDone <- probe.HandleCCSResponse(lobeID, payload)
		}()

		probe.probe(nil)

		if err := <-lobeResponderDone; err != nil {
			t.Fatalf("lobe responder (wrong hash) error: %v", err)
		}

		result := probe.LastResult()
		if result.Score != 0.0 {
			t.Errorf("wrong hash: expected score=0.0, got %f", result.Score)
		}
		if result.Assessment != "critical" {
			t.Errorf("wrong hash: expected assessment=critical, got %q", result.Assessment)
		}
		if score, ok := result.NodeScores[lobeID]; !ok || score != 0.0 {
			t.Errorf("wrong hash: expected NodeScores[%q]=0.0, got %v", lobeID, result.NodeScores)
		}
	})
}

// TestCCS_NoResponse_ZeroScore verifies that a lobe that does not respond within
// the timeout results in score=0.0 for that node.
func TestCCS_NoResponse_ZeroScore(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord, 1)

	lobeID := "lobe-silent"
	coord.joinHandler.mu.Lock()
	coord.joinHandler.members[lobeID] = NodeInfo{NodeID: lobeID, Addr: "127.0.0.1:9501"}
	coord.joinHandler.mu.Unlock()

	// Use a pipe but the "lobe" side never reads — Send will block/fail quickly
	// because the net.Pipe buffer is small.
	cortexSide, lobeSide := net.Pipe()
	t.Cleanup(func() {
		cortexSide.Close()
		lobeSide.Close()
	})

	// Drain the probe frame so Send doesn't block, but never respond.
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

	store := &mockHebbianSampler{
		keys:    [][16]byte{{5}},
		weights: map[[16]byte]float64{{5}: 0.5},
	}
	probe := NewCCSProbe(store, coord)
	// Reduce sampleN and make timeout short for the test.
	probe.sampleN = 1

	// probe() has a 5s timeout; for the test we don't want to wait.
	// Instead, just let the probe finish with a missing response and check score.
	// We'll use the direct probe API but check the fast path:
	// Trigger probe in a goroutine that we wait on with a deadline.
	done := make(chan struct{})
	go func() {
		defer close(done)
		probe.probe(nil)
	}()

	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Fatal("probe timed out waiting for response timeout")
	}

	result := probe.LastResult()
	if score, ok := result.NodeScores[lobeID]; !ok || score != 0.0 {
		t.Errorf("no-response: expected NodeScores[%q]=0.0, got %v", lobeID, result.NodeScores)
	}
	if result.Score != 0.0 {
		t.Errorf("no-response: expected overall score=0.0, got %f", result.Score)
	}
}

// TestCCS_HandleCCSProbe_LobeComputesAndResponds verifies the Lobe-side handler:
// HandleCCSProbe unmarshals the probe, computes the local hash, and sends back a
// TypeCCSResponse frame to the Cortex peer.
func TestCCS_HandleCCSProbe_LobeComputesAndResponds(t *testing.T) {
	// Create a coordinator for the Lobe node.
	lobeCoord, _ := newTestCoordinator(t, "replica")

	// Known keys and weights the Lobe "stores".
	keys := [][16]byte{{1}, {2}, {3}}
	weights := map[[16]byte]float64{{1}: 0.5, {2}: 0.8, {3}: 0.3}

	store := &mockHebbianSampler{keys: keys, weights: weights}
	probe := NewCCSProbe(store, lobeCoord)

	// Set up a net.Pipe to represent the Cortex↔Lobe connection.
	// The Lobe (probe) will Send() on the "cortexSide" peer, so we read from lobeSide.
	cortexSide, lobeSide := net.Pipe()
	t.Cleanup(func() {
		cortexSide.Close()
		lobeSide.Close()
	})

	cortexNodeID := "cortex-1"

	// Register cortex as a known peer in the Lobe's ConnManager so that
	// HandleCCSProbe can retrieve it via GetPeer(fromNodeID).
	peer := &PeerConn{nodeID: cortexNodeID, conn: cortexSide}
	lobeCoord.mgr.mu.Lock()
	lobeCoord.mgr.peers[cortexNodeID] = peer
	lobeCoord.mgr.mu.Unlock()

	// Build a CCSProbeMsg with the known keys.
	probeMsg := mbp.CCSProbeMsg{
		RequestID:   "req-1",
		SampledKeys: keys,
	}
	payload, err := msgpack.Marshal(probeMsg)
	if err != nil {
		t.Fatalf("marshal CCSProbeMsg: %v", err)
	}

	// Read the response frame from lobeSide in a goroutine.
	responseCh := make(chan mbp.CCSResponseMsg, 1)
	errCh := make(chan error, 1)
	go func() {
		f, err := mbp.ReadFrame(lobeSide)
		if err != nil {
			errCh <- err
			return
		}
		if f.Type != mbp.TypeCCSResponse {
			errCh <- fmt.Errorf("expected TypeCCSResponse 0x%02x, got 0x%02x", mbp.TypeCCSResponse, f.Type)
			return
		}
		var resp mbp.CCSResponseMsg
		if err := msgpack.Unmarshal(f.Payload, &resp); err != nil {
			errCh <- err
			return
		}
		responseCh <- resp
	}()

	// Call the Lobe-side handler.
	if err := probe.HandleCCSProbe(cortexNodeID, payload); err != nil {
		t.Fatalf("HandleCCSProbe: %v", err)
	}

	// Wait for the response or an error.
	select {
	case err := <-errCh:
		t.Fatalf("reading CCSResponse: %v", err)
	case resp := <-responseCh:
		// Verify RequestID is echoed back.
		if resp.RequestID != "req-1" {
			t.Errorf("RequestID = %q, want req-1", resp.RequestID)
		}
		// Verify the hash matches what ComputeCCSHash would produce locally.
		expectedHash := ComputeCCSHash(keys, weights)
		if !bytes.Equal(resp.Hash, expectedHash) {
			t.Errorf("hash mismatch:\n  got:  %x\n  want: %x", resp.Hash, expectedHash)
		}
		if resp.KeyCount != len(keys) {
			t.Errorf("KeyCount = %d, want %d", resp.KeyCount, len(keys))
		}
	}
}

// TestCCS_CortexConsistency verifies coordinator.CognitiveConsistency() works.
func TestCCS_CortexConsistency(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")

	// Without a probe wired, should return default excellent.
	result := coord.CognitiveConsistency()
	if result.Score != 1.0 {
		t.Errorf("no probe: expected score=1.0, got %f", result.Score)
	}
	if result.Assessment != "excellent" {
		t.Errorf("no probe: expected assessment=excellent, got %q", result.Assessment)
	}

	// Wire a probe and check its result is returned.
	store := &mockHebbianSampler{}
	probe := NewCCSProbe(store, coord)
	coord.SetCCSProbe(probe)

	// Manually set last result.
	probe.mu.Lock()
	probe.last = CCSResult{
		Score:      0.95,
		Assessment: "good",
		NodeScores: map[string]float64{"node-x": 1.0},
		SampledAt:  time.Now(),
	}
	probe.mu.Unlock()

	result = coord.CognitiveConsistency()
	if result.Score != 0.95 {
		t.Errorf("with probe: expected score=0.95, got %f", result.Score)
	}
	if result.Assessment != "good" {
		t.Errorf("with probe: expected assessment=good, got %q", result.Assessment)
	}
}
