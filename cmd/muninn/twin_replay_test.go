package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestTwinReplayCLI_ReadOnlyEndToEnd drives `muninn twin-replay` against a
// synthetic data directory: the report is ids and counts only, the judging
// file carries the text, and the store is opened read-only (it is unchanged).
// Invented fixture domain: beekeeping.
func TestTwinReplayCLI_ReadOnlyEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	db, err := storage.OpenPebble(filepath.Join(dataDir, "pebble"), storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 64})
	ws := store.ResolveVaultPrefix("apiary")
	if err := store.WriteVaultName(ws, "apiary"); err != nil {
		t.Fatal(err)
	}
	base := make([]float32, 8)
	base[0] = 1
	near := make([]float32, 8)
	near[0], near[1] = float32(math.Cos(0.2)), float32(math.Sin(0.2)) // cos ≈ 0.980
	at := time.Now().Add(-48 * time.Hour)
	for i, c := range []struct {
		content string
		vec     []float32
		off     time.Duration
	}{
		{"hive 9 queen is marked green", base, 0},
		{"the queen in hive 9 carries a green mark", near, 15 * time.Minute},
	} {
		if _, err := store.WriteEngram(context.Background(), ws, &storage.Engram{Concept: "queen", Content: c.content,
			CreatedAt: at.Add(c.off), UpdatedAt: at, LastAccess: at, Confidence: 1, Stability: 30, State: storage.StateActive, Embedding: c.vec}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	store.Close()

	out := filepath.Join(t.TempDir(), "judge")
	exit := 0
	origExit := osExit
	osExit = func(code int) { exit = code }
	defer func() { osExit = origExit }()
	stdout := captureStdout(func() {
		runTwinReplay([]string{"--data-dir", dataDir, "--vault", "apiary", "--model", "bge-small-en-v1.5", "--out", out})
	})
	if exit != 0 {
		t.Fatalf("exit %d, stdout %s", exit, stdout)
	}
	var rep struct {
		Replay struct {
			FlaggedWrites int                       `json:"flagged_writes"`
			Writes        int                       `json:"writes"`
			Threshold     float64                   `json:"threshold"`
			Verdict       struct{ Decision string } `json:"verdict"`
		} `json:"replay"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, stdout)
	}
	if rep.Replay.FlaggedWrites != 1 || rep.Replay.Writes != 2 || rep.Replay.Threshold != 0.93 {
		t.Fatalf("report: %+v", rep.Replay)
	}
	if strings.Contains(stdout, "green") || strings.Contains(stdout, "hive 9") {
		t.Fatalf("the printed report must carry ids and counts only, never content:\n%s", stdout)
	}
	judge, err := os.ReadFile(filepath.Join(out, "judge.jsonl"))
	if err != nil || !strings.Contains(string(judge), "green mark") {
		t.Fatalf("the judging file must carry full text: %v %s", err, judge)
	}
	for _, f := range []string{"pairs.jsonl", "key.jsonl"} {
		b, err := os.ReadFile(filepath.Join(out, f))
		if err != nil || strings.Contains(string(b), "green") {
			t.Fatalf("%s must be ids only: %v %s", f, err, b)
		}
	}
	// No --model and no recorded model: refused with the way out named.
	exit = 0
	errOut := captureStderr(func() { runTwinReplay([]string{"--data-dir", dataDir, "--vault", "apiary"}) })
	if exit == 0 || !strings.Contains(errOut, "no_model_hint") {
		t.Fatalf("an unknown model must be refused (exit %d): %s", exit, errOut)
	}
}
