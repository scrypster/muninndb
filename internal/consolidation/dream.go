package consolidation

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
)

// DreamOpts configures a dream consolidation pass.
type DreamOpts struct {
	DryRun bool
	Force  bool   // bypass trigger gates
	Scope  string // limit to single vault ("" = all vaults)
}

// DreamReport collects results across all vaults for a single dream run.
type DreamReport struct {
	Reports       []*ConsolidationReport
	Skipped       []string // vault names skipped (legal, no LLM, etc.)
	TotalDuration time.Duration
	JournalEntry  string // formatted journal markdown
}

// defaultDreamPhaseList is the safe set of phases that run when MUNINN_DREAM_PHASES
// is unset. Excluded: 1 (replay, -0.011), 2b (LLM, -0.011), 3 (schema, +0.004
// neutral), 4 (stability, -0.014), 6 (journal, +0.003 neutral). Only phases with
// clearly positive ablation deltas are included.
var defaultDreamPhaseList = []string{"0", "2", "5"}

func newDefaultDreamPhases() map[string]bool {
	m := make(map[string]bool, len(defaultDreamPhaseList))
	for _, p := range defaultDreamPhaseList {
		m[p] = true
	}
	return m
}

// parseDreamPhases parses a comma-separated list of phase identifiers from
// MUNINN_DREAM_PHASES. Returns a fresh copy of the safe defaults {0, 2, 5}
// when raw is empty or on invalid input (with a warning).
func parseDreamPhases(raw string) (phases map[string]bool, isDefault bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return newDefaultDreamPhases(), true
	}
	valid := map[string]bool{
		"0": true, "1": true, "2": true, "2b": true,
		"3": true, "4": true, "5": true, "6": true,
	}
	phases = map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !valid[p] {
			slog.Warn("dream: invalid phase in MUNINN_DREAM_PHASES, falling back to safe defaults (0,2,5)",
				"invalid", p, "raw", raw)
			return newDefaultDreamPhases(), true
		}
		phases[p] = true
	}
	if len(phases) == 0 {
		return newDefaultDreamPhases(), true
	}
	return phases, false
}

func phaseEnabled(phases map[string]bool, phase string) bool {
	return phases[phase]
}

