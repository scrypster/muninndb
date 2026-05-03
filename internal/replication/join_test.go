package replication

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// newTestEpochStore opens a temp Pebble DB and returns an EpochStore.
func newTestEpochStore(t *testing.T) *EpochStore {
	t.Helper()
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	es, err := NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore: %v", err)
	}
	return es
}

// newTestRepLog opens a temp Pebble DB and returns a ReplicationLog.
func newTestRepLog(t *testing.T) *ReplicationLog {
	t.Helper()
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewReplicationLog(db)
}

// sendJoinResponse writes a JoinResponse frame to conn (simulates Cortex reply).
func sendJoinResponse(t *testing.T, conn net.Conn, resp mbp.JoinResponse) {
	t.Helper()
	payload, err := msgpack.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal JoinResponse: %v", err)
	}
	frame := &mbp.Frame{
		Version:       0x01,
		Type:          mbp.TypeJoinResponse,
		PayloadLength: uint32(len(payload)),
		Payload:       payload,
	}
	if err := mbp.WriteFrame(conn, frame); err != nil {
		t.Fatalf("WriteFrame JoinResponse: %v", err)
	}
}

// readJoinRequest reads a JoinRequest frame from conn (simulates Cortex side).
func readJoinRequest(t *testing.T, conn net.Conn) mbp.JoinRequest {
	t.Helper()
	frame, err := mbp.ReadFrame(conn)
	if err != nil {
		t.Fatalf("ReadFrame JoinRequest: %v", err)
	}
	if frame.Type != mbp.TypeJoinRequest {
		t.Fatalf("expected TypeJoinRequest 0x%02x, got 0x%02x", mbp.TypeJoinRequest, frame.Type)
	}
	var req mbp.JoinRequest
	if err := msgpack.Unmarshal(frame.Payload, &req); err != nil {
		t.Fatalf("unmarshal JoinRequest: %v", err)
	}
	return req
}

// readLeaveMessage reads a LeaveMessage frame from conn.
func readLeaveMessage(t *testing.T, conn net.Conn) mbp.LeaveMessage {
	t.Helper()
	frame, err := mbp.ReadFrame(conn)
	if err != nil {
		t.Fatalf("ReadFrame LeaveMessage: %v", err)
	}
	if frame.Type != mbp.TypeLeave {
		t.Fatalf("expected TypeLeave 0x%02x, got 0x%02x", mbp.TypeLeave, frame.Type)
	}
	var msg mbp.LeaveMessage
	if err := msgpack.Unmarshal(frame.Payload, &msg); err != nil {
		t.Fatalf("unmarshal LeaveMessage: %v", err)
	}
	return msg
}

// ---- JoinHandler tests ----

func TestJoinHandler_HandleJoinRequest_Success(t *testing.T) {
	es := newTestEpochStore(t)
	// Set a non-zero epoch so the handler accepts joins.
	if err := es.ForceSet(3); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "", es, repLog, mgr)

	var joinedInfo NodeInfo
	handler.OnLobeJoined = func(info NodeInfo) {
		joinedInfo = info
	}

	req := mbp.JoinRequest{
		NodeID:          "lobe-1",
		Addr:            "127.0.0.1:9001",
		LastApplied:     0,
		ProtocolVersion: mbp.CurrentProtocolVersion,
	}

	// Simulate what the coordinator does: register the live inbound conn BEFORE
	// calling HandleJoinRequest, then pass that PeerConn into the handler.
	cortexConn, lobeConn := net.Pipe()
	t.Cleanup(func() { cortexConn.Close(); lobeConn.Close() })
	peer := mgr.RegisterConn(req.NodeID, req.Addr, cortexConn)

	resp := handler.HandleJoinRequest(req, peer)

	if !resp.Accepted {
		t.Fatalf("expected Accepted=true, got false: %s", resp.RejectReason)
	}
	if resp.Epoch != 3 {
		t.Errorf("resp.Epoch = %d, want 3", resp.Epoch)
	}
	if resp.CortexID != "cortex-1" {
		t.Errorf("resp.CortexID = %q, want %q", resp.CortexID, "cortex-1")
	}

	// Lobe should be in members.
	members := handler.Members()
	if len(members) != 1 {
		t.Fatalf("len(members) = %d, want 1", len(members))
	}
	if members[0].NodeID != "lobe-1" {
		t.Errorf("member NodeID = %q, want lobe-1", members[0].NodeID)
	}

	// OnLobeJoined callback should have been called.
	if joinedInfo.NodeID != "lobe-1" {
		t.Errorf("joinedInfo.NodeID = %q, want lobe-1", joinedInfo.NodeID)
	}

	// The peer registered before HandleJoinRequest must still be connected —
	// HandleJoinRequest must NOT call AddPeer (which would close the live conn).
	if !peer.IsConnected() {
		t.Error("PeerConn is not connected after HandleJoinRequest — AddPeer must not be called inside HandleJoinRequest")
	}
	p, ok := mgr.GetPeer("lobe-1")
	if !ok {
		t.Error("lobe-1 not found in ConnManager")
	}
	if p != peer {
		t.Error("ConnManager returned a different PeerConn — live conn was replaced")
	}
}

