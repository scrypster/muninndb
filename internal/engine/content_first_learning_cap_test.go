package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/storage"
)

// cfTransitionCounter is a TransitionCacheStore that records every persisted
// (src → dst) pair the PAS transition worker writes.
type cfTransitionCounter struct {
	mu    sync.Mutex
	pairs map[[2][16]byte]uint32
}

func (c *cfTransitionCounter) IncrBy(_ [8]byte, src, dst [16]byte, n uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pairs[[2][16]byte{src, dst}] += n
}

// TestContentFirst_OnlyServedRowsEnterLearningState — under content-first
// order phase 6 hands every gated row to the engine (DeferTruncation), but a
// normal recall must still feed learning ONLY the rows it served: the
// activation log (the Hebbian read side's "recently activated" set and PAS's
// previous-activation set), the PAS transitions persisted from it (previous
// rows x served rows), and the recall event the label pipeline reads. RED
// when Run logs every gated row: ~20 x L transition pairs per recall instead
// of L1 x L2.
func TestContentFirst_OnlyServedRowsEnterLearningState(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-learning-cap"
	cfBuild(c, vault)
	counter := &cfTransitionCounter{pairs: map[[2][16]byte]uint32{}}
	tw := cognitive.NewTransitionWorker(context.Background(), counter)
	c.eng.SetTransitionWorker(tw)
	var events []*storage.RecallEvent
	c.eng.recallEventWriteHook = func(_ context.Context, _ [8]byte, _ storage.ULID, ev *storage.RecallEvent) error {
		events = append(events, ev)
		return nil
	}

	all := c.recall(vault, cfQuery, 100, true)
	gated := 0
	for _, r := range all {
		if r.abs > 0 {
			gated++
		}
	}
	const l1, l2 = 5, 4
	if gated < 3*l1 {
		t.Fatalf("premise: need many more gated rows (%d) than the limit (%d)", gated, l1)
	}

	served := func(rows []cfRow) map[storage.ULID]bool {
		m := map[storage.ULID]bool{}
		for _, r := range rows {
			id, _ := storage.ParseULID(r.id)
			m[id] = true
		}
		return m
	}
	resp1 := c.activate(vault, cfQuery, l1, false, nil) // normal: writes the log
	first := map[storage.ULID]bool{}
	for _, a := range resp1.Activations {
		first[mustULID(t, a.ID)] = true
	}
	associated := map[string]bool{}
	for _, a := range resp1.Associated {
		associated[a.ID] = true
	}
	ws := c.store.ResolveVaultPrefix(vault)
	entries := c.eng.activation.AssocLog().RecentForVault(wsVaultID(ws), 1)
	if len(entries) != 1 {
		t.Fatalf("premise: the normal recall wrote no log entry")
	}
	logged := entries[0].EngramIDs
	if len(logged) != l1 {
		t.Errorf("activation log holds %d rows after a limit=%d recall with %d gated rows; want exactly the %d served", len(logged), l1, gated, l1)
	}
	for _, id := range logged {
		if !first[id] {
			t.Errorf("activation log holds %s, which the recall did not serve", id)
		}
	}
	// The event records exactly what was served: the l1 matched rows, then
	// the associated items (COG-35), flagged and ranked after them.
	if len(events) != 1 || len(events[0].Entries) != l1+len(resp1.Associated) {
		t.Fatalf("recall event must carry the %d served rows plus %d associated items", l1, len(resp1.Associated))
	}
	for i, e := range events[0].Entries {
		id, _ := storage.ParseULID(e.ID)
		isAssoc := e.Source&storage.RecallSourceAssociated != 0
		switch {
		case i < l1 && (!first[id] || isAssoc):
			t.Errorf("recall event entry %d (%s) is not a served matched row", i, e.ID)
		case i >= l1 && (!associated[e.ID] || !isAssoc):
			t.Errorf("recall event entry %d (%s) past the matched rows is not a flagged associated item", i, e.ID)
		}
	}

	second := served(c.recall(vault, "riprap drainage", l2, false)) // reads the log as PAS's previous set
	tw.Stop()                                                       // flush
	srcs := map[storage.ULID]bool{}
	for k := range counter.pairs {
		src, dst := storage.ULID(k[0]), storage.ULID(k[1])
		srcs[src] = true
		if !first[src] || !second[dst] {
			t.Errorf("persisted transition %s → %s is not (served by recall 1) → (served by recall 2)", src, dst)
		}
	}
	if len(srcs) == 0 || len(srcs) > l1 {
		t.Errorf("transition sources: %d distinct, want 1..%d (the rows recall 1 served)", len(srcs), l1)
	}
	if max := l1 * l2; len(counter.pairs) > max {
		t.Errorf("%d transition pairs persisted, at most %d (served x served) allowed", len(counter.pairs), max)
	}
}
