package mbp

import (
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := msgpack.Marshal(v)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	return b
}

func mustUnmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := msgpack.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
}

func TestRoundTrip_VoteRequest(t *testing.T) {
	orig := VoteRequest{
		Epoch:       42,
		CandidateID: "node-1",
		LastSeq:     1000,
		ConfigEpoch: 7,
	}
	var got VoteRequest
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_VoteResponse(t *testing.T) {
	orig := VoteResponse{
		Epoch:   42,
		VoterID: "node-2",
		Granted: true,
	}
	var got VoteResponse
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_CortexClaim(t *testing.T) {
	orig := CortexClaim{
		Epoch:        5,
		FencingToken: 999,
		CortexID:     "node-1",
		CortexAddr:   "10.0.0.1:7700",
	}
	var got CortexClaim
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_ReplEntry(t *testing.T) {
	orig := ReplEntry{
		Seq:         100,
		Op:          2,
		Key:         []byte("mykey"),
		Value:       []byte("myvalue"),
		TimestampNS: 1700000000000000000,
	}
	var got ReplEntry
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.Seq != orig.Seq || got.Op != orig.Op || got.TimestampNS != orig.TimestampNS {
		t.Errorf("scalar mismatch: got %+v, want %+v", got, orig)
	}
	if string(got.Key) != string(orig.Key) {
		t.Errorf("Key mismatch: got %q, want %q", got.Key, orig.Key)
	}
	if string(got.Value) != string(orig.Value) {
		t.Errorf("Value mismatch: got %q, want %q", got.Value, orig.Value)
	}
}

func TestRoundTrip_ReplBatch(t *testing.T) {
	orig := ReplBatch{
		Entries: []ReplEntry{
			{Seq: 1, Op: 1, Key: []byte("k1"), Value: []byte("v1"), TimestampNS: 100},
			{Seq: 2, Op: 2, Key: []byte("k2"), Value: []byte("v2"), TimestampNS: 200},
			{Seq: 3, Op: 3, Key: []byte("k3"), Value: []byte("v3"), TimestampNS: 300},
		},
	}
	var got ReplBatch
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if len(got.Entries) != len(orig.Entries) {
		t.Fatalf("Entries length mismatch: got %d, want %d", len(got.Entries), len(orig.Entries))
	}
	for i, e := range orig.Entries {
		g := got.Entries[i]
		if g.Seq != e.Seq || g.Op != e.Op || g.TimestampNS != e.TimestampNS {
			t.Errorf("entry[%d] scalar mismatch: got %+v, want %+v", i, g, e)
		}
		if string(g.Key) != string(e.Key) || string(g.Value) != string(e.Value) {
			t.Errorf("entry[%d] KV mismatch", i)
		}
	}
}

func TestRoundTrip_ReplAck(t *testing.T) {
	orig := ReplAck{LastSeq: 500, NodeID: "node-3"}
	var got ReplAck
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_ReplNack(t *testing.T) {
	orig := ReplNack{FromSeq: 400, NodeID: "node-4"}
	var got ReplNack
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_GossipMember(t *testing.T) {
	orig := GossipMember{
		ID:       "node-5",
		Addr:     "10.0.0.5:7700",
		Role:     2,
		LastSeq:  888,
		LastSeen: 1700000001,
	}
	var got GossipMember
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_GossipMessage(t *testing.T) {
	orig := GossipMessage{
		SenderID: "node-1",
		Epoch:    10,
		Members: []GossipMember{
			{ID: "node-1", Addr: "10.0.0.1:7700", Role: 1, LastSeq: 100, LastSeen: 1000},
			{ID: "node-2", Addr: "10.0.0.2:7700", Role: 2, LastSeq: 200, LastSeen: 2000},
			{ID: "node-3", Addr: "10.0.0.3:7700", Role: 2, LastSeq: 300, LastSeen: 3000},
		},
	}
	var got GossipMessage
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.SenderID != orig.SenderID || got.Epoch != orig.Epoch {
		t.Errorf("scalar mismatch: got %+v, want %+v", got, orig)
	}
	if len(got.Members) != len(orig.Members) {
		t.Fatalf("Members length mismatch: got %d, want %d", len(got.Members), len(orig.Members))
	}
	for i, m := range orig.Members {
		if got.Members[i] != m {
			t.Errorf("Members[%d] mismatch: got %+v, want %+v", i, got.Members[i], m)
		}
	}
}

func TestRoundTrip_JoinRequest(t *testing.T) {
	orig := JoinRequest{
		NodeID:       "node-6",
		Addr:         "10.0.0.6:7700",
		LastApplied:  750,
		Capabilities: []string{"replication", "snapshot", "gossip"},
		SecretHash:   []byte{0xde, 0xad, 0xbe, 0xef},
	}
	var got JoinRequest
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.NodeID != orig.NodeID || got.Addr != orig.Addr || got.LastApplied != orig.LastApplied {
		t.Errorf("scalar mismatch: got %+v, want %+v", got, orig)
	}
	if len(got.Capabilities) != len(orig.Capabilities) {
		t.Fatalf("Capabilities length mismatch")
	}
	for i, c := range orig.Capabilities {
		if got.Capabilities[i] != c {
			t.Errorf("Capabilities[%d] mismatch: got %q, want %q", i, got.Capabilities[i], c)
		}
	}
	if string(got.SecretHash) != string(orig.SecretHash) {
		t.Errorf("SecretHash mismatch")
	}
}

func TestRoundTrip_JoinResponse_Accepted(t *testing.T) {
	orig := JoinResponse{
		Accepted:   true,
		CortexID:   "node-1",
		CortexAddr: "10.0.0.1:7700",
		Epoch:      10,
		Members: []GossipMember{
			{ID: "node-1", Addr: "10.0.0.1:7700", Role: 1, LastSeq: 100, LastSeen: 1000},
			{ID: "node-2", Addr: "10.0.0.2:7700", Role: 2, LastSeq: 200, LastSeen: 2000},
		},
		RejectReason: "",
	}
	var got JoinResponse
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.Accepted != orig.Accepted || got.CortexID != orig.CortexID ||
		got.CortexAddr != orig.CortexAddr || got.Epoch != orig.Epoch || got.RejectReason != orig.RejectReason {
		t.Errorf("scalar mismatch: got %+v, want %+v", got, orig)
	}
	if len(got.Members) != len(orig.Members) {
		t.Fatalf("Members length mismatch")
	}
	for i, m := range orig.Members {
		if got.Members[i] != m {
			t.Errorf("Members[%d] mismatch", i)
		}
	}
}

func TestRoundTrip_JoinResponse_Rejected(t *testing.T) {
	orig := JoinResponse{
		Accepted:     false,
		RejectReason: "cluster at capacity",
		Epoch:        10,
	}
	var got JoinResponse
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.Accepted != orig.Accepted || got.RejectReason != orig.RejectReason || got.Epoch != orig.Epoch {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_SDownNotification(t *testing.T) {
	orig := SDownNotification{
		SenderID:  "node-1",
		TargetID:  "node-3",
		Epoch:     15,
		Timestamp: 1700000009,
	}
	var got SDownNotification
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_LeaveMessage(t *testing.T) {
	orig := LeaveMessage{NodeID: "node-7", Epoch: 20}
	var got LeaveMessage
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_SnapHeader(t *testing.T) {
	orig := SnapHeader{
		SnapshotSeq: 50,
		NodeID:      "node-1",
		TotalKeys:   10000,
		Timestamp:   1700000005,
	}
	var got SnapHeader
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_SnapChunk(t *testing.T) {
	orig := SnapChunk{
		ChunkNum:  3,
		LastChunk: false,
		Pairs: []KVPair{
			{Key: []byte("a"), Value: []byte("1")},
			{Key: []byte("b"), Value: []byte("2")},
			{Key: []byte("c"), Value: []byte("3")},
		},
	}
	var got SnapChunk
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.ChunkNum != orig.ChunkNum || got.LastChunk != orig.LastChunk {
		t.Errorf("scalar mismatch: got %+v, want %+v", got, orig)
	}
	if len(got.Pairs) != len(orig.Pairs) {
		t.Fatalf("Pairs length mismatch")
	}
	for i, p := range orig.Pairs {
		if string(got.Pairs[i].Key) != string(p.Key) || string(got.Pairs[i].Value) != string(p.Value) {
			t.Errorf("Pairs[%d] mismatch", i)
		}
	}
}

func TestRoundTrip_KVPair(t *testing.T) {
	orig := KVPair{Key: []byte("hello"), Value: []byte("world")}
	var got KVPair
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if string(got.Key) != string(orig.Key) || string(got.Value) != string(orig.Value) {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

// TestCognitiveSideEffect_RoundTrip serializes a CognitiveSideEffect with 20
// CoActivationRefs and 20 AccessedIDs, then unmarshals and verifies equality.
func TestCognitiveSideEffect_RoundTrip(t *testing.T) {
	const n = 20
	cas := make([]CoActivationRef, n)
	for i := 0; i < n; i++ {
		var id [16]byte
		id[0] = byte(i + 1)
		id[15] = byte(i + 1)
		cas[i] = CoActivationRef{ID: id, Score: float64(i) * 0.05}
	}

	aids := make([][16]byte, n)
	for i := 0; i < n; i++ {
		var id [16]byte
		id[0] = byte(100 + i)
		id[15] = byte(100 + i)
		aids[i] = id
	}

	orig := CognitiveSideEffect{
		QueryID:       "round-trip-qid",
		OriginNodeID:  "node-rt",
		Timestamp:     1700000099,
		CoActivations: cas,
		AccessedIDs:   aids,
	}

	var got CognitiveSideEffect
	mustUnmarshal(t, mustMarshal(t, orig), &got)

	if got.QueryID != orig.QueryID || got.OriginNodeID != orig.OriginNodeID || got.Timestamp != orig.Timestamp {
		t.Errorf("scalar mismatch: got %+v, want %+v", got, orig)
	}
	if len(got.CoActivations) != n {
		t.Fatalf("CoActivations length: got %d, want %d", len(got.CoActivations), n)
	}
	for i := range orig.CoActivations {
		if got.CoActivations[i] != orig.CoActivations[i] {
			t.Errorf("CoActivations[%d] mismatch: got %+v, want %+v", i, got.CoActivations[i], orig.CoActivations[i])
		}
	}
	if len(got.AccessedIDs) != n {
		t.Fatalf("AccessedIDs length: got %d, want %d", len(got.AccessedIDs), n)
	}
	for i := range orig.AccessedIDs {
		if got.AccessedIDs[i] != orig.AccessedIDs[i] {
			t.Errorf("AccessedIDs[%d] mismatch: got %v, want %v", i, got.AccessedIDs[i], orig.AccessedIDs[i])
		}
	}
}

func TestRoundTrip_CoActivationRef(t *testing.T) {
	orig := CoActivationRef{
		ID:    [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Score: 0.987,
	}
	var got CoActivationRef
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.ID != orig.ID || got.Score != orig.Score {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_CognitiveSideEffect(t *testing.T) {
	orig := CognitiveSideEffect{
		QueryID:      "qid-abc",
		OriginNodeID: "node-2",
		Timestamp:    1700000010,
		CoActivations: []CoActivationRef{
			{ID: [16]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, Score: 0.9},
			{ID: [16]byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}, Score: 0.8},
			{ID: [16]byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3}, Score: 0.7},
			{ID: [16]byte{4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4}, Score: 0.6},
			{ID: [16]byte{5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}, Score: 0.5},
		},
		AccessedIDs: [][16]byte{
			{10, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 10},
			{20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 20},
			{30, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 30},
			{40, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 40},
			{50, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 50},
		},
	}
	var got CognitiveSideEffect
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.QueryID != orig.QueryID || got.OriginNodeID != orig.OriginNodeID || got.Timestamp != orig.Timestamp {
		t.Errorf("scalar mismatch: got %+v, want %+v", got, orig)
	}
	if len(got.CoActivations) != len(orig.CoActivations) {
		t.Fatalf("CoActivations length mismatch: got %d, want %d", len(got.CoActivations), len(orig.CoActivations))
	}
	for i, ca := range orig.CoActivations {
		if got.CoActivations[i] != ca {
			t.Errorf("CoActivations[%d] mismatch: got %+v, want %+v", i, got.CoActivations[i], ca)
		}
	}
	if len(got.AccessedIDs) != len(orig.AccessedIDs) {
		t.Fatalf("AccessedIDs length mismatch: got %d, want %d", len(got.AccessedIDs), len(orig.AccessedIDs))
	}
	for i, id := range orig.AccessedIDs {
		if got.AccessedIDs[i] != id {
			t.Errorf("AccessedIDs[%d] mismatch: got %v, want %v", i, got.AccessedIDs[i], id)
		}
	}
}

func TestCognitiveSideEffect_ArchiveRestore_RoundTrip(t *testing.T) {
	orig := CognitiveSideEffect{
		QueryID:      "test-query",
		OriginNodeID: "node-1",
		Timestamp:    time.Now().UnixNano(),
		ArchivedEdges: []EdgeRef{
			{Src: [16]byte{1}, Dst: [16]byte{2}},
		},
		RestoredEdges: []EdgeRef{
			{Src: [16]byte{3}, Dst: [16]byte{4}},
		},
	}

	data, err := msgpack.Marshal(&orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got CognitiveSideEffect
	if err = msgpack.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got.ArchivedEdges) != 1 {
		t.Errorf("ArchivedEdges: got %d, want 1", len(got.ArchivedEdges))
	}
	if len(got.RestoredEdges) != 1 {
		t.Errorf("RestoredEdges: got %d, want 1", len(got.RestoredEdges))
	}
}

func TestRoundTrip_CogAck(t *testing.T) {
	orig := CogAck{QueryID: "qid-xyz"}
	var got CogAck
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("mismatch: got %+v, want %+v", got, orig)
	}
}

func TestFrameTypeConstants_NoConflicts(t *testing.T) {
	clusterTypes := map[string]uint8{
		"TypeReplEntry":    TypeReplEntry,
		"TypeReplBatch":    TypeReplBatch,
		"TypeReplAck":      TypeReplAck,
		"TypeReplNack":     TypeReplNack,
		"TypeSnapHeader":   TypeSnapHeader,
		"TypeSnapChunk":    TypeSnapChunk,
		"TypeSnapAck":      TypeSnapAck,
		"TypeSnapComplete": TypeSnapComplete,
		"TypeCogForward":   TypeCogForward,
		"TypeCogAck":       TypeCogAck,
		"TypeVoteRequest":  TypeVoteRequest,
		"TypeVoteResponse": TypeVoteResponse,
		"TypeCortexClaim":  TypeCortexClaim,
		"TypeSDown":        TypeSDown,
		"TypeODown":        TypeODown,
		"TypeGossip":       TypeGossip,
		"TypeJoinRequest":  TypeJoinRequest,
		"TypeJoinResponse": TypeJoinResponse,
		"TypeLeave":        TypeLeave,
	}

	baseTypes := map[string]uint8{
		"TypeHello":        TypeHello,
		"TypeHelloOK":      TypeHelloOK,
		"TypeWrite":        TypeWrite,
		"TypeWriteOK":      TypeWriteOK,
		"TypeRead":         TypeRead,
		"TypeReadResp":     TypeReadResp,
		"TypeActivate":     TypeActivate,
		"TypeActivateResp": TypeActivateResp,
		"TypeSubscribe":    TypeSubscribe,
		"TypeSubOK":        TypeSubOK,
		"TypeActivation":   TypeActivation,
		"TypeLink":         TypeLink,
		"TypeLinkOK":       TypeLinkOK,
		"TypeForget":       TypeForget,
		"TypeForgetOK":     TypeForgetOK,
		"TypeUnsub":        TypeUnsub,
		"TypeUnsubOK":      TypeUnsubOK,
		"TypeStat":         TypeStat,
		"TypeStatResp":     TypeStatResp,
		"TypePing":         TypePing,
		"TypePong":         TypePong,
		"TypeError":        TypeError,
	}

	// Verify cluster types are in the expected range 0x20-0x38
	for name, v := range clusterTypes {
		if v < 0x20 || v > 0x38 {
			t.Errorf("%s = 0x%02x is outside expected cluster range [0x20, 0x38]", name, v)
		}
	}

	// Verify cluster types do not overlap with base types
	seen := make(map[uint8]string)
	for name, v := range baseTypes {
		seen[v] = name
	}
	for name, v := range clusterTypes {
		if conflict, exists := seen[v]; exists {
			t.Errorf("%s = 0x%02x conflicts with %s", name, v, conflict)
		}
		seen[v] = name
	}

	// Verify no duplicates within cluster types themselves
	clusterSeen := make(map[uint8]string)
	for name, v := range clusterTypes {
		if conflict, exists := clusterSeen[v]; exists {
			t.Errorf("%s = 0x%02x duplicates %s", name, v, conflict)
		}
		clusterSeen[v] = name
	}
}
