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

// TestJoinClient_Join_PreservesConnForStreaming is the regression test for #448
// Bug 2. Join previously did `defer conn.Close()`, so the connection died the
// moment Join returned — but the Cortex registers that same inbound conn as the
// Lobe's peer and streams ReplEntry frames over it. First write hit a closed
// socket ("broken pipe") and replication never started.
//
// This test uses REAL TCP and the full Join() (not joinConn, which the existing
// net.Pipe tests call directly, bypassing the defer-close lifecycle). It asserts
// Join hands back the live conn and that a frame the Cortex streams *after* Join
// returns is still readable from it.
func TestJoinClient_Join_PreservesConnForStreaming(t *testing.T) {
	es := newTestEpochStore(t)
	mgr := NewConnManager("lobe-tcp")
	client := NewJoinClient("lobe-tcp", "127.0.0.1:9999", "", es, nil, mgr)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read the JoinRequest, send an accepting JoinResponse (no snapshot).
		readJoinRequest(t, conn)
		sendJoinResponse(t, conn, mbp.JoinResponse{
			Accepted: true, CortexID: "cortex-tcp", CortexAddr: ln.Addr().String(), Epoch: 1,
		})
		// Stream a ReplEntry over the SAME conn, exactly as the real streamer does.
		payload, _ := msgpack.Marshal(mbp.ReplEntry{Seq: 1, Op: 1, Key: []byte("k"), Value: []byte("v")})
		_ = mbp.WriteFrame(conn, &mbp.Frame{
			Version: 0x01, Type: mbp.TypeReplEntry,
			PayloadLength: uint32(len(payload)), Payload: payload,
		})
		<-done // keep the conn open until the test has read the frame
	}()

	result, err := client.Join(context.Background(), ln.Addr().String())
	require.NoError(t, err)
	require.NotNil(t, result.Conn, "Join must return the live conn so the Lobe can read the stream (#448 Bug 2)")

	// The frame the Cortex streamed after Join returned must be readable — i.e.
	// Join did NOT close the conn.
	_ = result.Conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := mbp.ReadFrame(result.Conn)
	require.NoError(t, err, "streamed frame must be readable from the preserved conn")
	require.Equal(t, mbp.TypeReplEntry, frame.Type)

	close(done)
	_ = result.Conn.Close()
}
