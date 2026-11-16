package activation_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/pebble/vfs/errorfs"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

// When the keyword index cannot be READ, recall degrades loudly and gracefully:
// it does not fail, it says so (a WARN naming the vault), and whatever the
// vector channel found is still returned. Same contract as an unreachable embed
// backend (#578), from the other side.
//
// The graceful half was already true. The loud half was not reachable: the
// index turned every read error into "no match" with a nil error, so the WARN
// below never fired and a recall over an unreadable index was indistinguishable
// from a recall that genuinely found nothing.

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func faultyFTS(t *testing.T) (*fts.Index, *pebble.DB, *atomic.Bool) {
	t.Helper()
	armed := new(atomic.Bool)
	inj := errorfs.InjectorFunc(func(op errorfs.Op, path string) error {
		if armed.Load() && op.OpKind() == errorfs.OpKindRead && strings.HasSuffix(path, ".sst") {
			return errorfs.ErrInjected
		}
		return nil
	})
	cache := pebble.NewCache(1)
	db, err := pebble.Open("faulty-fts", &pebble.Options{FS: errorfs.Wrap(vfs.NewMem(), inj), Cache: cache})
	if err != nil {
		cache.Unref()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		armed.Store(false)
		db.Close()
		cache.Unref()
	})
	return fts.New(db), db, armed
}

func TestRecall_KeywordReadErrorDegradesLoudlyAndKeepsVectorResults(t *testing.T) {
	store := newStubStore()
	idx, db, armed := faultyFTS(t)
	ws := [8]byte{0xFA, 0x17} // passed as the request's VaultPrefix; the stub store ignores it

	var ids []storage.ULID
	for _, n := range []struct{ concept, content string }{
		{"Velmora pilot boats", "Port Velmora keeps three pilot boats on the east quay."},
		{"Velmora harbor bell", "The harbor bell of Port Velmora rings twice for an arriving barge."},
		{"Thrennic kiln firing days", "The Thrennic glassworks fires its kiln twice a week."},
	} {
		eng := &storage.Engram{Concept: n.concept, Content: n.content, Confidence: 1.0, Stability: 30.0, CreatedAt: time.Now()}
		store.writeEngram(eng)
		if err := idx.IndexEngram(ws, [16]byte(eng.ID), eng.Concept, "", eng.Content, nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, eng.ID)
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	req := func() *activation.ActivateRequest {
		return &activation.ActivateRequest{VaultID: 4242, VaultPrefix: ws, Context: []string{"Velmora pilot boats"}, Threshold: 0.0, MaxResults: 10}
	}
	const warn = "activation: fts search degraded"

	t.Run("keyword only", func(t *testing.T) {
		eng := activation.New(store, activation.NewFTSAdapter(idx), nil, activation.NewNoopEmbedder())
		defer eng.Close()

		logs := captureLogs(t)
		healthy, err := eng.Run(context.Background(), req())
		if err != nil || len(healthy.Activations) == 0 {
			t.Fatalf("setup: healthy recall = %d results, err %v; want a keyword match", len(healthy.Activations), err)
		}
		if strings.Contains(logs.String(), warn) {
			t.Fatalf("setup: a healthy recall logged %q", warn)
		}

		armed.Store(true)
		res, err := eng.Run(context.Background(), req())
		armed.Store(false)
		if err != nil {
			t.Fatalf("recall with an unreadable keyword index failed outright: %v — it must degrade, not fail", err)
		}
		if !strings.Contains(logs.String(), warn) || !strings.Contains(logs.String(), "vault=4242") {
			t.Errorf("recall over an unreadable keyword index returned %d results and logged no %q for the vault — a silent degradation. Log:\n%s",
				len(res.Activations), warn, logs.String())
		}
	})

	t.Run("with the vector channel", func(t *testing.T) {
		hnsw := &stubHNSW{results: []activation.ScoredID{{ID: ids[0], Score: 0.9}}}
		eng := activation.New(store, activation.NewFTSAdapter(idx), hnsw, &stubEmbedder{})
		defer eng.Close()

		logs := captureLogs(t)
		armed.Store(true)
		res, err := eng.Run(context.Background(), req())
		armed.Store(false)
		if err != nil {
			t.Fatalf("recall with an unreadable keyword index failed outright: %v", err)
		}
		found := false
		for _, a := range res.Activations {
			found = found || a.Engram.ID == ids[0]
		}
		if !found {
			t.Errorf("the vector channel's match was not returned when the keyword index could not be read (%d results)", len(res.Activations))
		}
		if !strings.Contains(logs.String(), warn) {
			t.Errorf("recall kept the vector result but logged no %q — the caller's operator cannot tell the keyword channel was down. Log:\n%s", warn, logs.String())
		}
	})
}
