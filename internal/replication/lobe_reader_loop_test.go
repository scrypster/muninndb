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

// TestStreamFromCortex_AppliesStreamedEntry verifies the second half of the
// #448 Bug 2 fix: once the Lobe keeps the join connection open, its reader loop
// (streamFromCortex) reads replication frames the Cortex streams and applies
// them. Uses real TCP, like the production path.
func TestStreamFromCortex_AppliesStreamedEntry(t *testing.T) {
	coord, db := newTestCoordinator(t, "replica")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	hold := make(chan struct{})
	defer close(hold)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		payload, _ := msgpack.Marshal(mbp.ReplEntry{Seq: 1, Op: uint8(OpSet), Key: []byte("repl-key"), Value: []byte("repl-val")})
		_ = mbp.WriteFrame(conn, &mbp.Frame{
			Version: 0x01, Type: mbp.TypeReplEntry,
			PayloadLength: uint32(len(payload)), Payload: payload,
		})
		<-hold // keep the conn open until the test finishes
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = coord.streamFromCortex(ctx, conn, "cortex-test") }()

	// The reader loop must apply the streamed ReplEntry to the local store.
	require.Eventually(t, func() bool {
		v, closer, err := db.Get([]byte("repl-key"))
		if err != nil {
			return false
		}
		ok := string(v) == "repl-val"
		closer.Close()
		return ok
	}, 3*time.Second, 20*time.Millisecond, "streamed ReplEntry must be applied by the reader loop")
}
