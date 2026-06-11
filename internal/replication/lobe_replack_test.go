package replication

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

// TestStreamFromCortex_SendsReplAck is the #516 part 3 regression: after a Lobe
// applies a streamed ReplEntry it must send a ReplAck back over the same
// connection (which the Cortex's handleClusterConn reads) so the Cortex can
// track per-replica progress / lag. Before this, no ReplAck was ever produced
// and last_seq stayed 0.
func TestStreamFromCortex_SendsReplAck(t *testing.T) {
	coord, _ := newTestCoordinator(t, "replica")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	ackCh := make(chan mbp.ReplAck, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Stream one ReplEntry to the Lobe.
		payload, _ := msgpack.Marshal(mbp.ReplEntry{Seq: 1, Op: uint8(OpSet), Key: []byte("k"), Value: []byte("v")})
		_ = mbp.WriteFrame(conn, &mbp.Frame{
			Version: 0x01, Type: mbp.TypeReplEntry,
			PayloadLength: uint32(len(payload)), Payload: payload,
		})
		// Read the ack the Lobe sends back.
		f, err := mbp.ReadFrame(conn)
		if err != nil || f.Type != mbp.TypeReplAck {
			return
		}
		var ack mbp.ReplAck
		if msgpack.Unmarshal(f.Payload, &ack) == nil {
			ackCh <- ack
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = coord.streamFromCortex(ctx, conn, "cortex-test") }()

	select {
	case ack := <-ackCh:
		require.Equal(t, "node-test", ack.NodeID, "ack should carry the lobe's node id")
		require.Equal(t, uint64(1), ack.LastSeq, "ack should carry the applied seq")
	case <-time.After(3 * time.Second):
		t.Fatal("no ReplAck received from the lobe after it applied the entry")
	}
}