// DreamOnce runs a single dream consolidation pass across vaults.
// In dream mode, the dedup threshold is lowered to 0.85 to surface
// near-duplicate candidates for future LLM review (Phase 2b).
func (w *Worker) DreamOnce(ctx context.Context, opts DreamOpts) (*DreamReport, error) {
	start := time.Now()
	dreport := &DreamReport{}

	phases, usingDefaults := parseDreamPhases(os.Getenv("MUNINN_DREAM_PHASES"))
	if usingDefaults {
		slog.Info("dream: using safe default phases (0,2,5)")
	} else {
		enabled := make([]string, 0, len(phases))
		for p := range phases {
			enabled = append(enabled, p)
		}
		sort.Strings(enabled)
		slog.Info("dream: phases enabled", "phases", enabled)
	}

	// Resolve which vaults to process.
	var vaults []string
	if opts.Scope != "" {
		vaults = []string{opts.Scope}
	} else {
		var err error
		vaults, err = w.Engine.ListVaults(ctx)
		if err != nil {
			return nil, fmt.Errorf("dream: list vaults: %w", err)
		}
	}

	if len(vaults) == 0 {
		slog.Info("dream: no vaults to process")
		dreport.TotalDuration = time.Since(start)
		return dreport, nil
	}

	store := w.Engine.Store()

	// Construct a dream-specific worker to avoid mutating the caller's instance.
	// This prevents data races if DreamOnce is called while the background
	// consolidation scheduler is running on the same Worker.
	dw := &Worker{
		Engine:         w.Engine,
		Schedule:       w.Schedule,
		MaxDedup:       w.MaxDedup,
		MaxTransitive:  w.MaxTransitive,
		DryRun:         opts.DryRun,
		DedupThreshold: 0.85,
		OllamaLLM:     w.OllamaLLM,
		AnthropicLLM:  w.AnthropicLLM,
		OpenAILLM:     w.OpenAILLM,
	}

	for _, vault := range vaults {
		if err := ctx.Err(); err != nil {
			return dreport, fmt.Errorf("dream: context cancelled: %w", err)
		}

		wsPrefix := store.ResolveVaultPrefix(vault)

		report := &ConsolidationReport{
			Vault:     vault,
			StartedAt: time.Now(),
			DryRun:    opts.DryRun,
		}

		// Phase 0: Orient
		var summary *VaultSummary
		if phaseEnabled(phases, "0") {
			var err error
			summary, err = dw.runPhase0Orient(ctx, store, wsPrefix, vault)
			if err != nil {
				slog.Warn("dream: phase 0 (orient) failed", "vault", vault, "error", err)
				report.Errors = append(report.Errors, "phase0_orient: "+err.Error())
			}
			report.Orient = summary
		} else {
			slog.Debug("dream: skipping phase 0 (not in MUNINN_DREAM_PHASES)")
		}

		// Skip legal vaults entirely.
		if summary != nil && summary.IsLegal {
			report.LegalSkipped = summary.EngramCount
			slog.Info("dream: skipping legal vault (protected)",
				"vault", vault, "engrams", summary.EngramCount)
			dreport.Skipped = append(dreport.Skipped, vault)
			report.Duration = time.Since(report.StartedAt)
			dreport.Reports = append(dreport.Reports, report)
			continue
		}

		// Enforce trigger gates unless Force is set.
		if !opts.Force {
			lastDream, engramsAtDream, found, gateErr := store.ReadDreamState(wsPrefix)
			if gateErr != nil {
				// Fail open: proceed on read error.
				slog.Warn("dream: failed to read dream state, proceeding", "vault", vault, "error", gateErr)
			} else if found {
				elapsed := time.Since(lastDream)
				timeGatePassed := elapsed >= 12*time.Hour
				currentCount := int64(0)
				if summary != nil {
					currentCount = int64(summary.EngramCount)
				}
				volumeGatePassed := currentCount-engramsAtDream >= 3
				if !timeGatePassed || !volumeGatePassed {
					slog.Info("dream: skipping vault (gates not met)",
						"vault", vault,
						"time_since_last", elapsed.Round(time.Second),
						"new_engrams", currentCount-engramsAtDream)
					dreport.Skipped = append(dreport.Skipped, vault)
					report.Duration = time.Since(report.StartedAt)
					dreport.Reports = append(dreport.Reports, report)
					continue
				}
			}
			// If not found (first dream), gates pass — proceed.
		}

		// Phase 1: Activation Replay
		if phaseEnabled(phases, "1") {
			if err := dw.runPhase1Replay(ctx, store, wsPrefix, report); err != nil {
				slog.Warn("dream: phase 1 (replay) failed", "vault", vault, "error", err)
				report.Errors = append(report.Errors, "phase1_replay: "+err.Error())
			}
		} else {
			slog.Debug("dream: skipping phase 1 (not in MUNINN_DREAM_PHASES)")
		}

		// Phase 2: Semantic Deduplication (threshold 0.85 in dream mode)
		if phaseEnabled(phases, "2") {
			if err := dw.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
				slog.Warn("dream: phase 2 (dedup) failed", "vault", vault, "error", err)
				report.Errors = append(report.Errors, "phase2_dedup: "+err.Error())
			}
		} else {
			slog.Debug("dream: skipping phase 2 (not in MUNINN_DREAM_PHASES)")
		}

		// Phase 2b: LLM Consolidation
		if phaseEnabled(phases, "2b") {
			if err := dw.runPhase2bLLMConsolidation(ctx, store, wsPrefix, report, vault, report.DedupClustersForLLM); err != nil {
				slog.Warn("dream: phase 2b (LLM consolidation) failed", "vault", vault, "error", err)
				report.Errors = append(report.Errors, "phase2b_llm: "+err.Error())
			}
		} else {
			slog.Debug("dream: skipping phase 2b (not in MUNINN_DREAM_PHASES)")
		}

		// Phase 3: Schema Promotion
		if phaseEnabled(phases, "3") {
			if err := dw.runPhase3SchemaPromotion(ctx, store, wsPrefix, report); err != nil {
				slog.Warn("dream: phase 3 (schema promotion) failed", "vault", vault, "error", err)
				report.Errors = append(report.Errors, "phase3_schema_promotion: "+err.Error())
			}
		} else {
			slog.Debug("dream: skipping phase 3 (not in MUNINN_DREAM_PHASES)")
		}

		// Phase 4: Bidirectional Stability
		if phaseEnabled(phases, "4") {
			if err := dw.runPhase4BidirectionalStability(ctx, store, wsPrefix, report, vault); err != nil {
				slog.Warn("dream: phase 4 (stability) failed", "vault", vault, "error", err)
				report.Errors = append(report.Errors, "phase4_stability: "+err.Error())
			}
		} else {
			slog.Debug("dream: skipping phase 4 (not in MUNINN_DREAM_PHASES)")
		}

		// Phase 5: Transitive Inference
		if phaseEnabled(phases, "5") {
			if err := dw.runPhase5TransitiveInference(ctx, store, wsPrefix, report); err != nil {
				slog.Warn("dream: phase 5 (transitive inference) failed", "vault", vault, "error", err)
				report.Errors = append(report.Errors, "phase5_transitive_inference: "+err.Error())
			}
		} else {
			slog.Debug("dream: skipping phase 5 (not in MUNINN_DREAM_PHASES)")
		}

		report.Duration = time.Since(report.StartedAt)

		if !opts.DryRun {
			currentCount := int64(0)
			if report.Orient != nil {
				currentCount = int64(report.Orient.EngramCount)
			}
			if err := store.WriteDreamState(wsPrefix, time.Now(), currentCount); err != nil {
				slog.Warn("dream: failed to write dream state", "vault", vault, "error", err)
			}
		}

		dreport.Reports = append(dreport.Reports, report)

		slog.Info("dream: vault completed", "vault", vault, "duration", report.Duration,
			"merged", report.MergedEngrams)
	}

	dreport.TotalDuration = time.Since(start)

	// Phase 6: Dream Journal
	if phaseEnabled(phases, "6") {
		journalEntry := formatJournalEntry(dreport, time.Now())
		if !opts.DryRun {
			if path, err := appendJournal(journalEntry); err != nil {
				slog.Warn("dream: failed to write journal", "error", err)
			} else {
				slog.Info("dream: journal appended", "path", path)
			}
		}
		dreport.JournalEntry = journalEntry
	} else {
		slog.Debug("dream: skipping phase 6 (not in MUNINN_DREAM_PHASES)")
	}
	return dreport, nil
}
