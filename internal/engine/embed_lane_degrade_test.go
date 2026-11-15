package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/scrypster/muninndb/internal/embedlane"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// busyEmbedder models an embedder whose lane stays busy past any deadline:
// it never produces a vector, it waits until the caller's context ends and
// returns that error — what the bundled local embedder now does when its
// query lane is occupied. It records whether recall marked the call as a
// query embed.
type busyEmbedder struct{ sawQueryMark atomic.Bool }

func (b *busyEmbedder) Embed(ctx context.Context, _ []string) ([]float32, error) {
	if embedlane.IsQuery(ctx) {
		b.sawQueryMark.Store(true)
	}
	<-ctx.Done()
	return nil, fmt.Errorf("local embed: query lane busy: %w", ctx.Err())
}
func (b *busyEmbedder) Tokenize(text string) []string { return []string{text} }

// TestRecall_DegradesToBM25WhenEmbedTimesOut: when the query embed cannot be
// had within recall's embed budget, recall runs keyword-only and SAYS so —
// semantic_degraded with reason embed_timeout, the timeout counter, and
// muninn_status's embedder state — instead of failing or passing off a
// keyword-only ranking as a full one. Also pins that recall marks its embed as
// a query embed (the explicit lane signal).
func TestRecall_DegradesToBM25WhenEmbedTimesOut(t *testing.T) {
	emb := &busyEmbedder{}
	eng, cleanup := testEnvWithHNSW(t, emb)
	defer cleanup()
	ctx := context.Background()
	const vault = "embed-lane-degrade-vault"
	w, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "hive inspection",
		Content: "the beekeeper inspected the hive frames for capped brood"})
	if err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()

	beforeTimeout := testutil.ToFloat64(metrics.RecallEmbedTimeoutTotal.WithLabelValues(vault))
	beforeFallback := testutil.ToFloat64(metrics.RecallEmbedFallbackTotal.WithLabelValues(vault))

	// A caller deadline: the embed gets half of it (#658), BM25 the rest.
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := eng.Activate(rctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"hive frames brood"}, MaxResults: 5})
	if err != nil {
		t.Fatalf("recall hard-failed when the embed timed out: %v — want a keyword-only degraded result", err)
	}
	if !emb.sawQueryMark.Load() {
		t.Error("recall's query embed was not marked embedlane.WithQuery — the embedder cannot route it to the query lane")
	}
	if !resp.SemanticDegraded {
		t.Fatal("semantic_degraded=false on a recall whose query embed timed out")
	}
	if resp.SemanticDegradedReason != activation.DegradedEmbedTimeout {
		t.Fatalf("semantic_degraded_reason = %q, want %q", resp.SemanticDegradedReason, activation.DegradedEmbedTimeout)
	}
	found := false
	for _, a := range resp.Activations {
		found = found || a.ID == w.ID
	}
	if !found {
		t.Fatalf("keyword fallback did not return the matching memory; got %d activations", len(resp.Activations))
	}
	if got := testutil.ToFloat64(metrics.RecallEmbedTimeoutTotal.WithLabelValues(vault)) - beforeTimeout; got != 1 {
		t.Errorf("muninndb_recall_embed_timeout_total moved by %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.RecallEmbedFallbackTotal.WithLabelValues(vault)) - beforeFallback; got != 1 {
		t.Errorf("muninndb_recall_embed_fallback_total moved by %v, want 1", got)
	}
	h := eng.EmbedHealth()
	if h.State != "degraded_to_keyword" || h.LastDegradedReason != activation.DegradedEmbedTimeout {
		t.Fatalf("EmbedHealth = %+v, want degraded_to_keyword with reason %q", h, activation.DegradedEmbedTimeout)
	}
}

// TestRecall_DegradedReasonDistinguishesFailureClasses: an embed that fails
// outright and one that returns a zero vector are reported as their own
// reasons, never as a timeout — an operator acts differently on each.
func TestRecall_DegradedReasonDistinguishesFailureClasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		emb  activation.Embedder
		want string
	}{
		{"zero vector", zeroVecEmbedder{}, activation.DegradedEmbedZeroVector},
		{"backend error", failingEmbedder{}, activation.DegradedEmbedUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, cleanup := testEnvWithHNSW(t, tc.emb)
			defer cleanup()
			ctx := context.Background()
			if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "reason-vault", Content: "the smoker was relit before opening the second hive"}); err != nil {
				t.Fatal(err)
			}
			resp, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: "reason-vault", Context: []string{"smoker hive"}})
			if err != nil {
				t.Fatal(err)
			}
			if !resp.SemanticDegraded || resp.SemanticDegradedReason != tc.want {
				t.Fatalf("degraded=%v reason=%q, want true/%q", resp.SemanticDegraded, resp.SemanticDegradedReason, tc.want)
			}
			if got := eng.EmbedHealth().LastDegradedReason; got != tc.want {
				t.Fatalf("EmbedHealth reason = %q, want %q", got, tc.want)
			}
		})
	}
}

type failingEmbedder struct{}

func (failingEmbedder) Embed(context.Context, []string) ([]float32, error) {
	return nil, fmt.Errorf("dial tcp 127.0.0.1:1: connection refused")
}
func (failingEmbedder) Tokenize(text string) []string { return []string{text} }
