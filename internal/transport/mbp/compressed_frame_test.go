package mbp_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestCompressedBombOverTheWire sends the decompression bomb the way an
// attacker would: one compressed PING, on a connection with no credential at
// all (a public vault needs none), over real TCP. PING is open to every kind
// of session, so nothing about the session can stand in the way of the
// payload being decoded.
//
// The payload is a few kilobytes on the wire and declares more than a frame
// may carry. The server must answer with an error without allocating what the
// payload claims. Before the bound it decoded the whole thing first.
//
// The fixture is small (24 MiB decoded) so the test is safe on unfixed code.
func TestCompressedBombOverTheWire(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}
	client := dialMBP(t, env.addr, "", vault)

	const decoded = mbp.MaxPayloadSize + 8<<20
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	bomb := enc.EncodeAll(make([]byte, decoded), nil)
	enc.Close()

	// Warm the connection up, so the measurement is of the bomb alone.
	if r := client.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"}); r.frameType != mbp.TypePong {
		t.Fatalf("PING answered with frame 0x%02X %q", r.frameType, r.message)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r := client.sendFlags(mbp.TypePing, mbp.FlagCompressed, bomb)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	t.Logf("%d bytes on the wire declaring %d MiB: answered %d %q; the process allocated %.2f MiB",
		len(bomb), decoded>>20, r.code, r.message, float64(allocated)/(1<<20))
	if !r.isError() {
		t.Fatalf("answered with frame 0x%02X; want an error frame", r.frameType)
	}
	if !strings.Contains(r.message, "decompress") {
		t.Errorf("error %q; want it to say the payload could not be decompressed", r.message)
	}
	if allocated > 4<<20 {
		t.Errorf("the server allocated %.2f MiB answering a %d-byte frame; want it bounded by the frame, not by what the payload claims",
			float64(allocated)/(1<<20), len(bomb))
	}

	// The connection is still usable: the refusal is an answer, not a crash.
	if r := client.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"}); r.frameType != mbp.TypePong {
		t.Errorf("after the refusal, PING answered with frame 0x%02X %q", r.frameType, r.message)
	}
}
