package mbp_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// streamedZeros compresses n zero bytes as a streamed zstd frame: one whose
// header does not declare its decoded size.
func streamedZeros(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64*1024)
	for written := 0; written < n; written += len(chunk) {
		if _, err := enc.Write(chunk[:min(len(chunk), n-written)]); err != nil {
			t.Fatal(err)
		}
		if written == 0 {
			if err := enc.Flush(); err != nil { // commit to a header before the size is known
				t.Fatal(err)
			}
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	var hdr zstd.Header
	if err := hdr.Decode(buf.Bytes()); err != nil || hdr.HasFCS {
		t.Fatalf("fixture: want a frame with no declared size (err %v, declared %v)", err, hdr.HasFCS)
	}
	return buf.Bytes()
}

// TestEveryFrameOfABurstIsAnswered: a connection that sends as many compressed
// frames as it may have in flight at once — every one a streamed frame that
// does not fit the first scratch buffer, so every one needs the larger buffer
// — gets an answer to every frame, promptly. None is left waiting.
//
// It is sent the way an attacker would send it: PING frames, on a connection
// with no credential (a public vault needs none).
//
// With a budget that a frame could hold part of while waiting for the rest,
// this burst deadlocks the connection: every frame takes its small first
// charge, the budget has no room left for any of them to take the large one,
// and none gives anything back. No frame is ever answered, and the connection
// and its goroutines are never released, even after the client has gone.
func TestEveryFrameOfABurstIsAnswered(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}
	client := dialMBP(t, env.addr, "", vault)

	const frames = 64 // what one connection may have in flight
	frame := streamedZeros(t, 1<<20)
	for i := 0; i < frames; i++ {
		client.post(mbp.TypePing, mbp.FlagCompressed, frame)
	}

	const bound = 15 * time.Second
	_ = client.conn.SetReadDeadline(time.Now().Add(bound))
	answered := map[uint64]bool{}
	kinds := map[string]int{}
	for len(answered) < frames {
		f, err := mbp.ReadFrame(client.conn)
		if err != nil {
			break
		}
		answered[f.CorrelationID] = true
		if f.Type == mbp.TypeError {
			var ep mbp.ErrorPayload
			_ = mbp.DecodeMsgpack(f.Payload, &ep)
			kinds[ep.Message]++
		} else {
			kinds["(not an error)"]++
		}
	}
	t.Logf("%d of %d frames (%d bytes each on the wire) answered: %v", len(answered), frames, len(frame), kinds)
	if len(answered) != frames {
		t.Fatalf("%d of %d frames were answered within %s; the rest are still waiting", len(answered), frames, bound)
	}

	// The connection is not wedged: an ordinary frame is still served.
	_ = client.conn.SetReadDeadline(time.Now().Add(bound))
	if r := client.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"}); r.frameType != mbp.TypePong {
		t.Errorf("after the burst, PING answered with frame 0x%02X %q", r.frameType, r.message)
	}
}
