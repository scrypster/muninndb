package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// passProbeEmbedder records, from inside the embed call, whether the
// processor was publishing its pass start.
type passProbeEmbedder struct {
	mockPlugin
	rp        *RetroactiveProcessor
	sawActive bool
}

func (p *passProbeEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	_, active := p.rp.PassStartedAt()
	p.sawActive = p.sawActive || active
	out := make([]float32, len(texts)*8)
	for i := range texts {
		out[i*8] = 1
	}
	return out, nil
}
func (p *passProbeEmbedder) Dimension() int    { return 8 }
func (p *passProbeEmbedder) MaxBatchSize() int { return 8 }

// TestRetroactiveProcessor_WriteTwinHookOffIsInert: with no write-twin hook
// wired (MUNINN_WRITE_RELATED=off) the processor publishes no pass start and
// calls nothing; with it wired, it does both — and the hook receives the
// vault prefix and the exact stored vector.
func TestRetroactiveProcessor_WriteTwinHookOffIsInert(t *testing.T) {
	for _, wired := range []bool{false, true} {
		store, registry, _ := openTestStoreWithHNSW(t)
		ctx := context.Background()
		ws := store.VaultPrefix("twin-hook")
		if _, err := store.WriteEngram(ctx, ws, &storage.Engram{Concept: "signal box", Content: "the signal box lever frame has twelve levers", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		p := &passProbeEmbedder{mockPlugin: mockPlugin{name: "probe", tier: TierEmbed}}
		rp := NewRetroactiveProcessor(NewStoreAdapter(store, registry), p, DigestEmbed)
		p.rp = rp
		hooked := 0
		if wired {
			rp.SetOnEmbedded(func(gotWS [8]byte, _ *Engram, vec []float32) {
				if gotWS != ws || len(vec) != 8 || vec[0] != 1 {
					t.Errorf("hook got ws %x vec %v", gotWS, vec)
				}
				hooked++
			})
		}
		if !rp.processBatch(ctx) {
			t.Fatal("pass failed")
		}
		if p.sawActive != wired || (hooked == 1) != wired {
			t.Fatalf("wired=%v: pass start published=%v, hook calls=%d", wired, p.sawActive, hooked)
		}
	}
}
