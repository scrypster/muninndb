package rest

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/replication"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// refusingLeader is a listener that answers every join request the way a
// leader answers a replica that has applied past its log head.
func refusingLeader(t *testing.T, head uint64) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				frame, err := mbp.ReadFrame(conn)
				if err != nil || frame.Type != mbp.TypeJoinRequest {
					return // a discovery hello: this stand-in is not a peer
				}
				payload, err := msgpack.Marshal(mbp.JoinResponse{
					Accepted:      false,
					CortexID:      "node-leader",
					Epoch:         2,
					RejectReason:  "lobe ahead of cortex: invented",
					LeaderHeadSeq: &head,
				})
				if err != nil {
					return
				}
				_ = mbp.WriteFrame(conn, &mbp.Frame{
					Version: 0x01, Type: mbp.TypeJoinResponse,
					PayloadLength: uint32(len(payload)), Payload: payload,
				})
			}()
		}
	}()
	return ln.Addr().String()
}

// TestClusterHealth_ReplicaThatRefusedItsLeaderIsDown: a replica that will not
// follow its leader replicates nothing, and its lag still reads 0, because the
// lag is measured against the replica's own log. Before this the health probe
// answered 200 "ok" for it. It must answer 503 and say why.
func TestClusterHealth_ReplicaThatRefusedItsLeaderIsDown(t *testing.T) {
	const secret = "invented-test-cluster-secret"

	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	applier := replication.NewApplier(db)
	// This replica has applied through seq 5; the leader below says its log
	// ends at 3.
	if err := applier.Apply(replication.ReplicationEntry{Seq: 5, Op: replication.OpSet, Key: []byte("probe/k"), Value: []byte("invented")}); err != nil {
		t.Fatalf("seed applied position: %v", err)
	}
	epochStore, err := replication.NewEpochStore(db)
	if err != nil {
		t.Fatalf("new epoch store: %v", err)
	}
	coord := replication.NewClusterCoordinator(&config.ClusterConfig{
		Enabled:       true,
		NodeID:        "node-replica",
		BindAddr:      "127.0.0.1:0",
		Seeds:         []string{refusingLeader(t, 3)},
		Role:          "replica",
		LeaseTTL:      10,
		HeartbeatMS:   1000,
		ClusterSecret: secret,
	}, replication.NewReplicationLog(db), applier, epochStore)

	srv := newTestServer(t, nil)
	srv.SetCoordinator(coord)
	health := func() (int, clusterHealthResponse) {
		t.Helper()
		req := httptest.NewRequest("GET", "/v1/cluster/health", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		var resp clusterHealthResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode health: %v", err)
		}
		return w.Code, resp
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = coord.Run(ctx) }()
	// Stop the coordinator's goroutines before the store closes (cleanups run
	// last-in-first-out, and the store's was registered first).
	t.Cleanup(func() { cancel(); _ = coord.Stop(); <-done })

	// Wait for the refusal itself, not for a duration: the coordinator reports
	// the halt as soon as its first join is turned away.
	deadline := time.Now().Add(60 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, halted := coord.ReplicationHalt(); halted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the replica never reported that it refused its leader")
		}
		<-tick.C
	}

	code, resp := health()
	if code != http.StatusServiceUnavailable {
		t.Errorf("HTTP status = %d, want 503 for a replica that has stopped replicating", code)
	}
	if resp.Status != "down" {
		t.Errorf("status = %q, want down", resp.Status)
	}
	if resp.Role != "replica" {
		t.Errorf("role = %q, want replica", resp.Role)
	}
	if resp.ReplicationLag != 0 {
		t.Errorf("replication_lag = %d; this test expects the lag to read 0, which is why the status cannot come from it", resp.ReplicationLag)
	}
	for _, want := range []string{"5", "3", "node-leader", "docs/cluster-operations.md"} {
		if !strings.Contains(resp.Reason, want) {
			t.Errorf("reason does not mention %q: %s", want, resp.Reason)
		}
	}
}

// silentSeed is a listener that accepts connections and never answers them: a
// leader this node has not managed to talk to yet.
func silentSeed(t *testing.T) (addr string, closeAll func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	return ln.Addr().String(), func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	}
}

