package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// F6 (712-currency fix round): a production engine with no ActivationEngine
// wired silently lost the documented similar_existing feature on every
// write — the nil guard returned without a trace. A misconfigured engine
// silently losing a documented feature is the claim-discipline shape this
// project treats as loud-required (principle #2).
// ---------------------------------------------------------------------------

func TestSimilarExisting_NilActivationEngine_WarnsLoudly(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	defer store.Close()

	// No ActivationEngine wired — exactly the "minimal harness" shape the
	// pre-existing guard's comment names, driven here as if it were a
	// misconfigured production engine.
	eng := NewEngine(EngineConfig{Store: store})
	defer eng.Stop()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	_, err = eng.Write(context.Background(), &mbp.WriteRequest{
		Vault: "nil-activation-probe", Content: "a write with no activation engine wired",
	})
	slog.SetDefault(prev)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !strings.Contains(buf.String(), "similar_existing") {
		t.Fatalf("F6 violated: a nil ActivationEngine silently skipped similar_existing with no WARN logged; got log output: %q", buf.String())
	}
}