// TestJoinHandler_HandleJoinRequest_LiveConnPreserved is a regression test for
// the bug where HandleJoinRequest called h.mgr.AddPeer(), closing the live
// inbound PeerConn that the coordinator registered via RegisterConn before
// invoking HandleJoinRequest. After the fix, the live conn must survive the
// call and remain usable for peer.Send().
func TestJoinHandler_HandleJoinRequest_LiveConnPreserved(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "", es, repLog, mgr)

	req := mbp.JoinRequest{
		NodeID:          "test-lobe",
		Addr:            "127.0.0.1:9100",
		LastApplied:     0,
		ProtocolVersion: mbp.CurrentProtocolVersion,
	}

	// Simulate coordinator: register the live inbound conn first.
	cortexSide, lobeSide := net.Pipe()
	t.Cleanup(func() { cortexSide.Close(); lobeSide.Close() })
	peer := mgr.RegisterConn(req.NodeID, req.Addr, cortexSide)

	// Drain the lobe side in the background so peer.Send doesn't block.
	readErrs := make(chan error, 1)
	go func() {
		f, err := mbp.ReadFrame(lobeSide)
		_ = f
		readErrs <- err
	}()

	resp := handler.HandleJoinRequest(req, peer)
	if !resp.Accepted {
		t.Fatalf("HandleJoinRequest rejected: %s", resp.RejectReason)
	}

	// The live conn must still be open — not closed by AddPeer inside HandleJoinRequest.
	if !peer.IsConnected() {
		t.Fatal("regression: PeerConn was closed inside HandleJoinRequest (AddPeer called on live conn)")
	}

	// Send must succeed; before the fix it returned ErrNotConnected.
	payload := []byte("ping")
	if err := peer.Send(mbp.TypePing, payload); err != nil {
		t.Fatalf("peer.Send after HandleJoinRequest: %v (regression: AddPeer closed the live conn)", err)
	}

	// Verify the lobe side received the frame.
	select {
	case err := <-readErrs:
		if err != nil {
			t.Fatalf("lobe side read error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for frame on lobe side")
	}
}

func TestJoinHandler_HandleJoinRequest_WrongEpoch(t *testing.T) {
	es := newTestEpochStore(t)
	// Epoch 0 → cluster not bootstrapped, join must be rejected.
	// (epoch starts at 0 by default from NewEpochStore)

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "", es, repLog, mgr)

	req := mbp.JoinRequest{
		NodeID:      "lobe-2",
		Addr:        "127.0.0.1:9002",
		LastApplied: 0,
	}

	resp := handler.HandleJoinRequest(req, nil)

	if resp.Accepted {
		t.Fatal("expected Accepted=false for epoch-0 cluster, got true")
	}
	if resp.RejectReason == "" {
		t.Error("expected non-empty RejectReason")
	}

	// Members should be empty.
	if len(handler.Members()) != 0 {
		t.Errorf("members should be empty, got %d", len(handler.Members()))
	}
}

