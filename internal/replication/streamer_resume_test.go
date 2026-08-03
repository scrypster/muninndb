package replication

import (
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/vmihailenco/msgpack/v5"
)

// appendEntries fills a replication log with n OpSet entries.
func appendEntries(t *testing.T, log *ReplicationLog, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := log.Append(OpSet, []byte(fmt.Sprintf("k%03d", i)), []byte("v")); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// decodeReplEntries pulls the ReplEntry sequence numbers off the wire.
func decodeReplEntries(t *testing.T, frames []*mbp.Frame) []uint64 {
	t.Helper()
	var seqs []uint64
	for _, f := range frames {
		if f.Type != mbp.TypeReplEntry {
			continue
		}
		var e mbp.ReplEntry
		if err := msgpack.Unmarshal(f.Payload, &e); err != nil {
			t.Fatalf("unmarshal ReplEntry: %v", err)
		}
		seqs = append(seqs, e.Seq)
	}
	return seqs
}

// TestStartStreamerForLobe_ResumesFromAckedPosition is the other half of the
// #627 spiral. A Lobe that has acked seq 40 out of 50 needs 10 entries. Before
// the fix the Cortex restarted every stream at seq 0, so each reconnect re-sent
// the whole log — the transfer volume tracked the log size, not the lag, and a
// Lobe that could not finish one full pass could never finish any.
func TestStartStreamerForLobe_ResumesFromAckedPosition(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")

	const (
		total  = 50
		acked  = 40
		expect = total - acked
	)
	appendEntries(t, coord.repLog, total)
	coord.UpdateReplicaSeq("lobe-behind", acked)

	conn := newPacedConn(1<<20, expect) // absorbs whole frames; never stalls
	defer conn.Close()
	coord.mgr.RegisterConn("lobe-behind", "127.0.0.1:19999", conn)

	coord.startStreamerForLobe(NodeInfo{NodeID: "lobe-behind", Addr: "127.0.0.1:19999", Role: RoleReplica})
	defer coord.stopStreamerForLobe("lobe-behind")

	select {
	case <-conn.Reached():
	case <-time.After(10 * time.Second):
		t.Fatalf("only %d of %d expected frames arrived", conn.FrameCount(), expect)
	}

	seqs := decodeReplEntries(t, conn.Frames())
	if len(seqs) == 0 {
		t.Fatal("no ReplEntry frames on the wire")
	}
	if seqs[0] != acked+1 {
		t.Fatalf("stream resumed at seq %d, want %d — the whole log was re-sent (%d entries)",
			seqs[0], acked+1, len(seqs))
	}
	if len(seqs) != expect {
		t.Fatalf("streamed %d entries, want exactly the %d-entry backlog", len(seqs), expect)
	}
}

// TestStreamStartSeq_PrefersSnapshotOverStaleAck covers the case the ack alone
// cannot: the snapshot receiver does not advance the Lobe's Applier.lastApplied,
// so a Lobe that has just been handed a complete database still acks a low seq.
// Resuming from the ack there would re-stream everything the snapshot contained.
func TestStreamStartSeq_PrefersSnapshotOverStaleAck(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")

	coord.UpdateReplicaSeq("fresh-lobe", 3)
	coord.snapshotSeqs.Store("fresh-lobe", uint64(900))
	if got := coord.streamStartSeq("fresh-lobe"); got != 900 {
		t.Fatalf("streamStartSeq with snapshot 900 / ack 3: got %d, want 900", got)
	}

	// Once the Lobe overtakes the snapshot point, the ack is authoritative again.
	coord.UpdateReplicaSeq("fresh-lobe", 1200)
	if got := coord.streamStartSeq("fresh-lobe"); got != 1200 {
		t.Fatalf("streamStartSeq with snapshot 900 / ack 1200: got %d, want 1200", got)
	}

	// An unknown node must fall back to 0: replay is idempotent, skipping is not.
	if got := coord.streamStartSeq("never-seen"); got != 0 {
		t.Fatalf("streamStartSeq for an unknown node: got %d, want 0", got)
	}
}
