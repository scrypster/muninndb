package mbp_test

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// benchmarkRoundTrip measures one request and its answer over a real loopback
// TCP connection to a real engine: what a client waits for. It is the context
// for the per-frame session check (BenchmarkAPIKeyStillValid in internal/auth):
// PING is the cheapest frame there is, so it is where that check is the
// largest share; READ is an ordinary request.
func benchmarkRoundTrip(b *testing.B, keyed bool, read bool) {
	env := newMatrixEnv(b)
	const vault = "pilot-station"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: !keyed}); err != nil {
		b.Fatal(err)
	}
	token := ""
	if keyed {
		var err error
		if token, _, err = env.store.GenerateAPIKey(vault, "bench", auth.ModeFull, nil); err != nil {
			b.Fatal(err)
		}
	}
	client := dialMBP(b, env.addr, token, vault)
	_ = client.conn.SetDeadline(time.Now().Add(10 * time.Minute))

	frameType, okType := uint8(mbp.TypePing), uint8(mbp.TypePong)
	var payload []byte
	var err error
	if read {
		resp, werr := env.eng.Write(fullCtx(), &mbp.WriteRequest{Vault: vault, Concept: "Berth 4", Content: "Berth 4 is closed for dredging."})
		if werr != nil {
			b.Fatal(werr)
		}
		frameType, okType = mbp.TypeRead, mbp.TypeReadResp
		payload, err = mbp.EncodeMsgpack(&mbp.ReadRequest{Vault: vault, ID: resp.ID, ReadOnly: true})
	} else {
		payload, err = mbp.EncodeMsgpack(&mbp.PingRequest{Data: "watch"})
	}
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := mbp.WriteFrame(client.conn, &mbp.Frame{Version: 0x01, Type: frameType, CorrelationID: uint64(i + 1), Payload: payload}); err != nil {
			b.Fatal(err)
		}
		f, err := mbp.ReadFrame(client.conn)
		if err != nil || f.Type != okType {
			b.Fatalf("answer: %v frame 0x%02X", err, f.Type)
		}
	}
}

func BenchmarkRoundTrip_Ping_Keyed(b *testing.B)   { benchmarkRoundTrip(b, true, false) }
func BenchmarkRoundTrip_Ping_Keyless(b *testing.B) { benchmarkRoundTrip(b, false, false) }
func BenchmarkRoundTrip_Read_Keyed(b *testing.B)   { benchmarkRoundTrip(b, true, true) }