func TestJoinHandler_HandleLeave(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "", es, repLog, mgr)

	var leftNodeID string
	handler.OnLobeLeft = func(nodeID string) {
		leftNodeID = nodeID
	}

	// First, join lobe-3.
	req := mbp.JoinRequest{NodeID: "lobe-3", Addr: "127.0.0.1:9003"}
	resp := handler.HandleJoinRequest(req, nil)
	if !resp.Accepted {
		t.Fatalf("join failed: %s", resp.RejectReason)
	}
	if len(handler.Members()) != 1 {
		t.Fatalf("expected 1 member after join, got %d", len(handler.Members()))
	}

	// Now send LeaveMessage.
	leave := mbp.LeaveMessage{NodeID: "lobe-3", Epoch: 1}
	handler.HandleLeave(leave)

	// Member should be removed.
	if len(handler.Members()) != 0 {
		t.Errorf("expected 0 members after leave, got %d", len(handler.Members()))
	}

	// OnLobeLeft callback should have been called.
	if leftNodeID != "lobe-3" {
		t.Errorf("leftNodeID = %q, want lobe-3", leftNodeID)
	}

	// Peer should be removed from ConnManager.
	if _, ok := mgr.GetPeer("lobe-3"); ok {
		t.Error("lobe-3 still registered in ConnManager after leave")
	}
}

func TestJoinHandler_Members_ThreadSafe(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(2); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "", es, repLog, mgr)

	const goroutines = 10
	const joins = 20

	var wg sync.WaitGroup

	// Concurrent HandleJoinRequest calls.
	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < joins; i++ {
				nodeID := fmt.Sprintf("lobe-%d-%d", g, i)
				req := mbp.JoinRequest{
					NodeID: nodeID,
					Addr:   fmt.Sprintf("127.0.0.1:%d", 10000+g*joins+i),
				}
				handler.HandleJoinRequest(req, nil)
			}
		}()
	}

	// Concurrent Members() calls.
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < joins; i++ {
				_ = handler.Members()
			}
		}()
	}

	wg.Wait()

	// We should have goroutines*joins members.
	members := handler.Members()
	expected := goroutines * joins
	if len(members) != expected {
		t.Errorf("len(members) = %d, want %d", len(members), expected)
	}
}

// ---- JoinClient tests ----

// mockCortex runs a simple server over a net.Pipe() pair that handles a single
// JoinRequest, sends the given JoinResponse, and then returns the request it received.
func mockCortexJoin(t *testing.T, resp mbp.JoinResponse) (clientConn net.Conn, serverReq <-chan mbp.JoinRequest) {
	t.Helper()
	cConn, sConn := net.Pipe()
	reqCh := make(chan mbp.JoinRequest, 1)
	go func() {
		defer sConn.Close()
		req := readJoinRequest(t, sConn)
		reqCh <- req
		sendJoinResponse(t, sConn, resp)
	}()
	return cConn, reqCh
}

func TestJoinClient_Join_Success(t *testing.T) {
	es := newTestEpochStore(t)
	mgr := NewConnManager("lobe-1")

	client := NewJoinClient("lobe-1", "127.0.0.1:9100", "", es, nil, mgr)

	serverResp := mbp.JoinResponse{
		Accepted: true,
		CortexID: "cortex-1",
		CortexAddr: "127.0.0.1:8000",
		Epoch:    5,
	}

	clientConn, reqCh := mockCortexJoin(t, serverResp)

	resp, err := client.joinConn(context.Background(), clientConn)
	if err != nil {
		t.Fatalf("joinConn returned error: %v", err)
	}

	// Validate request sent by client.
	req := <-reqCh
	if req.NodeID != "lobe-1" {
		t.Errorf("req.NodeID = %q, want lobe-1", req.NodeID)
	}
	if req.Addr != "127.0.0.1:9100" {
		t.Errorf("req.Addr = %q, want 127.0.0.1:9100", req.Addr)
	}

	// Validate response.
	if !resp.Accepted {
		t.Fatalf("expected Accepted=true")
	}
	if resp.Epoch != 5 {
		t.Errorf("resp.Epoch = %d, want 5", resp.Epoch)
	}

	// Epoch should be updated locally.
	if got := es.Load(); got != 5 {
		t.Errorf("local epoch = %d, want 5", got)
	}

	// Cortex peer registered.
	if _, ok := mgr.GetPeer("cortex-1"); !ok {
		t.Error("cortex-1 not registered in ConnManager")
	}
}

