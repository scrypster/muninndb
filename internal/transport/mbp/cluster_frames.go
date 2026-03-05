package mbp

// VoteRequest is sent by a candidate during leader election.
type VoteRequest struct {
	Epoch       uint64 `msgpack:"epoch"`
	CandidateID string `msgpack:"candidate_id"`
	LastSeq     uint64 `msgpack:"last_seq"`
	ConfigEpoch uint64 `msgpack:"config_epoch"`
}

// VoteResponse is the reply to a VoteRequest.
type VoteResponse struct {
	Epoch   uint64 `msgpack:"epoch"`
	VoterID string `msgpack:"voter_id"`
	Granted bool   `msgpack:"granted"`
}

// CortexClaim is broadcast by the election winner.
type CortexClaim struct {
	Epoch        uint64 `msgpack:"epoch"`
	FencingToken uint64 `msgpack:"fencing_token"`
	CortexID     string `msgpack:"cortex_id"`
	CortexAddr   string `msgpack:"cortex_addr"`
}

// ReplEntry is a single entry in the replication stream.
type ReplEntry struct {
	Seq         uint64 `msgpack:"seq"`
	Op          uint8  `msgpack:"op"`
	Key         []byte `msgpack:"key"`
	Value       []byte `msgpack:"value"`
	TimestampNS int64  `msgpack:"ts"`
}

// ReplBatch is a batch of replication entries.
type ReplBatch struct {
	Entries []ReplEntry `msgpack:"entries"`
}

// ReplAck acknowledges receipt of replication entries up to LastSeq.
type ReplAck struct {
	LastSeq uint64 `msgpack:"last_seq"`
	NodeID  string `msgpack:"node_id"`
}

// ReplNack requests resend from a given sequence.
type ReplNack struct {
	FromSeq uint64 `msgpack:"from_seq"`
	NodeID  string `msgpack:"node_id"`
}

// GossipMember is a single member in a gossip message.
type GossipMember struct {
	ID       string `msgpack:"id"`
	Addr     string `msgpack:"addr"`
	Role     uint8  `msgpack:"role"`
	LastSeq  uint64 `msgpack:"last_seq"`
	LastSeen int64  `msgpack:"last_seen"`
}

// GossipMessage carries cluster membership state.
type GossipMessage struct {
	SenderID string         `msgpack:"sender_id"`
	Epoch    uint64         `msgpack:"epoch"`
	Members  []GossipMember `msgpack:"members"`
}

// JoinRequest is sent by a node joining the cluster.
type JoinRequest struct {
	NodeID          string   `msgpack:"node_id"`
	Addr            string   `msgpack:"addr"`
	LastApplied     uint64   `msgpack:"last_applied"`
	Capabilities    []string `msgpack:"capabilities"`
	SecretHash      []byte   `msgpack:"secret_hash"`
	ProtocolVersion uint16   `msgpack:"proto_ver,omitempty"`
}

// JoinResponse is the cluster's reply to a join request.
type JoinResponse struct {
	Accepted     bool           `msgpack:"accepted"`
	CortexID     string         `msgpack:"cortex_id"`
	CortexAddr   string         `msgpack:"cortex_addr"`
	Epoch        uint64         `msgpack:"epoch"`
	Members      []GossipMember `msgpack:"members"`
	RejectReason string         `msgpack:"reject_reason,omitempty"`
	// NeedsSnapshot is true when the Cortex will stream a full Pebble snapshot
	// immediately after this response. The Lobe must receive the snapshot before
	// starting normal NetworkStreamer replication.
	NeedsSnapshot bool `msgpack:"needs_snapshot,omitempty"`
	// SnapshotSeq is the Cortex's replication log seq at snapshot time.
	// Entries after this seq were written during the transfer and must be
	// caught up via NetworkStreamer starting from SnapshotSeq+1.
	SnapshotSeq            uint64 `msgpack:"snapshot_seq,omitempty"`
	MinProtocolVersion     uint16 `msgpack:"min_proto_ver,omitempty"`
	CurrentProtocolVersion uint16 `msgpack:"current_proto_ver,omitempty"`
}

// SDownNotification is sent when a node marks another as subjectively down.
type SDownNotification struct {
	SenderID  string `msgpack:"sender_id"`
	TargetID  string `msgpack:"target_id"`
	Epoch     uint64 `msgpack:"epoch"`
	Timestamp int64  `msgpack:"timestamp"`
}

// LeaveMessage is sent by a node gracefully departing the cluster.
type LeaveMessage struct {
	NodeID string `msgpack:"node_id"`
	Epoch  uint64 `msgpack:"epoch"`
}

