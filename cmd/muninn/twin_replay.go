package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/auth"
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/writetwin"
)

// twin-replay and twin-audit are the acceptance tooling for the write-time
// twin check (COG-35, design v3 §6). Both are READ-ONLY and run offline
// against a data directory — normally a `muninn backup` copy, since a running
// server holds the Pebble lock. Both print ids and counts only; memory text
// is written solely to the judging file the operator asks for with --out.

// openReadOnlyStore opens dataDir/pebble read-only, refusing a directory a
// live server owns.
func openReadOnlyStore(dataDir string) (*pebble.DB, *storage.PebbleStore, error) {
	if pid, err := readPID(filepath.Join(dataDir, "muninn.pid")); err == nil && isProcessRunning(pid) {
		return nil, nil, fmt.Errorf("muninn is running (pid %d) on %s — take a `muninn backup` and point --data-dir at the backup", pid, dataDir)
	}
	db, err := pebble.Open(filepath.Join(dataDir, "pebble"), &pebble.Options{ReadOnly: true})
	if err != nil {
		return nil, nil, fmt.Errorf("open pebble read-only: %w", err)
	}
	return db, storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000}), nil
}

// resolveReplayThreshold applies the live rule offline: --threshold, else the
// vault's plasticity override, else the hint for --model or the vault's
// recorded embed model. No hint → an error naming the two ways out.
func resolveReplayThreshold(db *pebble.DB, store *storage.PebbleStore, vault string, flagT float64, flagModel string) (writetwin.Threshold, error) {
	if flagT > 0 {
		v := flagT
		return writetwin.ResolveThreshold(&v, "", nil), nil
	}
	if cfg, err := auth.NewStore(db).GetVaultConfig(vault); err == nil {
		if r := auth.ResolvePlasticity(cfg.Plasticity); r.WriteTwinThresholdOverride != nil {
			return writetwin.ResolveThreshold(r.WriteTwinThresholdOverride, "", nil), nil
		}
	}
	model := flagModel
	if model == "" {
		if m, err := store.GetEmbedModel(store.ResolveVaultPrefix(vault)); err == nil {
			model = m
		}
	}
	thr := writetwin.ResolveThreshold(nil, model, embedpkg.WriteTwinHint)
	if !thr.OK {
		return thr, fmt.Errorf("no write-twin hint for embed model %q (no_model_hint): pass --model <registered model> or --threshold <value>", model)
	}
	return thr, nil
}

