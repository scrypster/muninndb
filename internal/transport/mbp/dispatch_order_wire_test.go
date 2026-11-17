package mbp_test

import (
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestRefusedFrameIsNotDecompressedOverTheWire is the same pin as
// TestDispatch_ARefusedFrameIsNeverDecompressed, end to end: a real key of a
// restricted mode, a real TCP connection, a real engine. A frame the key may
// not send is answered with the mode refusal whether its payload is garbage
// flagged as compressed or a valid compressed payload, the engine is never
// reached, and the server allocates nothing to decode it.
func TestRefusedFrameIsNotDecompressedOverTheWire(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "pilot-station"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: false}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	const decoded = 8 << 20
	valid := enc.EncodeAll(make([]byte, decoded), nil)
	enc.Close()
	garbage := []byte("flagged as compressed, and nothing of the kind")

	cases := []struct {
		mode    string
		frame   uint8
		wantMsg string
	}{
		{auth.ModeObserve, mbp.TypeWrite, readOnlyMsg},
		{auth.ModeObserve, mbp.TypeForget, readOnlyMsg},
		{auth.ModeWrite, mbp.TypeRead, writeOnlyMsg},
		{auth.ModeWrite, mbp.TypeActivate, writeOnlyMsg},
	}
	clients := map[string]*mbpClient{}
	for _, tc := range cases {
		if clients[tc.mode] == nil {
			token, _, err := env.store.GenerateAPIKey(vault, "order-"+tc.mode, tc.mode, nil)
			if err != nil {
				t.Fatalf("GenerateAPIKey(%s): %v", tc.mode, err)
			}
			clients[tc.mode] = dialMBP(t, env.addr, token, vault)
		}
		client := clients[tc.mode]
		env.rec.take()

		r := client.sendFlags(tc.frame, mbp.FlagCompressed, garbage)
		if r.code != mbp.ErrAuthFailed || r.message != tc.wantMsg {
			t.Errorf("%s key, frame 0x%02X, garbage payload: answered %d %q; want %d %q",
				tc.mode, tc.frame, r.code, r.message, mbp.ErrAuthFailed, tc.wantMsg)
		}

		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		r = client.sendFlags(tc.frame, mbp.FlagCompressed, valid)
		runtime.ReadMemStats(&after)
		if r.code != mbp.ErrAuthFailed || r.message != tc.wantMsg {
			t.Errorf("%s key, frame 0x%02X, valid payload: answered %d %q; want %d %q",
				tc.mode, tc.frame, r.code, r.message, mbp.ErrAuthFailed, tc.wantMsg)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
			t.Errorf("%s key, frame 0x%02X: refusing a frame that decodes to %d MiB allocated %.2f MiB; want nothing decoded",
				tc.mode, tc.frame, decoded>>20, float64(allocated)/(1<<20))
		}

		if reached := env.rec.take(); len(reached) != 0 {
			t.Errorf("%s key, frame 0x%02X: reached the engine: %v", tc.mode, tc.frame, reached)
		}
	}
}
