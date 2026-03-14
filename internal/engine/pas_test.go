package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// testPASEnv wires up an Engine with PAS enabled:
//   - TransitionCache wired into the activation engine
//   - TransitionWorker wired into the engine
//   - An authStore with PredictiveActivation enabled for the test vault
func testPASEnv(t *testing.T) (*Engine, func()) {
	t.Helper()
	dir := t.TempDir()

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)

	embedder := &noopEmbedder{}
	ftsA := &ftsAdapter{ftsIdx}
	actEngine := activation.New(store, ftsA, nil, embedder)
	actEngine.SetTransitionStore(store.TransitionCache())

	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)

	authStore := auth.NewStore(db)

	eng := NewEngine(EngineConfig{Store: store, AuthStore: authStore, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})

	tw := cognitive.NewTransitionWorker(context.Background(), store.TransitionCache())
	eng.SetTransitionWorker(tw)

	// Configure the test vault with PAS enabled.
	pasEnabled := true
	maxInj := 10
	err = authStore.SetVaultConfig(auth.VaultConfig{
		Name:   "test",
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			PredictiveActivation: &pasEnabled,
			PASMaxInjections:     &maxInj,
		},
	})
	if err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	return eng, func() {
		tw.Stop()
		eng.Stop()
		store.Close()
	}
}

func TestPAS_TransitionRecording(t *testing.T) {
	eng, cleanup := testPASEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write 4 engrams with distinct content for FTS matching.
	concepts := []struct {
		concept string
		content string
	}{
		{"coffee brewing", "how to brew pour-over coffee step by step"},
		{"coffee beans", "arabica beans from Ethiopia make excellent pour-over"},
		{"tea preparation", "green tea should be steeped at 175 degrees"},
		{"baking bread", "sourdough bread requires a starter culture"},
	}

	for _, c := range concepts {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "test",
			Concept: c.concept,
			Content: c.content,
		})
		if err != nil {
			t.Fatalf("Write(%q): %v", c.concept, err)
		}
	}

	awaitFTS(t, eng) // FTS indexing

	// Activation 1: query about coffee brewing
	resp1, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"coffee brewing pour-over"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate 1: %v", err)
	}
	if len(resp1.Activations) == 0 {
		t.Fatal("Activate 1 returned 0 results")
	}

	// Allow drainLog goroutine to process the first activation's log entry.
	time.Sleep(100 * time.Millisecond)

	// Activation 2: query about coffee beans (sequential after brewing)
	resp2, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"arabica coffee beans Ethiopia"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate 2: %v", err)
	}
	if len(resp2.Activations) == 0 {
		t.Fatal("Activate 2 returned 0 results")
	}

	// Force the transition worker to drain by stopping and restarting.
	// The 30s batch interval means sleep alone won't flush within test timeouts.
	tw := eng.transitionWorker
	tw.Stop()
	// Restart for subsequent operations.
	newTw := cognitive.NewTransitionWorker(context.Background(), eng.store.TransitionCache())
	eng.SetTransitionWorker(newTw)
	defer newTw.Stop()

	// Verify transitions were recorded by checking the TransitionCache directly.
	wsPrefix := eng.store.ResolveVaultPrefix("test")
	for _, prevItem := range resp1.Activations {
		var srcID [16]byte
		id, _ := storage.ParseULID(prevItem.ID)
		copy(srcID[:], id[:])

		targets, err := eng.store.TransitionCache().GetTopTransitions(ctx, wsPrefix, srcID, 10)
		if err != nil {
			t.Fatalf("GetTopTransitions: %v", err)
		}
		if len(targets) > 0 {
			return // at least one transition was recorded
		}
	}
	t.Error("no transitions recorded after sequential activations")
}

func TestPAS_TransitionBoostInScoreComponents(t *testing.T) {
	eng, cleanup := testPASEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write engrams
	items := []struct {
		concept string
		content string
	}{
		{"step one login", "first step is to login to the system with credentials"},
		{"step two dashboard", "after login navigate to the main dashboard"},
		{"step three report", "from dashboard generate the monthly report"},
		{"unrelated topic", "quantum physics explains particle behavior"},
	}

	for _, item := range items {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "test",
			Concept: item.concept,
			Content: item.content,
		})
		if err != nil {
			t.Fatalf("Write(%q): %v", item.concept, err)
		}
	}

	awaitFTS(t, eng)

	// Build a transition pattern: login → dashboard (repeated to strengthen)
	for i := 0; i < 3; i++ {
		_, _ = eng.Activate(ctx, &mbp.ActivateRequest{
			Vault:      "test",
			Context:    []string{"login system credentials"},
			MaxResults: 10,
			Threshold:  0.01,
		})
		time.Sleep(100 * time.Millisecond)

		_, _ = eng.Activate(ctx, &mbp.ActivateRequest{
			Vault:      "test",
			Context:    []string{"navigate dashboard main"},
			MaxResults: 10,
			Threshold:  0.01,
		})
		time.Sleep(100 * time.Millisecond)
	}

	// Force-flush the transition worker to ensure all transitions are processed.
	tw2 := eng.transitionWorker
	tw2.Stop()
	newTw2 := cognitive.NewTransitionWorker(context.Background(), eng.store.TransitionCache())
	eng.SetTransitionWorker(newTw2)
	defer newTw2.Stop()

	// Now activate with "login" context. PAS should predict "dashboard" as a follow-up.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"login system credentials"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Final Activate: %v", err)
	}

	// Check if any result has a non-zero TransitionBoost.
	var hasTransitionBoost bool
	for _, a := range resp.Activations {
		if a.ScoreComponents.TransitionBoost > 0 {
			hasTransitionBoost = true
			t.Logf("PAS boost: %s (%.4f) transition_boost=%.4f",
				a.Concept, a.Score, a.ScoreComponents.TransitionBoost)
		}
	}

	if !hasTransitionBoost {
		t.Error("no TransitionBoost observed in any result — PAS boost integration broken")
	}
}

func TestPAS_DisabledByDefault(t *testing.T) {
	eng, cleanup := testEnv(t) // uses testEnv which has no authStore / PAS config
	defer cleanup()
	ctx := context.Background()

	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "test concept",
		Content: "test content for PAS disabled check",
	})
	if err != nil {
		t.Fatal(err)
	}

	awaitFTS(t, eng)

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"test content"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, a := range resp.Activations {
		if a.ScoreComponents.TransitionBoost != 0 {
			t.Errorf("TransitionBoost should be 0 when PAS disabled, got %f", a.ScoreComponents.TransitionBoost)
		}
	}
}

func TestPAS_VaultIsolation(t *testing.T) {
	eng, cleanup := testPASEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write to vault "test" (PAS enabled)
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "vault test item",
		Content: "item in the test vault for PAS isolation",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Write to vault "other"
	_, err = eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "other",
		Concept: "other vault item",
		Content: "item in a different vault",
	})
	if err != nil {
		t.Fatal(err)
	}

	awaitFTS(t, eng)

	// Activate in "test" vault
	resp1, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"vault test item"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Activate in "other" vault — should not see "test" vault's results
	resp2, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "other",
		Context:    []string{"other vault item"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify no cross-contamination: test vault results should not appear in other vault
	for _, a := range resp2.Activations {
		if a.Concept == "vault test item" {
			t.Error("cross-vault contamination: test vault item appeared in other vault results")
		}
	}
	_ = resp1
}
