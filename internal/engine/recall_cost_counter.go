package engine

import (
	"context"
	"sync/atomic"
)

// recallCostCounter counts the work in an activation that scales with its
// candidate list, so a test can pin that explain's diagnostic activation does
// not do it — by counting the work, never by timing it (a timing assertion
// flakes on a loaded machine). Production never attaches one: the cost is one
// context lookup per phase.
type recallCostCounter struct {
	// currencyPairs is the number of row pairs the COG-25 currency pass
	// examines (examined*(examined-1)/2).
	currencyPairs atomic.Int64
	// boostInjectReads is the number of entity-boost injection candidates
	// that cleared the threshold and were read (a point read and a lease read
	// each) — the cost a6e32c9 bounded for explain.
	boostInjectReads atomic.Int64
	// supersessionWindow / contradictionWindow are the rows the declared-
	// relationship phases examine (per-row association reads each).
	supersessionWindow  atomic.Int64
	contradictionWindow atomic.Int64
}

type recallCostKey struct{}

func withRecallCostCounter(ctx context.Context, c *recallCostCounter) context.Context {
	return context.WithValue(ctx, recallCostKey{}, c)
}

func recallCostFrom(ctx context.Context) *recallCostCounter {
	c, _ := ctx.Value(recallCostKey{}).(*recallCostCounter)
	return c
}
