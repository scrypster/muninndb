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

// TestConnectionDecodeBudgetOverTheWire shows the per-connection budget doing
// its job on a real connection: two compressed frames that do not fit in the
// budget together are not both decoded at once. The second is decoded only
// when the first one's handler has finished.
//
// The budget is shrunk for the test so the frames can be a couple of hundred
// kilobytes; in production it is two frames' worth (32 MiB) and the frames
// that would fill it decode to megabytes each.
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
	client := dialMBP(t, env.addr, token, vault)

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
	env.rec.writeEntered = make(chan struct{}, 4)
	env.rec.writeHold = make(chan struct{})

	client.post(mbp.TypeWrite, mbp.FlagCompressed, frame)
	client.post(mbp.TypeWrite, mbp.FlagCompressed, frame)

	select {
	case <-env.rec.writeEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first WRITE never reached the engine")
	}
	select {
	case <-env.rec.writeEntered:
		t.Fatalf("both frames (%d bytes decoded each) were decoded and handled at once; the connection's budget is %d", len(raw), budget)
	case <-time.After(200 * time.Millisecond):
		// the second is waiting for the first to finish
	}

	env.rec.writeHold <- struct{}{} // let the first handler finish
	select {
	case <-env.rec.writeEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the second WRITE was never decoded after the first finished")
	}
	env.rec.writeHold <- struct{}{}

	// Both are answered. The content is far over an engram's limit, so the
	// engine refuses it; what matters here is that each frame got an answer.
	for i := 0; i < 2; i++ {
		f, err := mbp.ReadFrame(client.conn)
		if err != nil {
			t.Fatalf("read answer %d: %v", i+1, err)
		}
		if f.Type != mbp.TypeError && f.Type != mbp.TypeWriteOK {
			t.Errorf("answer %d is frame 0x%02X", i+1, f.Type)
		}
	}
}