func TestJoinClient_Join_Rejected(t *testing.T) {
	es := newTestEpochStore(t)
	mgr := NewConnManager("lobe-2")

	client := NewJoinClient("lobe-2", "127.0.0.1:9200", "", es, nil, mgr)

	serverResp := mbp.JoinResponse{
		Accepted:     false,
		RejectReason: "cluster not yet bootstrapped (epoch 0)",
		Epoch:        0,
		CortexID:     "cortex-1",
	}

	clientConn, _ := mockCortexJoin(t, serverResp)

	_, err := client.joinConn(context.Background(), clientConn)
	if err == nil {
		t.Fatal("expected error for rejected join, got nil")
	}

	// Epoch should remain 0 (not updated on rejection).
	if got := es.Load(); got != 0 {
		t.Errorf("local epoch = %d, want 0 (should not update on rejection)", got)
	}
}

func TestJoinClient_Leave(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(7); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	mgr := NewConnManager("lobe-3")
	client := NewJoinClient("lobe-3", "127.0.0.1:9300", "", es, nil, mgr)

	cConn, sConn := net.Pipe()
	defer cConn.Close()

	msgCh := make(chan mbp.LeaveMessage, 1)
	go func() {
		defer sConn.Close()
		msg := readLeaveMessage(t, sConn)
		msgCh <- msg
	}()

	if err := client.leaveConn(cConn); err != nil {
		t.Fatalf("leaveConn returned error: %v", err)
	}

	msg := <-msgCh
	if msg.NodeID != "lobe-3" {
		t.Errorf("msg.NodeID = %q, want lobe-3", msg.NodeID)
	}
	if msg.Epoch != 7 {
		t.Errorf("msg.Epoch = %d, want 7", msg.Epoch)
	}
}

// ---- Phase 2: snapshot catch-up tests ----

// mockCortexJoinWithSnapshot simulates a Cortex that:
//  1. Receives a JoinRequest
//  2. Sends a JoinResponse with NeedsSnapshot=true and SnapshotSeq=snapshotSeq
//  3. Streams a snapshot (header + chunk + complete) of the provided KV pairs
//  4. Optionally appends catch-up replication entries after the snapshot
func mockCortexJoinWithSnapshot(
	t *testing.T,
	db *pebble.DB,
	repLog *ReplicationLog,
	snapshotSeq uint64,
	epoch uint64,
) (clientConn net.Conn, done <-chan struct{}) {
	t.Helper()
	cConn, sConn := net.Pipe()
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		defer sConn.Close()

		// Read JoinRequest
		frame, err := mbp.ReadFrame(sConn)
		if err != nil {
			t.Errorf("mockCortex: read JoinRequest: %v", err)
			return
		}
		if frame.Type != mbp.TypeJoinRequest {
			t.Errorf("mockCortex: expected TypeJoinRequest, got 0x%02x", frame.Type)
			return
		}

		// Send JoinResponse with NeedsSnapshot=true
		resp := mbp.JoinResponse{
			Accepted:      true,
			CortexID:      "cortex-mock",
			Epoch:         epoch,
			NeedsSnapshot: true,
			SnapshotSeq:   snapshotSeq,
		}
		respPayload, _ := msgpack.Marshal(resp)
		respFrame := &mbp.Frame{
			Version:       0x01,
			Type:          mbp.TypeJoinResponse,
			PayloadLength: uint32(len(respPayload)),
			Payload:       respPayload,
		}
		if err := mbp.WriteFrame(sConn, respFrame); err != nil {
			t.Errorf("mockCortex: write JoinResponse: %v", err)
			return
		}

		// Stream snapshot using real SnapshotSender
		peer := &PeerConn{nodeID: "lobe-mock", addr: "pipe", conn: sConn}
		sender := NewSnapshotSender(db, repLog)
		if _, err := sender.Send(context.Background(), peer); err != nil {
			t.Errorf("mockCortex: snapshot send: %v", err)
		}
	}()

	return cConn, doneCh
}

