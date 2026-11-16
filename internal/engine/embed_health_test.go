package engine

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// switchEmbedder returns an all-zero vector (the degraded class) while broken
// is set, and a usable unit vector otherwise.
type switchEmbedder struct{ broken atomic.Bool }

func (s *switchEmbedder) Embed(_ context.Context, _ []string) ([]float32, error) {
	v := make([]float32, 8)
	if !s.broken.Load() {
		v[0] = 1
	}
	return v, nil
}
func (s *switchEmbedder) Tokenize(text string) []string { return []string{text} }

// TestEmbedHealth_FollowsRecallOutcomes pins that EmbedHealth reports what the
// most recent embedding-backed recall observed — the status surface's only
// cheap evidence — and never claims "up" before any recall exercised it.
func TestEmbedHealth_FollowsRecallOutcomes(t *testing.T) {
	emb := &switchEmbedder{}
	eng, cleanup := testEnvWithHNSW(t, emb)
	defer cleanup()
	ctx := context.Background()
	const vault = "embed-health-vault"
	if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "the beehive frames were inspected for queen cells"}); err != nil {
		t.Fatal(err)
	}
	recall := func() {
		t.Helper()
		if _, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"beehive frames"}}); err != nil {
			t.Fatal(err)
		}
	}

	if got := eng.EmbedHealth().State; got != "unknown" {
		t.Fatalf("before any recall: state = %q, want unknown", got)
	}
	emb.broken.Store(true)
	recall()
	if got := eng.EmbedHealth().State; got != "degraded_to_keyword" {
		t.Fatalf("after a degraded recall: state = %q, want degraded_to_keyword", got)
	}
	emb.broken.Store(false)
	recall()
	if got := eng.EmbedHealth().State; got != "up" {
		t.Fatalf("after a recovered recall: state = %q, want up", got)
	}
}

func TestEmbedHealth_NoopIsDisabled(t *testing.T) {
	eng, cleanup := testEnvWithHNSW(t, activation.NewNoopEmbedder())
	defer cleanup()
	if got := eng.EmbedHealth().State; got != "disabled" {
		t.Fatalf("noop embedder: state = %q, want disabled", got)
	}
}
