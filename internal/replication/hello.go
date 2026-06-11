package replication

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand"
	"net"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// helloHMAC computes the PeerHello SecretHash over node-id, addr, and role.
// Role is covered because it gates voter registration on the receiver (#522 Step 4).
func helloHMAC(secret, nodeID, addr string, role uint8) []byte {
	if secret == "" {
		return nil
	}
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(nodeID + "\n" + addr + "\n"))
	h.Write([]byte{role})
	return h.Sum(nil)
}

// roleFromConfigString maps a config role string to a NodeRole. "auto" (and any
// unknown value) maps to a voting (non-observer) role that can lead.
func roleFromConfigString(role string) NodeRole {
	switch role {
	case "primary":
		return RolePrimary
	case "replica":
		return RoleReplica
	case "sentinel":
		return RoleSentinel
	case "observer":
		return RoleObserver
	default:
		return RolePrimary // auto: a voter that can lead
	}
}

// configuredRole is this node's role for PeerHello/JoinRequest advertisement.
func (c *ClusterCoordinator) configuredRole() NodeRole {
	return roleFromConfigString(c.cfg.Role)
}

func (c *ClusterCoordinator) localHello() mbp.PeerHello {
	role := uint8(c.configuredRole())
	return mbp.PeerHello{
		NodeID:          c.cfg.NodeID,
		Addr:            c.advertiseAddr,
		Role:            role,
		Epoch:           c.epochStore.Load(),
		SecretHash:      helloHMAC(c.cfg.ClusterSecret, c.cfg.NodeID, c.advertiseAddr, role),
		ProtocolVersion: mbp.CurrentProtocolVersion,
	}
}

func (c *ClusterCoordinator) verifyHello(h mbp.PeerHello) bool {
	if c.cfg.ClusterSecret == "" {
		return true // open mode
	}
	return hmac.Equal(h.SecretHash, helloHMAC(c.cfg.ClusterSecret, h.NodeID, h.Addr, h.Role))
}

func writeHelloFrame(conn net.Conn, h mbp.PeerHello) error {
	payload, err := msgpack.Marshal(h)
	if err != nil {
		return err
	}
	return mbp.WriteFrame(conn, &mbp.Frame{
		Version:       0x01,
		Type:          mbp.TypePeerHello,
		PayloadLength: uint32(len(payload)),
		Payload:       payload,
	})
}

// runPeerDiscovery dials each configured seed that has no live identified conn
// and performs a PeerHello handshake, building the discovery mesh that lets
// non-joining nodes (two primaries, sentinels, lobe↔lobe) reach each other so
// their heartbeats, votes, and claims flow (#522 Step 4). Runs for every role.
func (c *ClusterCoordinator) runPeerDiscovery(ctx context.Context) {
	interval := c.heartbeatInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	backoff := make(map[string]time.Time) // seed addr → next eligible dial time
	seedOwner := make(map[string]string)  // seed addr → real node-id learned for it
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, seed := range c.cfg.Seeds {
			if seed == c.advertiseAddr || c.mgr.HasLivePeerAt(seed) {
				continue
			}
			// Skip a seed already covered by a live conn to its node — this is how
			// a Lobe avoids re-dialing its Cortex when the advertised address (the
			// PeerConn key) differs from the dialed seed string (DNS vs IP).
			if owner, ok := seedOwner[seed]; ok {
				if p, ok2 := c.mgr.GetPeer(owner); ok2 && p.IsConnected() {
					continue
				}
			}
			if t, ok := backoff[seed]; ok && time.Now().Before(t) {
				continue
			}
			nodeID, err := c.helloDial(ctx, seed)
			if err != nil {
				jitter := time.Duration(rand.Int63n(int64(interval) + 1))
				backoff[seed] = time.Now().Add(5*time.Second + jitter)
				continue
			}
			if nodeID != "" {
				seedOwner[seed] = nodeID
			}
			delete(backoff, seed)
		}
	}
}

// helloDial dials a seed and runs the handshake from the initiating side.
// Returns the peer's node-id (even when the conn was not adopted by the
// tie-break — the caller uses it to stop re-dialing an already-covered seed).
func (c *ClusterCoordinator) helloDial(ctx context.Context, seedAddr string) (string, error) {
	var d net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := d.DialContext(dialCtx, "tcp", seedAddr)
	cancel()
	if err != nil {
		return "", err
	}
	if err := writeHelloFrame(conn, c.localHello()); err != nil {
		conn.Close()
		return "", err
	}
	frame, err := mbp.ReadFrame(conn)
	if err != nil || frame.Type != mbp.TypePeerHello {
		conn.Close()
		return "", fmt.Errorf("hello: bad reply from %s", seedAddr)
	}
	var peer mbp.PeerHello
	if err := msgpack.Unmarshal(frame.Payload, &peer); err != nil {
		conn.Close()
		return "", err
	}
	if peer.NodeID == "" || peer.NodeID == c.cfg.NodeID || !c.verifyHello(peer) {
		conn.Close()
		return "", fmt.Errorf("hello: invalid peer from %s", seedAddr)
	}
	// Our OUTBOUND conn is canonical iff we are the lower node-id.
	p, adopted := c.adoptHelloPeer(conn, peer, c.cfg.NodeID < peer.NodeID)
	if adopted {
		// The outbound conn has no other reader — spawn one so the peer's pings,
		// votes, and claims are processed.
		go c.readPeerFrames(ctx, p, peer.NodeID)
	}
	return peer.NodeID, nil
}

