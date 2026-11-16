package engine

import (
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// The inert-decay WARN dedupe set must not grow with caller-controlled input.
// A request-level weights.actr_decay is attacker-reachable on every recall: if
// the dedupe key carried the value, N distinct values would add N entries (and
// N log lines) — unbounded memory growth and a log flood.
func TestWarnInertACTRDecay_DedupeSetBoundedUnderDistinctWireValues(t *testing.T) {
	e := &Engine{}
	w := &activation.Weights{UseACTR: true}
	resolved := auth.ResolvePlasticity(nil)
	for i := 1; i <= 2000; i++ {
		e.warnInertACTRDecay("orchard", nil, resolved, w, &mbp.Weights{ACTRDecay: float32(i) / 1000})
	}
	n := 0
	e.inertDecayWarned.Range(func(_, _ any) bool { n++; return true })
	if n > 1 {
		t.Fatalf("dedupe set grew to %d entries for 2000 distinct wire actr_decay values; want at most 1", n)
	}
}