// TestJoinClient_WithSnapshot_CatchesUp verifies that when Cortex sends
// NeedsSnapshot=true, the Lobe receives the snapshot and StreamFromSeq equals
// the snapshotSeq from the snapshot header.
func TestJoinClient_WithSnapshot_CatchesUp(t *testing.T) {
	// Cortex side: DB with some pre-written KV entries
	cortexDB := newTestDB(t)
	cortexRepLog := NewReplicationLog(cortexDB)

	// Write a couple of entries to cortex so snapshot has data and seq > 0
	if err := cortexDB.Set([]byte("key1"), []byte("val1"), pebble.Sync); err != nil {
		t.Fatalf("cortex db set: %v", err)
	}
	seq1, err := cortexRepLog.Append(OpSet, []byte("key1"), []byte("val1"))
	if err != nil {
		t.Fatalf("replog append: %v", err)
	}
	seq2, err := cortexRepLog.Append(OpSet, []byte("key2"), []byte("val2"))
	if err != nil {
		t.Fatalf("replog append: %v", err)
	}
	_ = seq1
	_ = seq2

	// snapshotSeq is the seq at which we took the snapshot
	snapshotSeq := cortexRepLog.CurrentSeq()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	clientConn, done := mockCortexJoinWithSnapshot(t, cortexDB, cortexRepLog, snapshotSeq, 3)

	// Lobe side: new empty DB + JoinClient with DB for snapshot reception
	lobeDB := newTestDB(t)
	lobeES := newTestEpochStore(t)
	lobeMgr := NewConnManager("lobe-snap")
	lobeApplier := NewApplier(lobeDB)
	client := NewJoinClientWithDB("lobe-snap", "127.0.0.1:9400", "", lobeES, lobeApplier, lobeDB, lobeMgr)

	result, err := client.joinConn(ctx, clientConn)
	if err != nil {
		t.Fatalf("joinConn returned error: %v", err)
	}

	// Wait for mock cortex goroutine to finish
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("mock cortex goroutine did not complete in time")
	}

	// The result should indicate a snapshot was received
	if !result.NeedsSnapshot {
		t.Error("expected NeedsSnapshot=true in result")
	}

	// StreamFromSeq should equal the snapshotSeq from the snapshot header
	if result.StreamFromSeq != snapshotSeq {
		t.Errorf("StreamFromSeq = %d, want %d", result.StreamFromSeq, snapshotSeq)
	}

	// Lobe DB should now contain the key written to cortex DB
	val, closer, err := lobeDB.Get([]byte("key1"))
	if err != nil {
		t.Fatalf("lobe db get key1: %v", err)
	}
	if string(val) != "val1" {
		t.Errorf("lobe key1 = %q, want %q", val, "val1")
	}
	closer.Close()

	// Epoch should be updated
	if lobeES.Load() != 3 {
		t.Errorf("lobe epoch = %d, want 3", lobeES.Load())
	}
}

