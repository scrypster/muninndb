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

// PeerHello is the authenticated peer-discovery handshake (#522 Step 4). Nodes
// with no join relationship (two primaries, sentinels, lobe↔lobe) dial each
// configured seed and exchange these to establish identified connections that
// feed MSP liveness and elections. SecretHash = HMAC-SHA256(clusterSecret,
// node_id + "\n" + addr + "\n" + string(role)) — it covers role because role
// gates voter registration on the receiving side.
type PeerHello struct {
	NodeID          string `msgpack:"node_id"`
	Addr            string `msgpack:"addr"`  // sender's advertised address
	Role            uint8  `msgpack:"role"`  // sender's NodeRole
	Epoch           uint64 `msgpack:"epoch"` // informational only (no fencing)
	SecretHash      []byte `msgpack:"secret_hash"`
	ProtocolVersion uint16 `msgpack:"proto_ver,omitempty"`
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
	Role            uint8    `msgpack:"role,omitempty"`  // joiner's NodeRole; 0 = legacy = replica (#529)
	Probe           bool     `msgpack:"probe,omitempty"` // side-effect-free leader-discovery probe (#531 PR3)
	ProtocolVersion uint16   `msgpack:"proto_ver,omitempty"`
	// LastAppliedSum is the checksum of the entry the joiner applied at
	// LastApplied (four bytes, big-endian), or empty when it has none: nothing
	// applied yet, or its cursor was not written by a build that keeps one.
	// The Cortex compares it with the entry it holds at that sequence number.
	// Additive: a Cortex without the field ignores it.
	LastAppliedSum []byte `msgpack:"last_applied_sum,omitempty"`
	// Halted says the joiner was refused by a leader before and still holds
	// the record. A Cortex then admits it only when it can confirm that the
	// joiner's last entry is its own, and refuses it otherwise (Unverified),
	// where a joiner without the record is admitted when nothing can be
	// compared. Additive: a Cortex without the field ignores it, and the
	// joiner then declines by itself (see JoinResponse.Matched).
	Halted bool `msgpack:"halted,omitempty"`
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
	// LeaderHeadSeq is the head of the Cortex's replication log when it
	// answered: the highest sequence it has numbered. Only the Cortex sets it,
	// on an accepted join and on the refusal of a joiner that is ahead of it.
	//
	// It is a pointer because absent and zero mean different things. A Cortex
	// with an empty log reports 0, and a joiner that has applied anything is
	// ahead of that. A Cortex built before this field existed reports nothing,
	// and nothing can be concluded from that. A plain uint64 would decode both
	// as 0.
	//
	// Additive: no protocol-version bump. A binary without the field ignores
	// it on decode and omits it on encode.
	LeaderHeadSeq *uint64 `msgpack:"leader_head_seq,omitempty"`
	// Diverged is set on a refusal the Cortex gives because the joiner holds
	// an entry the Cortex does not: its position is past the Cortex's log
	// head, or its entry at that position is not the Cortex's. The joiner
	// treats it as a condition only an operator can clear, not as a join to
	// retry. Additive: a joiner without the field sees an ordinary rejection.
	Diverged bool `msgpack:"diverged,omitempty"`
	// Matched is set on an accepted join when the Cortex compared the
	// joiner's last entry with its own and they are the same entry. Absent
	// means nothing was confirmed: the Cortex could not compare, or it is a
	// build that does not. A joiner that holds a refusal record follows only
	// on Matched or a snapshot. Additive.
	Matched bool `msgpack:"matched,omitempty"`
	// Unverified is set on the refusal of a joiner that said it had been
	// refused before (JoinRequest.Halted), when the Cortex cannot confirm that
	// the joiner's last entry is its own. It is not a new proof that the two
	// differ. The earlier refusal stands. Additive: a joiner without the field
	// sees an ordinary rejection.
	Unverified bool `msgpack:"unverified,omitempty"`
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
