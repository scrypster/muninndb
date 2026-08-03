package storage

import (
	"context"
	"testing"
)

// benchCloneSink defeats escape analysis. Without it the shallow arm's copy is
// elided entirely and reports the same 80 B/op as the bare read, which is how
// the first run of this benchmark nearly argued that copy-on-read was free.
var benchCloneSink *Engram

// BenchmarkGetEngramWarm is the measurement behind STO-20's choice of
// copy-on-MUTATE over copy-on-READ (#858).
//
// The design question was whether GetEngram should simply return a copy, which
// would close the whole class structurally instead of at seven named call
// sites. It is rejected on cost, and this is the cost. All three arms hit the
// L1 cache; the only difference is what happens to the pointer afterwards.
//
// Measured on Apple Silicon, 2M iterations, three runs:
//
//	arm      ns/op      B/op   allocs/op
//	asis     113-132      80       2
//	shallow  185-237     464       3
//	deep     247-273     560       5
//
// A deep copy-on-read is ~2.1x on the hottest read path in the product, and
// recall reads hundreds of engrams per query through GetEngrams. A SHALLOW
// copy-on-read is cheaper but does not actually close the class — it leaves
// Tags, KeyPoints, Associations and Embedding aliased, so element mutation
// still races, and a census would be needed anyway. Copy-on-mutate makes seven
// writers pay the deep-copy cost and leaves every reader at 118 ns.
func BenchmarkGetEngramWarm(b *testing.B) {
	ctx := context.Background()
	store := openTestStore(b)
	ws := store.VaultPrefix("clone-bench")

	eng := &Engram{
		Concept:   "Heron survey",
		Content:   "a moderately long body of text about a field survey",
		Tags:      []string{"survey", "field", "heron"},
		KeyPoints: []string{"one", "two", "three"},
	}
	if _, err := store.WriteEngram(ctx, ws, eng); err != nil {
		b.Fatalf("WriteEngram: %v", err)
	}
	if _, err := store.GetEngram(ctx, ws, eng.ID); err != nil { // warm the cache
		b.Fatalf("GetEngram (warm): %v", err)
	}

	shallow := func(e *Engram) *Engram { c := *e; return &c }

	b.Run("asis", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			g, _ := store.GetEngram(ctx, ws, eng.ID)
			benchCloneSink = g
		}
	})
	b.Run("shallow", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			g, _ := store.GetEngram(ctx, ws, eng.ID)
			benchCloneSink = shallow(g)
		}
	})
	b.Run("deep", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			g, _ := store.GetEngram(ctx, ws, eng.ID)
			benchCloneSink = g.Clone()
		}
	})
}