// TestJoinClient_FreshJoin_NoSnapshot verifies that when Cortex sends
// NeedsSnapshot=false (old Cortex / Phase 1 fallback), joinConn returns
// a JoinResult with StreamFromSeq == lastApplied and no snapshot is read.
func TestJoinClient_FreshJoin_NoSnapshot(t *testing.T) {
	es := newTestEpochStore(t)
	mgr := NewConnManager("lobe-nosnapshot")

	// Client has no DB — should not need one when NeedsSnapshot=false
	client := NewJoinClient("lobe-nosnapshot", "127.0.0.1:9500", "", es, nil, mgr)

	serverResp := mbp.JoinResponse{
		Accepted:      true,
		CortexID:      "cortex-1",
		CortexAddr:    "127.0.0.1:8000",
		Epoch:         7,
		NeedsSnapshot: false,
	}

	clientConn, reqCh := mockCortexJoin(t, serverResp)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := client.joinConn(ctx, clientConn)
	if err != nil {
		t.Fatalf("joinConn returned error: %v", err)
	}

	// Consume server-side request channel to avoid goroutine leak
	<-reqCh

	if result.NeedsSnapshot {
		t.Error("expected NeedsSnapshot=false")
	}
	// StreamFromSeq should be lastApplied (0 since applier is nil)
	if result.StreamFromSeq != 0 {
		t.Errorf("StreamFromSeq = %d, want 0", result.StreamFromSeq)
	}
	if result.Epoch != 7 {
		t.Errorf("result.Epoch = %d, want 7", result.Epoch)
	}
	if es.Load() != 7 {
		t.Errorf("local epoch = %d, want 7", es.Load())
	}
}

// ---- HMAC cluster secret tests ----

// computeHMAC is a test helper that computes HMAC-SHA256(nodeID) using secret.
func computeHMAC(secret, nodeID string) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(nodeID))
	return h.Sum(nil)
}

// TestJoinHandler_HMACSecretMismatch_Rejected verifies that a JoinRequest with
// a SecretHash computed from the wrong secret is rejected.
func TestJoinHandler_HMACSecretMismatch_Rejected(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "correct-secret", es, repLog, mgr)

	// SecretHash computed from a DIFFERENT secret.
	req := mbp.JoinRequest{
		NodeID:     "lobe-hmac",
		Addr:       "127.0.0.1:9900",
		SecretHash: computeHMAC("wrong-secret", "lobe-hmac"),
	}

	resp := handler.HandleJoinRequest(req, nil)

	if resp.Accepted {
		t.Fatal("expected Accepted=false for wrong secret, got true")
	}
	if !strings.Contains(resp.RejectReason, "invalid cluster secret") {
		t.Errorf("RejectReason = %q, want it to contain %q", resp.RejectReason, "invalid cluster secret")
	}

	// Node must not be added to members.
	if len(handler.Members()) != 0 {
		t.Errorf("expected 0 members after rejection, got %d", len(handler.Members()))
	}
}

// TestJoinHandler_HMACSecretMatch_Accepted verifies that a JoinRequest with
// a correctly-computed SecretHash is accepted.
func TestJoinHandler_HMACSecretMatch_Accepted(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "correct-secret", es, repLog, mgr)

	// SecretHash correctly computed from "correct-secret".
	req := mbp.JoinRequest{
		NodeID:     "lobe-hmac",
		Addr:       "127.0.0.1:9901",
		SecretHash: computeHMAC("correct-secret", "lobe-hmac"),
	}

	resp := handler.HandleJoinRequest(req, nil)

	if !resp.Accepted {
		t.Fatalf("expected Accepted=true for correct secret, got false: %s", resp.RejectReason)
	}

	members := handler.Members()
	if len(members) != 1 || members[0].NodeID != "lobe-hmac" {
		t.Errorf("expected lobe-hmac in members, got %v", members)
	}
}

// TestJoinHandler_EmptySecret_AlwaysAccepted verifies that when clusterSecret
// is empty (open cluster), any SecretHash — including nil — is accepted.
func TestJoinHandler_EmptySecret_AlwaysAccepted(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	// clusterSecret = "" → open cluster, no HMAC validation.
	handler := NewJoinHandler("cortex-1", "", es, repLog, mgr)

	req := mbp.JoinRequest{
		NodeID:     "lobe-open",
		Addr:       "127.0.0.1:9902",
		SecretHash: nil, // any value, including nil
	}

	resp := handler.HandleJoinRequest(req, nil)

	if !resp.Accepted {
		t.Fatalf("expected Accepted=true for open cluster (empty secret), got false: %s", resp.RejectReason)
	}
}

// ---- Protocol version negotiation tests (rolling upgrade) ----

