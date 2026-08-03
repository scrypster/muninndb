package replication

import (
	"context"
	"log/slog"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/vmihailenco/msgpack/v5"
)

// Streamer streams replication log entries to a replica over a channel.
// Used by the primary to push entries to connected replicas.
type Streamer struct {
	log          *ReplicationLog
	entries      chan ReplicationEntry
	done         chan struct{}
	pollInterval time.Duration
}

// NewStreamer creates a new Streamer for a replication log.
// Default poll interval is 100ms.
func NewStreamer(log *ReplicationLog) *Streamer {
	return &Streamer{
		log:          log,
		entries:      make(chan ReplicationEntry, 1024), // buffered channel
		done:         make(chan struct{}),
		pollInterval: 100 * time.Millisecond,
	}
}

// Stream starts streaming entries from afterSeq to the entries channel.
// Polls the log every pollInterval for new entries.
// Blocks until ctx is cancelled or Stop is called.
func (s *Streamer) Stream(ctx context.Context, afterSeq uint64) error {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	currentSeq := afterSeq

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return nil
		case <-ticker.C:
			// Poll for new entries
			entries, err := s.log.ReadSince(currentSeq, 100)
			if err != nil {
				slog.Error("streamer: ReadSince failed; replication may have gaps",
					"seq", currentSeq, "err", err)
				continue
			}

			for _, entry := range entries {
				select {
				case s.entries <- entry:
					currentSeq = entry.Seq
				case <-ctx.Done():
					return ctx.Err()
				case <-s.done:
					return nil
				}
			}
		}
	}
}

// Entries returns the channel of streamed entries.
func (s *Streamer) Entries() <-chan ReplicationEntry {
	return s.entries
}

// Stop closes the streamer and its channel.
func (s *Streamer) Stop() {
	select {
	case <-s.done:
		// already closed
	default:
		close(s.done)
	}
}

// NetworkStreamer streams replication entries to a single peer connection
// using push-based notification from ReplicationLog.
type NetworkStreamer struct {
	log     *ReplicationLog
	peer    *PeerConn
	lastSeq uint64
}

// NewNetworkStreamer creates a NetworkStreamer that will send entries to peer
// starting from startSeq (exclusive — first entry sent has seq > startSeq).
func NewNetworkStreamer(log *ReplicationLog, peer *PeerConn, startSeq uint64) *NetworkStreamer {
	return &NetworkStreamer{
		log:     log,
		peer:    peer,
		lastSeq: startSeq,
	}
}

// LastSeq returns the highest sequence this streamer has put on the wire. It is
// read by the owning goroutine after Stream returns, so operators can see how
// far a dying streamer got (#627: a streamer that silently died mid-catch-up
// left nothing in the log to distinguish it from a healthy idle one).
func (s *NetworkStreamer) LastSeq() uint64 { return s.lastSeq }

// Stream blocks, waiting for push notifications from the ReplicationLog, then
// reads all available entries and sends them to the peer over MBP.
// Returns when ctx is cancelled or a fatal send/read error occurs.
//
// Protocol version safety: no per-entry version check is performed here because:
//   - The Lobe's protocol version is validated during the join handshake
//     (see JoinHandler.HandleJoinRequest), which rejects incompatible peers.
//   - The connection is long-lived — a Lobe cannot receive entries without
//     first completing the join handshake on this same connection.
//   - ReplEntry uses a fixed msgpack schema; format changes require a
//     protocol version bump, which the join check will catch.
func (s *NetworkStreamer) Stream(ctx context.Context) error {
	notify, unsub := s.log.Subscribe()
	defer unsub()

	drainAvailable := func() error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}

			entries, err := s.log.ReadSince(s.lastSeq, 1000)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				return nil
			}
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return err
				}

				payload, err := msgpack.Marshal(mbp.ReplEntry{
					Seq:         entry.Seq,
					Op:          uint8(entry.Op),
					Key:         entry.Key,
					Value:       entry.Value,
					TimestampNS: entry.TimestampNS,
				})
				if err != nil {
					return err
				}
				if err := sendBulk(ctx, s.peer, mbp.TypeReplEntry, payload, "replication stream"); err != nil {
					return err
				}
				s.lastSeq = entry.Seq
			}
		}
	}

	// Catch up immediately after subscribing so entries already present in the
	// log are streamed even if no new append arrives to trigger a notification.
	if err := drainAvailable(); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notify:
			if err := drainAvailable(); err != nil {
				return err
			}
		}
	}
}
