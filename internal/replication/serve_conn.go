package replication

import (
	"log"
	"net"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ServeConn reads MBP frames from an incoming cluster TCP connection and
// dispatches them to the coordinator. It returns when the connection is closed.
//
// This is the loop every accepted cluster connection runs in a real server
// (cmd/muninn starts one per Accept). It lives here, not in package main, so a
// test in this package can put real coordinators on real listeners and drive
// the same code a server does.
//
// Protocol: the first frame MUST be TypeJoinRequest or TypePeerHello. On
// success, ownership of conn transfers to the PeerConn inside ConnManager
// (HandleIncomingJoin → RegisterConn, or HandleIncomingHello). After that this
// goroutine no longer closes conn on exit — the coordinator's cleanup owns it.
// If the handshake fails or a protocol violation occurs, this goroutine closes
// conn via the deferred call.
func (c *ClusterCoordinator) ServeConn(conn net.Conn) {
	// connOwned flips to false once a handshake succeeds and the PeerConn takes
	// ownership, preventing a double-close on goroutine exit.
	connOwned := true
	defer func() {
		if connOwned {
			conn.Close()
		}
	}()

	fromNodeID := conn.RemoteAddr().String() // ephemeral until join completes
	joined := false

	for {
		frame, err := mbp.ReadFrame(conn)
		if err != nil {
			// The inbound conn died. If it had been adopted as a peer's registered
			// conn, evict it (unless already replaced) so the peer's restart isn't
			// blocked by a stale live-looking PeerConn (#534).
			if joined {
				c.EvictConn(fromNodeID, conn)
			}
			return // connection closed or error
		}
		if frame.Type == mbp.TypeJoinRequest {
			nodeID, adopted, err := c.HandleIncomingJoin(conn, frame.Payload)
			if err != nil {
				log.Printf("[cluster] join error from %s: %v", fromNodeID, err)
				return
			}
			if !adopted {
				// The join was answered on the raw conn without registering it (a
				// probe, or a non-leader's redirect). Close it here.
				return
			}
			fromNodeID = nodeID
			joined = true
			connOwned = false // PeerConn now owns conn
			continue
		}
		// PeerHello discovery handshake (#522 Step 4) — an alternative first frame
		// for peers that don't join (two primaries, sentinels, lobe↔lobe).
		if frame.Type == mbp.TypePeerHello {
			nodeID, adopted, err := c.HandleIncomingHello(conn, frame.Payload)
			if err != nil {
				log.Printf("[cluster] hello error from %s: %v", fromNodeID, err)
				return
			}
			if !adopted {
				return // tie-break loser: conn deliberately closed in adoptHelloPeer
			}
			fromNodeID = nodeID
			joined = true
			connOwned = false // PeerConn now owns conn
			continue
		}
		// Reject any frame that arrives before a join/hello handshake completes.
		if !joined {
			log.Printf("[cluster] unexpected frame type 0x%02x from %s before join; closing", frame.Type, fromNodeID)
			return
		}
		if err := c.HandleIncomingFrame(fromNodeID, frame.Type, frame.Payload); err != nil {
			log.Printf("[cluster] frame error from %s: %v", fromNodeID, err)
		}
	}
}