// HandleIncomingHello is the accept side: validate, reply, tie-break, adopt.
// Returns (peerNodeID, adopted, err). When adopted is false the caller closes
// conn; when true, the caller's frame-dispatch loop reads subsequent frames.
func (c *ClusterCoordinator) HandleIncomingHello(conn net.Conn, payload []byte) (string, bool, error) {
	var peer mbp.PeerHello
	if err := msgpack.Unmarshal(payload, &peer); err != nil {
		return "", false, err
	}
	if peer.NodeID == "" || peer.NodeID == c.cfg.NodeID {
		return "", false, fmt.Errorf("hello: invalid/self node-id %q", peer.NodeID)
	}
	if !c.verifyHello(peer) {
		return peer.NodeID, false, errors.New("hello: invalid cluster secret")
	}
	// Reply first — the dialer is blocked reading our hello.
	if err := writeHelloFrame(conn, c.localHello()); err != nil {
		return peer.NodeID, false, err
	}
	// The dialer's (their OUTBOUND) conn is canonical iff THEY are the lower id.
	_, adopted := c.adoptHelloPeer(conn, peer, peer.NodeID < c.cfg.NodeID)
	return peer.NodeID, adopted, nil
}

// adoptHelloPeer registers the conn (subject to the tie-break), reconciles the
// peer identity into MSP + voters, and announces our leadership on the new conn
// so a dual-leader can be resolved by the equal-epoch CortexClaim tie-break.
// Returns the adopted PeerConn (nil if not adopted) and whether it was adopted.
func (c *ClusterCoordinator) adoptHelloPeer(conn net.Conn, peer mbp.PeerHello, canonical bool) (*PeerConn, bool) {
	p, adopted := c.mgr.RegisterConnKind(peer.NodeID, peer.Addr, conn, kindHello, canonical)
	if !adopted {
		conn.Close()
		return nil, false
	}
	c.reconcileJoinedPeer(peer.NodeID, peer.Addr, NodeRole(peer.Role))
	if c.IsLeader() {
		c.sendClaim(p)
	}
	return p, true
}

// sendClaim announces our current leadership to a single peer over its conn.
func (c *ClusterCoordinator) sendClaim(p *PeerConn) {
	epoch := c.epochStore.Load()
	payload, err := msgpack.Marshal(mbp.CortexClaim{
		CortexID:     c.cfg.NodeID,
		CortexAddr:   c.advertiseAddr,
		Epoch:        epoch,
		FencingToken: epoch,
	})
	if err != nil {
		return
	}
	_ = p.Send(mbp.TypeCortexClaim, payload)
}

// startElectionWithJitter starts a failover election after a deterministic
// per-node delay (0–1 heartbeat) so concurrent ODOWN-triggered candidates stagger
// instead of splitting the vote, then retries a bounded number of times if it
// stays a stuck candidate — but only while no leader has emerged (#522 Step 4c).
func (c *ClusterCoordinator) startElectionWithJitter() {
	// Single-flight: a concurrent ODOWN event must not spawn a second election
	// driver that could ResetCandidate a candidacy this one is about to win.
	if !c.electing.CompareAndSwap(false, true) {
		return
	}
	defer c.electing.Store(false)

	hb := c.heartbeatInterval()
	h := fnv.New32a()
	_, _ = h.Write([]byte(c.cfg.NodeID))
	time.Sleep(time.Duration(uint64(h.Sum32()) % uint64(hb)))

	for attempt := 0; attempt < 4; attempt++ {
		if c.IsLeader() {
			return
		}
		if ldr := c.election.CurrentLeader(); ldr != "" && ldr != c.cfg.NodeID {
			return // someone else won
		}
		c.election.ResetCandidate() // clear any prior stuck candidacy so we can retry
		if err := c.election.StartElection(context.Background()); err != nil {
			slog.Debug("cluster: failover election start", "err", err)
		}
		time.Sleep(4 * hb) // wait for the outcome (promotion or a new leader's claim)
	}
}

// readPeerFrames reads frames from a hello-dialed (outbound) conn and dispatches
// them, until the conn errors (e.g. it was replaced by the tie-break, or closed).
func (c *ClusterCoordinator) readPeerFrames(ctx context.Context, p *PeerConn, nodeID string) {
	for {
		ft, payload, err := p.Receive()
		if err != nil {
			return
		}
		if err := c.HandleIncomingFrame(nodeID, ft, payload); err != nil {
			slog.Warn("cluster: hello-peer frame error", "node", nodeID, "type", ft, "err", err)
		}
	}
}