// TestJoinHandler_LegacyLobe_Accepted verifies that a JoinRequest with
// ProtocolVersion=0 (legacy pre-versioned Lobe) is accepted by the Cortex
// with a warning. This is critical for rolling upgrades: old Lobes will send
// version 0 (the msgpack zero-value for omitempty), and the new Cortex must
// accept them gracefully.
func TestJoinHandler_LegacyLobe_Accepted(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "", es, repLog, mgr)

	var joinedInfo NodeInfo
	handler.OnLobeJoined = func(info NodeInfo) {
		joinedInfo = info
	}

	// Legacy Lobe sends ProtocolVersion=0 (msgpack zero-value because omitempty).
	req := mbp.JoinRequest{
		NodeID:          "legacy-lobe-1",
		Addr:            "127.0.0.1:9050",
		LastApplied:     0,
		ProtocolVersion: 0,
	}

	resp := handler.HandleJoinRequest(req, nil)

	// Must be accepted.
	if !resp.Accepted {
		t.Fatalf("expected Accepted=true for legacy Lobe (proto v0), got false: %s", resp.RejectReason)
	}

	// Lobe should be registered.
	members := handler.Members()
	if len(members) != 1 {
		t.Fatalf("len(members) = %d, want 1", len(members))
	}
	if members[0].NodeID != "legacy-lobe-1" {
		t.Errorf("member NodeID = %q, want legacy-lobe-1", members[0].NodeID)
	}

	// OnLobeJoined callback should have fired.
	if joinedInfo.NodeID != "legacy-lobe-1" {
		t.Errorf("joinedInfo.NodeID = %q, want legacy-lobe-1", joinedInfo.NodeID)
	}

	// Response should include correct epoch.
	if resp.Epoch != 1 {
		t.Errorf("resp.Epoch = %d, want 1", resp.Epoch)
	}
}

// TestJoinHandler_CurrentVersionLobe_Accepted verifies that a JoinRequest with
// ProtocolVersion = CurrentProtocolVersion is accepted normally (no warning).
func TestJoinHandler_CurrentVersionLobe_Accepted(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "", es, repLog, mgr)

	// Current version Lobe sends the current protocol version.
	req := mbp.JoinRequest{
		NodeID:          "current-lobe-1",
		Addr:            "127.0.0.1:9051",
		LastApplied:     0,
		ProtocolVersion: mbp.CurrentProtocolVersion,
	}

	resp := handler.HandleJoinRequest(req, nil)

	// Must be accepted.
	if !resp.Accepted {
		t.Fatalf("expected Accepted=true for current Lobe (proto v%d), got false: %s",
			mbp.CurrentProtocolVersion, resp.RejectReason)
	}

	// Lobe should be registered.
	members := handler.Members()
	if len(members) != 1 {
		t.Fatalf("len(members) = %d, want 1", len(members))
	}
	if members[0].NodeID != "current-lobe-1" {
		t.Errorf("member NodeID = %q, want current-lobe-1", members[0].NodeID)
	}
}

// TestJoinHandler_FutureVersionLobe_Rejected verifies that a JoinRequest with
// ProtocolVersion > CurrentProtocolVersion is rejected because the Cortex cannot
// understand new protocol extensions sent by the newer Lobe.
func TestJoinHandler_FutureVersionLobe_Rejected(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "", es, repLog, mgr)

	// Future Lobe sends a version we don't support.
	futureVersion := mbp.CurrentProtocolVersion + 5
	req := mbp.JoinRequest{
		NodeID:          "future-lobe-1",
		Addr:            "127.0.0.1:9052",
		LastApplied:     0,
		ProtocolVersion: futureVersion,
	}

	resp := handler.HandleJoinRequest(req, nil)

	// Must be rejected.
	if resp.Accepted {
		t.Fatal("expected Accepted=false for future Lobe, got true")
	}

	// RejectReason must mention the version mismatch.
	if !strings.Contains(resp.RejectReason, "not supported") {
		t.Errorf("RejectReason = %q, expected to contain 'not supported'", resp.RejectReason)
	}

	// MinProtocolVersion should be set in response for client guidance.
	if resp.MinProtocolVersion != mbp.MinSupportedProtocolVersion {
		t.Errorf("MinProtocolVersion = %d, want %d",
			resp.MinProtocolVersion, mbp.MinSupportedProtocolVersion)
	}

	// Lobe should NOT be registered.
	if len(handler.Members()) != 0 {
		t.Errorf("expected 0 members after rejection, got %d", len(handler.Members()))
	}
}

