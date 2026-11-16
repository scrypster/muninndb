package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// BenchmarkRecall_2000Memories is the stale-fill guard's end-to-end check: a
// recall over a 2,000-memory vault, warm cache. Run with a fixed -benchtime
// (e.g. 300x); the fixture build is outside the timer.
func BenchmarkRecall_2000Memories(b *testing.B) {
	eng, cleanup := testEnvTB(b)
	defer cleanup()
	ctx := context.Background()
	const vault = "bench-railway-2000"
	topics := []string{"signal box", "point rodding", "ballast tamping", "loco boiler", "turntable pit", "goods shed", "water crane", "level crossing"}
	reqs := make([]*mbp.WriteRequest, 0, 50)
	for i := 0; i < 2000; i++ {
		t := topics[i%len(topics)]
		reqs = append(reqs, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("%s work log %d", t, i),
			Content: fmt.Sprintf("volunteer crew logged %s maintenance item %d on the heritage branch line", t, i),
		})
		if len(reqs) == cap(reqs) {
			if _, errs := eng.WriteBatch(ctx, reqs); errs != nil {
				for _, err := range errs {
					if err != nil {
						b.Fatal(err)
					}
				}
			}
			reqs = reqs[:0]
		}
	}
	if eng.ftsWorker != nil {
		if err := eng.ftsWorker.Flush(30 * time.Second); err != nil {
			b.Fatal(err)
		}
	}
	req := &mbp.ActivateRequest{Vault: vault, Context: []string{"signal box maintenance on the branch line"}, MaxResults: 10}
	if _, err := eng.Activate(ctx, req); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := eng.Activate(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}