// SnapHeader is the first frame in a snapshot transfer.
type SnapHeader struct {
	SnapshotSeq uint64 `msgpack:"snapshot_seq"`
	NodeID      string `msgpack:"node_id"`
	TotalKeys   uint64 `msgpack:"total_keys"`
	Timestamp   int64  `msgpack:"timestamp"`
}

// SnapChunk is a chunk of key-value pairs in a snapshot transfer.
type SnapChunk struct {
	ChunkNum  uint32   `msgpack:"chunk_num"`
	LastChunk bool     `msgpack:"last_chunk"`
	Pairs     []KVPair `msgpack:"pairs"`
}

// KVPair is a single key-value pair in a snapshot chunk.
type KVPair struct {
	Key   []byte `msgpack:"key"`
	Value []byte `msgpack:"value"`
}

// EdgeRef is a (src, dst) pair identifying an association edge.
type EdgeRef struct {
	Src [16]byte `msgpack:"src"`
	Dst [16]byte `msgpack:"dst"`
}

// CognitiveSideEffect carries cognitive side effects from a Lobe activation to the Cortex.
type CognitiveSideEffect struct {
	QueryID       string            `msgpack:"query_id"`
	OriginNodeID  string            `msgpack:"origin_node_id"`
	Timestamp     int64             `msgpack:"timestamp"`
	CoActivations []CoActivationRef `msgpack:"co_activations,omitempty"`
	AccessedIDs   [][16]byte        `msgpack:"accessed_ids,omitempty"`
	ArchivedEdges []EdgeRef         `msgpack:"archived_edges,omitempty"`
	RestoredEdges []EdgeRef         `msgpack:"restored_edges,omitempty"`
}

// CoActivationRef is a pair of engram IDs that were co-activated.
type CoActivationRef struct {
	ID    [16]byte `msgpack:"id"`
	Score float64  `msgpack:"score"`
}

// CogAck acknowledges a cognitive side effect forward.
type CogAck struct {
	QueryID string `msgpack:"query_id"`
}

// HandoffMessage is sent by the Cortex to the target node during graceful failover.
type HandoffMessage struct {
	TargetID  string `msgpack:"tid"`
	Epoch     uint64 `msgpack:"epoch"`
	CortexSeq uint64 `msgpack:"seq"`
}

// HandoffAck is sent by the target node back to the Cortex to confirm handoff.
type HandoffAck struct {
	TargetID string `msgpack:"tid"`
	Epoch    uint64 `msgpack:"epoch"`
	Success  bool   `msgpack:"ok"`
}

// ReconProbeMsg is sent by Cortex to a Lobe during post-partition reconciliation.
// It contains the sampled keys and the Cortex's weights for comparison.
type ReconProbeMsg struct {
	RequestID string     `msgpack:"rid"`
	Keys      [][16]byte `msgpack:"keys"`
	Weights   []float64  `msgpack:"weights"` // Cortex's weight for each key (parallel arrays)
}

// ReconReplyMsg is sent by a Lobe back to the Cortex with its local weights.
type ReconReplyMsg struct {
	RequestID string    `msgpack:"rid"`
	NodeID    string    `msgpack:"node_id"`
	Weights   []float64 `msgpack:"weights"` // Lobe's weight for each key (parallel, 0 = missing)
}

// ReconSyncMsg is sent by Cortex to a divergent Lobe with corrected weights.
type ReconSyncMsg struct {
	RequestID string     `msgpack:"rid"`
	Keys      [][16]byte `msgpack:"keys"`
	Weights   []float64  `msgpack:"weights"`
}

// ReconAckMsg is sent by a Lobe to confirm reconciliation weights were applied.
type ReconAckMsg struct {
	RequestID string `msgpack:"rid"`
	NodeID    string `msgpack:"node_id"`
	Applied   int    `msgpack:"applied"`
}

// CCSProbeMsg is sent by Cortex to all Lobes during a CCS measurement round.
// The Lobe must compute a SHA-256 hash of the (key, weight) pairs for all
// sampled keys and return it in a CCSResponseMsg.
type CCSProbeMsg struct {
	SampledKeys [][16]byte `msgpack:"keys"` // engram IDs to hash
	RequestID   string     `msgpack:"rid"`
}

// CCSResponseMsg is sent by a Lobe back to the Cortex in reply to a CCSProbeMsg.
type CCSResponseMsg struct {
	RequestID string `msgpack:"rid"`
	NodeID    string `msgpack:"node_id"`
	Hash      []byte `msgpack:"hash"`      // SHA-256 of sorted (key,weight) pairs
	KeyCount  int    `msgpack:"key_count"` // number of keys the lobe found locally
}
