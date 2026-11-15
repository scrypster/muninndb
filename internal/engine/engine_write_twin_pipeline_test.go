package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/writetwin"
)

// batchJitterPlugin is a deterministic embed plugin whose vector for a text
// depends slightly on the text's batch-mates — the bundled int8 model's real
// behaviour — and which records every vector it hands out.
type batchJitterPlugin struct {
	mu       sync.Mutex
	texts    []string
	produced map[string][]float32 // text → the vector this plugin returned for it
}

func (p *batchJitterPlugin) Name() string                                    { return "twin-test-embed" }
func (p *batchJitterPlugin) Tier() plugin.PluginTier                         { return plugin.TierEmbed }
func (p *batchJitterPlugin) Init(context.Context, plugin.PluginConfig) error { return nil }
func (p *batchJitterPlugin) Close() error                                    { return nil }
func (p *batchJitterPlugin) Dimension() int                                  { return twinDim }
func (p *batchJitterPlugin) MaxBatchSize() int                               { return 8 }

func hashVec(s string) []float32 {
	v := make([]float32, twinDim)
	var n float64
	for i := range v {
		h := sha256.Sum256([]byte(s + string(rune('a'+i))))
		x := float64(int32(binary.BigEndian.Uint32(h[:4]))) / math.MaxInt32
		v[i] = float32(x)
		n += x * x
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}

func (p *batchJitterPlugin) Embed(_ context.Context, texts []string) ([]float32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.produced == nil {
		p.produced = map[string][]float32{}
	}
	mates := hashVec(strings.Join(texts, "|"))
	out := make([]float32, 0, len(texts)*twinDim)
	for _, t := range texts {
		v := hashVec(t)
		for i := range v {
			v[i] += 0.01 * mates[i]
		}
		p.texts = append(p.texts, t)
		p.produced[t] = append([]float32(nil), v...)
		out = append(out, v...)
	}
	return out, nil
}

// startTwinPipeline wires a REAL retroactive embed processor to env's engine,
// exactly as cmd/muninn does: OnEmbedded feeds the ring, the processor is the
// feed, and every write notifies it.
func startTwinPipeline(t *testing.T, env *twinTestEnv) *batchJitterPlugin {
	t.Helper()
	p := &batchJitterPlugin{}
	rp := plugin.NewRetroactiveProcessor(plugin.NewStoreAdapter(env.store, env.reg), p, plugin.DigestEmbed)
	rp.SetOnEmbedded(env.eng.WriteTwinOnEmbedded)
	env.eng.SetWriteTwinFeed(rp)
	env.eng.SetRetroactiveProcessors(rp)
	env.eng.SetOnWrite(rp.Notify)
	ctx, cancel := context.WithCancel(context.Background())
	rp.Start(ctx)
	t.Cleanup(func() { cancel(); rp.Stop() })
	return p
}

// awaitRing waits for id's vector to reach the ring. The budget is a failure
// detector for a hang, never a timing assertion.
func (env *twinTestEnv) awaitRing(vault string, id storage.ULID) writetwin.Entry {
	env.t.Helper()
	e, ok := env.eng.twin.awaitVector(context.Background(), env.store.ResolveVaultPrefix(vault), id, 10*time.Second)
	if !ok {
		env.t.Fatalf("the embed processor never delivered %s to the ring", id)
	}
	return e
}

func TestWriteTwin_RingVectorMatchesStoredVector(t *testing.T) {
	env := newTwinEnv(t, writetwin.Shadow, "bge-small-en-v1.5")
	p := startTwinPipeline(t, env)
	ctx := context.Background()
	ws := env.store.ResolveVaultPrefix("layout")
	type w struct{ concept, content string }
	writes := []w{
		{"turntable", "the roundhouse turntable is a 90-foot kit"},
		{"turntable", "roundhouse turntable: 90-foot kit, motorised"},
		{"backdrop", "the backdrop behind the yard is a photo print"},
	}
	for _, x := range writes {
		id := env.write("layout", x.concept, x.content)
		entry := env.awaitRing("layout", id)
		text := x.concept + " " + x.content // the processor's text assembly
		p.mu.Lock()
		want, ok := p.produced[text]
		p.mu.Unlock()
		if !ok {
			t.Fatalf("the plugin never embedded %q — the ring vector came from elsewhere", text)
		}
		for i := range want {
			if entry.Vec[i] != want[i] {
				t.Fatalf("ring vector for %s differs from the vector the processor stored and indexed (component %d: %v vs %v)", id, i, entry.Vec[i], want[i])
			}
		}
		res, err := env.reg.Search(ctx, ws, entry.Vec, 1)
		if err != nil || len(res) != 1 || res[0].ID != [16]byte(id) || res[0].Score < 0.9999 {
			t.Fatalf("the ring vector must be the HNSW vector for %s: %+v err %v", id, res, err)
		}
		stored, err := env.store.GetEmbedding(ctx, ws, id)
		if err != nil || len(stored) != twinDim {
			t.Fatalf("stored 0x18 vector: %v err %v", len(stored), err)
		}
		if c := cosine32(stored, entry.Vec); c < 0.999 {
			t.Fatalf("ring vs stored (int8-dequantized) cosine %.5f < 0.999", c)
		}
	}
}

func TestWriteTwin_ShadowAddsNoEmbedCall(t *testing.T) {
	env := newTwinEnv(t, writetwin.Shadow, "bge-small-en-v1.5")
	p := startTwinPipeline(t, env)
	var ids []storage.ULID
	for _, c := range []string{"the station canopy is laser-cut card", "station canopy: laser-cut card kit", "the goods shed has a working crane"} {
		id := env.write("layout", "station", c)
		env.awaitRing("layout", id)
		ids = append(ids, id)
	}
	env.eng.WaitWriteTimeIdle() // every shadow job has run
	p.mu.Lock()
	n := len(p.texts)
	p.mu.Unlock()
	if n != len(ids) {
		t.Fatalf("shadow mode embedded %d texts for %d writes — it must add no embed call", n, len(ids))
	}
	if c := env.emb.calls.Load(); c != 0 {
		t.Fatalf("shadow mode called the query embedder %d times", c)
	}
}

func cosine32(a, b []float32) float64 {
	var d, na, nb float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return d / math.Sqrt(na*nb)
}