func runTwinReplay(args []string) {
	fs := flag.NewFlagSet("twin-replay", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "data directory to replay (normally a `muninn backup` copy)")
	vault := fs.String("vault", "", "vault to replay (required)")
	days := fs.Int("days", 60, "replay window in days")
	threshold := fs.Float64("threshold", 0, "override the threshold (clamped to [0.50, 0.999])")
	model := fs.String("model", "", "embed model whose hint to use when the vault records none (e.g. bge-small-en-v1.5)")
	outDir := fs.String("out", "", "write pairs.jsonl (ids), judge.jsonl (FULL TEXT, blind) and key.jsonl here")
	keyPath := fs.String("key", "", "key.jsonl from an earlier --out run (with --labels)")
	labelsPath := fs.String("labels", "", "judged labels, one JSON object per line: {\"item\":N,\"label\":\"duplicate|update|distinct\"}")
	seed := fs.Int64("seed", 1, "judging-sample seed")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		osExit(2)
		return
	}
	if *vault == "" {
		fmt.Fprintln(os.Stderr, "error: --vault is required")
		osExit(2)
		return
	}
	if (*keyPath == "") != (*labelsPath == "") {
		fmt.Fprintln(os.Stderr, "error: --key and --labels go together")
		osExit(2)
		return
	}
	db, store, err := openReadOnlyStore(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		osExit(1)
		return
	}
	defer store.Close()
	if !store.VaultNameExists(*vault) {
		fmt.Fprintf(os.Stderr, "error: vault %q not found in %s\n", *vault, *dataDir)
		osExit(1)
		return
	}
	thr, err := resolveReplayThreshold(db, store, *vault, *threshold, *model)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		osExit(1)
		return
	}
	opt := writetwin.ReplayOptions{Days: *days, Threshold: thr, Seed: *seed}
	rep, pairs, corpus, err := writetwin.Replay(context.Background(), store, store.ResolveVaultPrefix(*vault), opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: replay:", err)
		osExit(1)
		return
	}

	type scored struct {
		FlagsJudged     int     `json:"flags_judged"`
		FlagsPositive   int     `json:"flags_positive"`
		PrecisionLB     float64 `json:"precision_wilson_lower_95"`
		ControlsJudged  int     `json:"controls_judged"`
		ControlPositive int     `json:"controls_positive"`
	}
	var sc *scored
	if *labelsPath != "" {
		keys, labels, err := readKeyAndLabels(*keyPath, *labelsPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			osExit(1)
			return
		}
		fj, fp, cj, cp := writetwin.Score(keys, labels)
		sc = &scored{FlagsJudged: fj, FlagsPositive: fp, PrecisionLB: writetwin.WilsonLower(fp, fj), ControlsJudged: cj, ControlPositive: cp}
		rep.Verdict = writetwin.Decide(rep.FlaggedWrites, rep.Writes, rep.Guard, fj, fp)
	}

	if *outDir != "" {
		if err := writeReplayFiles(*outDir, corpus, pairs, opt); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			osExit(1)
			return
		}
		fmt.Fprintf(os.Stderr, "wrote %s/{pairs,judge,key}.jsonl — judge.jsonl holds full memory text: keep it local\n", *outDir)
	}
	out := map[string]any{"replay": rep}
	if sc != nil {
		out["judging"] = sc
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func writeReplayFiles(dir string, corpus *writetwin.Corpus, pairs []writetwin.ReplayPair, opt writetwin.ReplayOptions) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	judge, keys := writetwin.JudgingSet(corpus, pairs, opt)
	write := func(name string, n int, row func(i int) any) error {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		w := bufio.NewWriter(f)
		enc := json.NewEncoder(w)
		for i := 0; i < n; i++ {
			if err := enc.Encode(row(i)); err != nil {
				f.Close()
				return err
			}
		}
		if err := w.Flush(); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	if err := write("pairs.jsonl", len(pairs), func(i int) any {
		p := pairs[i]
		return map[string]any{"new_id": p.NewID.String(), "head_id": p.HeadID.String(), "similarity": p.Similarity,
			"age_s": int64(p.AgeSeconds), "rule": p.Rule, "new_created": p.NewCreated.UTC().Format(time.RFC3339)}
	}); err != nil {
		return err
	}
	if err := write("judge.jsonl", len(judge), func(i int) any { return judge[i] }); err != nil {
		return err
	}
	return write("key.jsonl", len(keys), func(i int) any { return keys[i] })
}

func readKeyAndLabels(keyPath, labelsPath string) ([]writetwin.KeyItem, map[int]string, error) {
	var keys []writetwin.KeyItem
	if err := readJSONL(keyPath, func(dec *json.Decoder) error {
		var k writetwin.KeyItem
		if err := dec.Decode(&k); err != nil {
			return err
		}
		keys = append(keys, k)
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("read key: %w", err)
	}
	labels := map[int]string{}
	if err := readJSONL(labelsPath, func(dec *json.Decoder) error {
		var l struct {
			Item  int    `json:"item"`
			Label string `json:"label"`
		}
		if err := dec.Decode(&l); err != nil {
			return err
		}
		switch lab := strings.ToLower(strings.TrimSpace(l.Label)); lab {
		case "duplicate", "update", "distinct":
			labels[l.Item] = lab
		default:
			return fmt.Errorf("item %d: label %q is not duplicate|update|distinct", l.Item, l.Label)
		}
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("read labels: %w", err)
	}
	return keys, labels, nil
}

func readJSONL(path string, each func(*json.Decoder) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for dec.More() {
		if err := each(dec); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
	return nil
}

func runTwinAudit(args []string) {
	fs := flag.NewFlagSet("twin-audit", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "data directory (normally a `muninn backup` copy)")
	vault := fs.String("vault", "", "vault to audit (required)")
	logPath := fs.String("log", "", "server log holding the shadow-mode \"write twin flagged\" lines (required)")
	minAge := fs.Duration("min-age", 7*24*time.Hour, "only count pairs flagged at least this long ago")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		osExit(2)
		return
	}
	if *vault == "" || *logPath == "" {
		fmt.Fprintln(os.Stderr, "error: --vault and --log are required")
		osExit(2)
		return
	}
	f, err := os.Open(*logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		osExit(1)
		return
	}
	pairs, span, err := writetwin.ParseFlagLog(f)
	f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: read log:", err)
		osExit(1)
		return
	}
	_, store, err := openReadOnlyStore(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		osExit(1)
		return
	}
	defer store.Close()
	rep, err := writetwin.Audit(context.Background(), store, store.ResolveVaultPrefix(*vault), pairs, span, time.Now(), *minAge)
	if err != nil && !errors.Is(err, writetwin.ErrNoPairs) {
		fmt.Fprintln(os.Stderr, "error: audit:", err)
		osExit(1)
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"audit": rep})
	if errors.Is(err, writetwin.ErrNoPairs) {
		fmt.Fprintln(os.Stderr, "note:", err)
	}
}