// TestClusterHealth_ARefusedReplicaIsDownFromProcessStart: a replica that was
// refused before it was restarted has not talked to any leader yet in this
// process, and may not manage to for a while. Its health must not read "ok" in
// the meantime. The refusal is on its disk, and so is the reason, with both of
// the operator's choices in it.
func TestClusterHealth_ARefusedReplicaIsDownFromProcessStart(t *testing.T) {
	const secret = "invented-test-cluster-secret"

	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The process that was refused.
	before := replication.NewApplier(db)
	if err := before.Apply(replication.ReplicationEntry{Seq: 5, Op: replication.OpSet, Key: []byte("probe/k"), Value: []byte("invented")}); err != nil {
		t.Fatalf("seed applied position: %v", err)
	}
	const why = `this node's entry at seq 5 is not the entry cortex "node-leader" (epoch 2) holds at that position (cortex log head 9)`
	if err := before.SetHalt(replication.HaltRecord{LeaderID: "node-leader", Epoch: 2, LastApplied: 5, LeaderHead: 9, Why: why, At: 1_700_000_000}); err != nil {
		t.Fatalf("record the refusal: %v", err)
	}

	// The process after the restart: everything is rebuilt from the store.
	epochStore, err := replication.NewEpochStore(db)
	if err != nil {
		t.Fatalf("new epoch store: %v", err)
	}
	seed, closeSeed := silentSeed(t)
	coord := replication.NewClusterCoordinator(&config.ClusterConfig{
		Enabled:       true,
		NodeID:        "node-replica",
		BindAddr:      "127.0.0.1:0",
		Seeds:         []string{seed},
		Role:          "replica",
		LeaseTTL:      10,
		HeartbeatMS:   1000,
		ClusterSecret: secret,
	}, replication.NewReplicationLog(db), replication.NewApplier(db), epochStore)

	srv := newTestServer(t, nil)
	srv.SetCoordinator(coord)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = coord.Run(ctx) }()
	// The join attempt in flight is blocked on the seed, so the seed goes
	// first. Then the coordinator's goroutines, before the store closes.
	t.Cleanup(func() { cancel(); closeSeed(); _ = coord.Stop(); <-done })

	// Wait until the node is a replica, which it is as soon as it starts and
	// well before any leader has answered it. Without the stored refusal a
	// replica with lag 0 answers 200 "ok" at this point.
	deadline := time.Now().Add(60 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for coord.Role() != replication.RoleReplica {
		if time.Now().After(deadline) {
			t.Fatal("the node never took the replica role")
		}
		<-tick.C
	}

	req := httptest.NewRequest("GET", "/v1/cluster/health", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	var resp clusterHealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if w.Code != http.StatusServiceUnavailable || resp.Status != "down" {
		t.Fatalf("health = HTTP %d status %q, want 503 down: this replica was refused before it restarted and has replicated nothing since", w.Code, resp.Status)
	}
	if resp.Role != "replica" || resp.ReplicationLag != 0 {
		t.Errorf("role = %q, replication_lag = %d; this test expects a replica whose lag reads 0, so that the status can only come from the stored refusal", resp.Role, resp.ReplicationLag)
	}
	for _, want := range []string{why, "two choices", "re-seed this replica from the Cortex", "make this replica the Cortex", "docs/cluster-operations.md"} {
		if !strings.Contains(resp.Reason, want) {
			t.Errorf("reason does not say %q: %s", want, resp.Reason)
		}
	}
}

// TestClusterHealth_NoReasonWhenReplicating: the control. A node that is not
// halted answers as it always did, with no reason field.
func TestClusterHealth_NoReasonWhenReplicating(t *testing.T) {
	const secret = "test-cluster-secret"
	srv := newTestServer(t, nil)
	coord := newTestCoordinatorWithSecret(t, secret)
	srv.SetCoordinator(coord)
	_ = coord.Election().StartElection(context.Background())

	req := httptest.NewRequest("GET", "/v1/cluster/health", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "reason") {
		t.Errorf("a healthy node's response carries a reason: %s", w.Body.String())
	}
}
