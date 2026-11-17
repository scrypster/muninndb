package mbp_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestClientGoneWithRequestsInFlight: a client that disconnects while its
// requests are still being handled does not take the server down with it.
//
// It is done the way anyone could do it: a connection with no credential (a
// public vault needs none) sends WRITE frames and closes the socket without
// waiting for the answers. The handlers finish afterwards, as any handler that
// takes longer than a round trip does.
//
// Before the fix, the connection closed its outgoing channel as soon as the
// reader saw the socket close, without waiting for the handlers it had
// started. The first handler to finish then sent its answer on a closed
// channel, which panics, in a goroutine nothing recovers: the whole server
// process exited. On that code this test does not fail an assertion — it
// kills the test binary with "panic: send on closed channel".
func TestClientGoneWithRequestsInFlight(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}

	// Keep each WRITE's handler running until the test lets it go.
	const inFlight = 3
	env.rec.writeEntered = make(chan struct{}, inFlight)
	env.rec.writeHold = make(chan struct{})

	client := dialMBP(t, env.addr, "", vault)
	for i := 0; i < inFlight; i++ {
		payload, err := mbp.EncodeMsgpack(&mbp.WriteRequest{
			Vault: vault, Concept: "Mooring " + strconv.Itoa(i),
			Content: "Mooring " + strconv.Itoa(i) + " is reserved for the pilot boat.",
		})
		if err != nil {
			t.Fatal(err)
		}
		client.post(mbp.TypeWrite, 0, payload)
	}
	for i := 0; i < inFlight; i++ {
		select {
		case <-env.rec.writeEntered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d WRITEs reached the engine", i, inFlight)
		}
	}

	client.conn.Close() // the client goes away with all three still running

	// Give the server time to notice. Nothing below depends on this for its
	// result; it only makes the old failure happen every time instead of most
	// times.
	time.Sleep(100 * time.Millisecond)

	for i := 0; i < inFlight; i++ {
		env.rec.writeHold <- struct{}{} // each handler finishes and goes to answer
	}

	// The server is still there: a new connection is served, and the writes
	// whose client left were carried out rather than abandoned half way.
	env.rec.writeHold = nil
	again := dialMBP(t, env.addr, "", vault)
	if r := again.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"}); r.frameType != mbp.TypePong {
		t.Fatalf("after the client left, PING on a new connection answered with frame 0x%02X %q", r.frameType, r.message)
	}
	r := again.request(mbp.TypeActivate, &mbp.ActivateRequest{Vault: vault, Context: []string{"pilot boat mooring"}, MaxResults: 10})
	if r.frameType != mbp.TypeActivateResp {
		t.Fatalf("ACTIVATE on a new connection answered with frame 0x%02X %q", r.frameType, r.message)
	}
	writes := 0
	for _, call := range env.rec.take() {
		if call == "Write" {
			writes++
		}
	}
	if writes != inFlight {
		t.Errorf("%d of the %d writes reached the engine", writes, inFlight)
	}
}
