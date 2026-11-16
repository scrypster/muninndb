package mbp_test

import (
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// post writes one frame and returns without waiting for its answer.
func (c *mbpClient) post(frameType uint8, flags uint16, payload []byte) {
	c.t.Helper()
	c.next++
	if err := mbp.WriteFrame(c.conn, &mbp.Frame{Version: 0x01, Type: frameType, Flags: flags, CorrelationID: c.next, Payload: payload}); err != nil {
		c.t.Fatalf("write frame 0x%02x: %v", frameType, err)
	}
}

// awaitNothingInFlight waits for the server to have no decompressed payload
// charged to any connection. A handler gives its charge back just after it has
// queued its answer, so the client can see the answer a moment before the
// charge is gone.
func awaitNothingInFlight(t *testing.T, env *matrixEnv, when string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for env.srv.DecodedInFlightForTest() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d bytes of decompressed payload are still charged to a connection", when, env.srv.DecodedInFlightForTest())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestConnectionDecodeBudgetOverTheWire shows the per-connection budget on
// real connections:
//
//   - a compressed frame that does not fit in what its connection has left is
//     ANSWERED, at once, with error 4012 — it is not held until there is room;
//   - a second connection is untouched by the first one being at its budget;
//   - and whatever happens to a frame — handled, refused, or its client gone
//     before the answer — nothing stays charged afterwards.
//
// The budget is shrunk for the test so the frames can be a few hundred
// kilobytes; in production it is two frames' worth (32 MiB).
func TestConnectionDecodeBudgetOverTheWire(t *testing.T) {
	const budget = 512 * 1024
	env := newMatrixEnvWith(t, func(s *mbp.Server) { s.SetDecodeBudgetForTest(budget) })
	const vault = "pilot-station"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: false}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}
	token, _, err := env.store.GenerateAPIKey(vault, "budget", auth.ModeFull, nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	// A WRITE whose payload decodes to about 300 KB: more than half the budget.
	raw, err := mbp.EncodeMsgpack(&mbp.WriteRequest{
		Vault: vault, Concept: "Tide table",
		Content: strings.Repeat("High water at the north mole is at 04:10. ", 7200),
	})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	frame := enc.EncodeAll(raw, nil)
	enc.Close()
	if len(raw) <= budget/2 || len(raw) > budget {
		t.Fatalf("fixture: the payload decodes to %d bytes; want between half the budget and the budget (%d)", len(raw), budget)
	}

	// Keep each WRITE's handler running until the test lets it go.
	env.rec.writeEntered = make(chan struct{}, 8)
	env.rec.writeHold = make(chan struct{})
	entered := func(what string) {
		t.Helper()
		select {
		case <-env.rec.writeEntered:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never reached the engine", what)
		}
	}

	a := dialMBP(t, env.addr, token, vault)
	a.post(mbp.TypeWrite, mbp.FlagCompressed, frame) // correlation id 1
	entered("connection A's first WRITE")
	if got := env.srv.DecodedInFlightForTest(); got != int64(len(raw)) {
		t.Errorf("with one frame being handled, %d bytes are charged; want %d, what it decoded to", got, len(raw))
	}

	// A second frame on the same connection does not fit. It is answered while
	// the first is still being handled.
	a.post(mbp.TypeWrite, mbp.FlagCompressed, frame) // correlation id 2
	_ = a.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := mbp.ReadFrame(a.conn)
	if err != nil {
		t.Fatalf("the frame that did not fit was not answered while the first was still being handled: %v", err)
	}
	var ep mbp.ErrorPayload
	if f.Type == mbp.TypeError {
		_ = mbp.DecodeMsgpack(f.Payload, &ep)
	}
	if f.CorrelationID != 2 || f.Type != mbp.TypeError || ep.Code != mbp.ErrRateLimited {
		t.Fatalf("answer: frame 0x%02X for request %d, error %d %q; want error %d for request 2",
			f.Type, f.CorrelationID, ep.Code, ep.Message, mbp.ErrRateLimited)
	}
	if got := env.srv.DecodedInFlightForTest(); got != int64(len(raw)) {
		t.Errorf("after the refusal %d bytes are charged; want only the first frame's %d", got, len(raw))
	}

	// Another connection has its own budget: its frame is handled while
	// connection A is still at its limit.
	b := dialMBP(t, env.addr, token, vault)
	b.post(mbp.TypeWrite, mbp.FlagCompressed, frame)
	entered("connection B's WRITE, sent while connection A was at its budget,")

	// A third connection's client leaves with its frame still being handled.
	c := dialMBP(t, env.addr, token, vault)
	c.post(mbp.TypeWrite, mbp.FlagCompressed, frame)
	entered("connection C's WRITE")
	c.conn.Close()

	for i := 0; i < 3; i++ {
		env.rec.writeHold <- struct{}{} // let A's, B's and C's handlers finish
	}
	for name, client := range map[string]*mbpClient{"A": a, "B": b} {
		_ = client.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := mbp.ReadFrame(client.conn); err != nil {
			t.Errorf("connection %s: its handled WRITE was never answered: %v", name, err)
		}
	}
	awaitNothingInFlight(t, env, "after every handler finished, one of them for a client that had gone")

	// Room again on connection A.
	env.rec.writeHold = nil
	if r := a.sendFlags(mbp.TypeWrite, mbp.FlagCompressed, frame); r.code == mbp.ErrRateLimited {
		t.Errorf("with nothing in flight, connection A's frame was still refused for want of room: %q", r.message)
	}
	awaitNothingInFlight(t, env, "at the end")
}

// TestNothingStaysChargedAfterABurst sends the burst of
// TestEveryFrameOfABurstIsAnswered at the production budget and checks the
// accounting afterwards: every frame was served or refused, and none of them
// left anything charged.
func TestNothingStaysChargedAfterABurst(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}
	client := dialMBP(t, env.addr, "", vault)

	const frames = 64
	frame := streamedZeros(t, 1<<20)
	for i := 0; i < frames; i++ {
		client.post(mbp.TypePing, mbp.FlagCompressed, frame)
	}
	_ = client.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	refused := 0
	for i := 0; i < frames; i++ {
		f, err := mbp.ReadFrame(client.conn)
		if err != nil {
			t.Fatalf("only %d of %d frames were answered: %v", i, frames, err)
		}
		var ep mbp.ErrorPayload
		if f.Type == mbp.TypeError && mbp.DecodeMsgpack(f.Payload, &ep) == nil && ep.Code == mbp.ErrRateLimited {
			refused++
		}
	}
	t.Logf("%d frames: %d refused for want of room, %d decoded", frames, refused, frames-refused)
	awaitNothingInFlight(t, env, "after the burst")
}