// TestJoinHandler_TooOldVersionLobe_Rejected verifies that a JoinRequest with
// ProtocolVersion < MinSupportedProtocolVersion is rejected with an actionable message.
// Temporarily raises MinSupportedProtocolVersion to 1 to exercise the rejection path.
func TestJoinHandler_TooOldVersionLobe_Rejected(t *testing.T) {
	// Temporarily raise MinSupportedProtocolVersion to 1 so we can exercise the
	// rejection path. Restore after the test.
	orig := mbp.MinSupportedProtocolVersion
	mbp.MinSupportedProtocolVersion = 1
	t.Cleanup(func() { mbp.MinSupportedProtocolVersion = orig })

	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "", es, repLog, mgr)

	// Version 0 is now below MinSupportedProtocolVersion=1 → should be rejected.
	req := mbp.JoinRequest{
		NodeID:          "old-lobe-1",
		Addr:            "127.0.0.1:9053",
		LastApplied:     0,
		ProtocolVersion: 0,
	}

	resp := handler.HandleJoinRequest(req, nil)

	// Must be rejected.
	if resp.Accepted {
		t.Fatal("expected Accepted=false for too-old Lobe, got true")
	}

	// RejectReason must be actionable — mention the version is no longer supported.
	if !strings.Contains(resp.RejectReason, "no longer supported") {
		t.Errorf("RejectReason = %q, expected to contain 'no longer supported'", resp.RejectReason)
	}
	// Must include MinProtocolVersion so the operator knows what to target.
	if resp.MinProtocolVersion != 1 {
		t.Errorf("MinProtocolVersion = %d, want 1", resp.MinProtocolVersion)
	}

	// Lobe should NOT be registered.
	if len(handler.Members()) != 0 {
		t.Errorf("expected 0 members after rejection, got %d", len(handler.Members()))
	}
}

// TestJoinHandler_ProtocolVersionRollingUpgrade simulates a rolling upgrade scenario:
// Mix of legacy Lobes (v0) and current Lobes (v1) all joining the same Cortex.
// All should be accepted and registered, proving the Cortex can handle heterogeneous versions.
func TestJoinHandler_ProtocolVersionRollingUpgrade(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(5); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}

	repLog := newTestRepLog(t)
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "", es, repLog, mgr)

	// Simulate a rolling upgrade: some old Lobes (v0), some new Lobes (v1).
	lobes := []struct {
		nodeID  string
		version uint16
	}{
		{"old-lobe-1", 0},
		{"new-lobe-1", mbp.CurrentProtocolVersion},
		{"old-lobe-2", 0},
		{"new-lobe-2", mbp.CurrentProtocolVersion},
		{"old-lobe-3", 0},
	}

	for _, lobe := range lobes {
		req := mbp.JoinRequest{
			NodeID:          lobe.nodeID,
			Addr:            fmt.Sprintf("127.0.0.1:9%03d", 100+len(handler.Members())),
			LastApplied:     0,
			ProtocolVersion: lobe.version,
		}

		resp := handler.HandleJoinRequest(req, nil)

		if !resp.Accepted {
			t.Errorf("lobe %s (proto v%d) rejected: %s",
				lobe.nodeID, lobe.version, resp.RejectReason)
		}
	}

	// All 5 lobes should be registered.
	members := handler.Members()
	if len(members) != 5 {
		t.Errorf("expected 5 members after rolling upgrade join, got %d", len(members))
	}

	// Verify all nodeIDs are present.
	memberMap := make(map[string]bool)
	for _, m := range members {
		memberMap[m.NodeID] = true
	}
	for _, lobe := range lobes {
		if !memberMap[lobe.nodeID] {
			t.Errorf("lobe %s not found in members", lobe.nodeID)
		}
	}
}
