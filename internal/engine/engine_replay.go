package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/plugin/enrich"
	"github.com/scrypster/muninndb/internal/storage"
)

// maxReplayFails is the number of consecutive enrichment failures after which
// an engram is silently skipped by ReplayEnrichment for the remainder of the
// server session. Prevents a broken engram from blocking every replay call.
const maxReplayFails = 3

// ErrEnrichmentConflict is returned when an explicit enrichment apply request
// targets an engram that changed after the caller fetched it.
var ErrEnrichmentConflict = errors.New("enrichment conflict")

// ReplayEnrichmentResult holds the outcome of a replay enrichment run.
type ReplayEnrichmentResult struct {
	Processed int
	Skipped   int
	Failed    int
	Remaining int
	StagesRun []string
	DryRun    bool
}

// EnrichmentCandidate is one active engram that still needs enrichment.
type EnrichmentCandidate struct {
	ID            storage.ULID
	Concept       string
	Content       string
	Summary       string
	MemoryType    string
	TypeLabel     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	MissingStages []string
	DigestFlags   uint8
}

// EnrichmentApplyEntity is one externally generated entity result.
type EnrichmentApplyEntity struct {
	Name       string
	Type       string
	Confidence float32
}

// EnrichmentApplyRelationship is one externally generated relationship result.
type EnrichmentApplyRelationship struct {
	FromEntity string
	ToEntity   string
	RelType    string
	Weight     float32
}

// EnrichmentApplyRequest contains explicit agent-generated enrichment output.
type EnrichmentApplyRequest struct {
	ID                string
	ExpectedUpdatedAt time.Time
	Summary           string
	MemoryType        string
	TypeLabel         string
	Entities          []EnrichmentApplyEntity
	Relationships     []EnrichmentApplyRelationship
	StagesCompleted   []string
	Source            string
}

// EnrichmentApplyResult is returned after explicit enrichment persistence.
type EnrichmentApplyResult struct {
	ID            storage.ULID
	AppliedStages []string
	UpdatedAt     time.Time
	DigestFlags   uint8
}

// stageToFlag maps a stage name to its DigestFlag bit.
var stageToFlag = map[string]uint8{
	"entities":       plugin.DigestEntities,
	"relationships":  plugin.DigestRelationships,
	"classification": plugin.DigestClassified,
	"summary":        plugin.DigestSummarized,
}

// defaultReplayStages are used when no stages param is provided.
var defaultReplayStages = []string{"entities", "relationships", "classification", "summary"}

// ReplayEnrichment re-runs the enrichment pipeline for active engrams in a
// vault that are missing one or more requested digest stage flags.
//
// Parameters:
//   - vault:  vault name
//   - stages: subset of ["entities","relationships","classification","summary"]
//   - limit:  max engrams to process in this call (1-200)
//   - dryRun: if true, scan only — count what would be processed, no writes
//
// The method requires an EnrichPlugin to be registered via SetEnrichPlugin.
// If no plugin is configured and dryRun is false, an error is returned.
func (e *Engine) ReplayEnrichment(ctx context.Context, vault string, stages []string, limit int, dryRun bool) (*ReplayEnrichmentResult, error) {
	stageMask, validStages, seen, err := normalizeEnrichmentStages(stages)
	if err != nil {
		return nil, err
	}

	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	ws := e.store.ResolveVaultPrefix(vault)

	// Collect active engram IDs.
	ids, err := e.store.ListByState(ctx, ws, storage.StateActive, limit)
	if err != nil {
		return nil, fmt.Errorf("replay enrichment: list active engrams: %w", err)
	}

	if len(ids) == 0 {
		return &ReplayEnrichmentResult{
			Processed: 0,
			Skipped:   0,
			Failed:    0,
			Remaining: 0,
			StagesRun: validStages,
			DryRun:    dryRun,
		}, nil
	}

	// Fetch full engram records.
	engrams, err := e.store.GetEngrams(ctx, ws, ids)
	if err != nil {
		return nil, fmt.Errorf("replay enrichment: get engrams: %w", err)
	}

	// On dry run, count how many engrams are missing at least one requested stage.
	if dryRun {
		needed := 0
		skipped := 0
		for _, eng := range engrams {
			if eng == nil {
				skipped++
				continue
			}
			// "not found" means no flags set yet — treat as 0.
			flags, _ := e.store.GetDigestFlags(ctx, plugin.ULID(eng.ID))
			if flags&stageMask != stageMask {
				needed++
			} else {
				skipped++
			}
		}
		return &ReplayEnrichmentResult{
			Processed: needed,
			Skipped:   skipped,
			Failed:    0,
			Remaining: 0,
			StagesRun: validStages,
			DryRun:    true,
		}, nil
	}

	// Real run: require an enrich plugin.
	if e.enrichPlugin == nil {
		return nil, fmt.Errorf("enrichment pipeline not configured: no enrich plugin available")
	}

	processed := 0
	skipped := 0
	failed := 0

	for i, eng := range engrams {
		if eng == nil {
			skipped++
			continue
		}

		// Honour context cancellation (deadline, manual cancel) — report remaining work.
		if ctx.Err() != nil {
			return &ReplayEnrichmentResult{
				Processed: processed,
				Skipped:   skipped,
				Failed:    failed,
				Remaining: countNonNilEngrams(engrams[i:]),
				StagesRun: validStages,
				DryRun:    false,
			}, nil
		}

		// Check which stages are already done.
		// "pebble: not found" means no flags written yet — treat as 0 (all stages needed).
		flags, _ := e.store.GetDigestFlags(ctx, plugin.ULID(eng.ID))

		// If all requested stages are already done, skip this engram.
		if flags&stageMask == stageMask {
			skipped++
			continue
		}

		// Skip engrams that have failed too many times this session.
		e.replayFailMu.Lock()
		failCount := e.replayFailCounts[eng.ID]
		e.replayFailMu.Unlock()
		if failCount >= maxReplayFails {
			slog.Debug("replay enrichment: skipping persistently failing engram",
				"id", eng.ID.String(), "fails", failCount)
			skipped++
			continue
		}

		// Run enrichment for this engram, optionally with a per-engram timeout.
		enrichCtx := ctx
		var enrichCancel context.CancelFunc
		if e.replayEnrichTimeout > 0 {
			enrichCtx, enrichCancel = context.WithTimeout(ctx, e.replayEnrichTimeout)
		}
		result, enrichErr := e.enrichPlugin.Enrich(enrichCtx, eng)
		if enrichCancel != nil {
			enrichCancel()
		}
		if enrichErr != nil {
			if errors.Is(enrichErr, enrich.ErrNothingToEnrich) {
				slog.Debug("replay enrichment: nothing to enrich, skipping", "id", eng.ID.String())
				skipped++
				continue
			}
			// Track consecutive failures; skip if threshold reached next time.
			e.replayFailMu.Lock()
			e.replayFailCounts[eng.ID]++
			newCount := e.replayFailCounts[eng.ID]
			e.replayFailMu.Unlock()
			slog.Warn("replay enrichment: enrich failed, skipping",
				"id", eng.ID.String(), "err", enrichErr, "fail_count", newCount)
			failed++
			continue
		}
		// Success — clear any prior failure count.
		e.replayFailMu.Lock()
		delete(e.replayFailCounts, eng.ID)
		e.replayFailMu.Unlock()

		// Persist enrichment results (summary, key_points, memory_type, type_label).
		if updateErr := e.store.UpdateDigest(ctx, eng.ID, result.Summary, result.KeyPoints, result.MemoryType, result.TypeLabel); updateErr != nil {
			slog.Warn("replay enrichment: UpdateDigest failed",
				"id", eng.ID.String(), "err", updateErr)
			failed++
			continue
		}

		// Upsert entities if entities stage was requested and not already done.
		if flags&plugin.DigestEntities == 0 && seen["entities"] {
			var linkedNames []string
			for _, entity := range result.Entities {
				record := storage.EntityRecord{
					Name:       entity.Name,
					Type:       entity.Type,
					Confidence: entity.Confidence,
				}
				if upsertErr := e.store.UpsertEntityRecord(ctx, record, "replay:enrich"); upsertErr != nil {
					slog.Warn("replay enrichment: UpsertEntityRecord failed",
						"id", eng.ID.String(), "name", entity.Name, "err", upsertErr)
					continue
				}
				if linkErr := e.store.WriteEntityEngramLink(ctx, ws, eng.ID, entity.Name); linkErr != nil {
					slog.Warn("replay enrichment: WriteEntityEngramLink failed",
						"id", eng.ID.String(), "name", entity.Name, "err", linkErr)
					continue
				}
				linkedNames = append(linkedNames, entity.Name)
			}
			for i := 0; i < len(linkedNames); i++ {
				for j := i + 1; j < len(linkedNames); j++ {
					_ = e.store.IncrementEntityCoOccurrence(ctx, ws, linkedNames[i], linkedNames[j])
				}
			}
			if len(result.Entities) > 0 {
				if setErr := e.store.SetDigestFlag(ctx, plugin.ULID(eng.ID), plugin.DigestEntities); setErr != nil {
					slog.Warn("replay enrichment: SetDigestFlag(DigestEntities) failed",
						"id", eng.ID.String(), "err", setErr)
				}
			}
		}

		// Upsert relationships if relationships stage was requested and not already done.
		if flags&plugin.DigestRelationships == 0 && seen["relationships"] {
			for _, rel := range result.Relationships {
				record := storage.RelationshipRecord{
					FromEntity: rel.FromEntity,
					ToEntity:   rel.ToEntity,
					RelType:    rel.RelType,
					Weight:     rel.Weight,
					Source:     "replay:enrich",
				}
				if upsertErr := e.store.UpsertRelationshipRecord(ctx, ws, eng.ID, record); upsertErr != nil {
					slog.Warn("replay enrichment: UpsertRelationshipRecord failed",
						"id", eng.ID.String(), "err", upsertErr)
				}
			}
			if len(result.Relationships) > 0 {
				if setErr := e.store.SetDigestFlag(ctx, plugin.ULID(eng.ID), plugin.DigestRelationships); setErr != nil {
					slog.Warn("replay enrichment: SetDigestFlag(DigestRelationships) failed",
						"id", eng.ID.String(), "err", setErr)
				}
			}
		}

		// Set classification flag if requested and produced output.
		if flags&plugin.DigestClassified == 0 && seen["classification"] && result.Classification != "" {
			if setErr := e.store.SetDigestFlag(ctx, plugin.ULID(eng.ID), plugin.DigestClassified); setErr != nil {
				slog.Warn("replay enrichment: SetDigestFlag(DigestClassified) failed",
					"id", eng.ID.String(), "err", setErr)
			}
		}

		// Set summarized flag if requested and produced output.
		if flags&plugin.DigestSummarized == 0 && seen["summary"] && result.Summary != "" {
			if setErr := e.store.SetDigestFlag(ctx, plugin.ULID(eng.ID), plugin.DigestSummarized); setErr != nil {
				slog.Warn("replay enrichment: SetDigestFlag(DigestSummarized) failed",
					"id", eng.ID.String(), "err", setErr)
			}
		}

		processed++
	}

	return &ReplayEnrichmentResult{
		Processed: processed,
		Skipped:   skipped,
		Failed:    failed,
		StagesRun: validStages,
		DryRun:    false,
	}, nil
}

// GetEnrichmentCandidates returns active engrams missing at least one requested
// stage without invoking any enrichment plugin.
//
// afterID is an exclusive cursor — pass a zero ULID to start from the beginning.
// The returned nextCursor is the last ULID examined; pass it as afterID on the
// next call to continue. A zero nextCursor means all engrams have been scanned.
//
// Adaptive batch sizing: starts at limit*4, doubles (up to 2000) if a batch
// yields 0 candidates — this converges quickly for highly-enriched vaults.
func (e *Engine) GetEnrichmentCandidates(ctx context.Context, vault string, stages []string, afterID storage.ULID, limit int) ([]EnrichmentCandidate, []string, storage.ULID, error) {
	stageMask, validStages, _, err := normalizeEnrichmentStages(stages)
	if err != nil {
		return nil, nil, storage.ULID{}, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	ws := e.store.ResolveVaultPrefix(vault)
	cursor := afterID
	candidates := make([]EnrichmentCandidate, 0, limit)
	batchSize := limit * 4
	if batchSize < 50 {
		batchSize = 50
	}

	for len(candidates) < limit {
		ids, err := e.store.ListByStateFrom(ctx, ws, storage.StateActive, cursor, batchSize)
		if err != nil {
			return nil, nil, storage.ULID{}, fmt.Errorf("get enrichment candidates: list active engrams: %w", err)
		}
		if len(ids) == 0 {
			// Exhausted — signal with zero cursor.
			return candidates, validStages, storage.ULID{}, nil
		}

		engramSlice, err := e.store.GetEngrams(ctx, ws, ids)
		if err != nil {
			return nil, nil, storage.ULID{}, fmt.Errorf("get enrichment candidates: get engrams: %w", err)
		}
		// Build map for O(1) lookup by ID.
		engramMap := make(map[storage.ULID]*storage.Engram, len(engramSlice))
		for _, eng := range engramSlice {
			if eng != nil {
				engramMap[eng.ID] = eng
			}
		}

		lastCheckedID := cursor
		candidatesThisBatch := 0
		hitLimit := false
		for _, id := range ids {
			if len(candidates) >= limit {
				hitLimit = true
				break
			}
			eng := engramMap[id]
			if eng == nil {
				lastCheckedID = id
				continue
			}
			flags, _ := e.store.GetDigestFlags(ctx, plugin.ULID(eng.ID))
			if flags&stageMask == stageMask {
				lastCheckedID = id
				continue
			}
			candidates = append(candidates, EnrichmentCandidate{
				ID:            eng.ID,
				Concept:       eng.Concept,
				Content:       eng.Content,
				Summary:       eng.Summary,
				MemoryType:    eng.MemoryType.String(),
				TypeLabel:     eng.TypeLabel,
				CreatedAt:     eng.CreatedAt,
				UpdatedAt:     eng.UpdatedAt,
				MissingStages: missingStagesForFlags(flags, validStages),
				DigestFlags:   flags,
			})
			candidatesThisBatch++
			lastCheckedID = id
		}
		cursor = lastCheckedID

		// If we hit the limit early, there may be more engrams — return a non-zero cursor.
		if hitLimit {
			break
		}

		// Adaptive sizing: double batch if no candidates found this round.
		if candidatesThisBatch == 0 {
			batchSize *= 2
			if batchSize > 2000 {
				batchSize = 2000
			}
		}
	}

	return candidates, validStages, cursor, nil
}

// ApplyEnrichment persists explicit agent-generated enrichment output.
func (e *Engine) ApplyEnrichment(ctx context.Context, vault string, req *EnrichmentApplyRequest) (*EnrichmentApplyResult, error) {
	if req == nil {
		return nil, fmt.Errorf("apply enrichment: request is required")
	}
	if req.ID == "" {
		return nil, fmt.Errorf("apply enrichment: id is required")
	}
	if req.ExpectedUpdatedAt.IsZero() {
		return nil, fmt.Errorf("apply enrichment: expected_updated_at is required")
	}

	id, err := storage.ParseULID(req.ID)
	if err != nil {
		return nil, fmt.Errorf("apply enrichment: parse id: %w", err)
	}
	eng, err := e.GetEngram(ctx, vault, id)
	if err != nil {
		return nil, fmt.Errorf("apply enrichment: get engram: %w", err)
	}
	if !eng.UpdatedAt.UTC().Equal(req.ExpectedUpdatedAt.UTC()) {
		return nil, fmt.Errorf("%w: engram updated at %s, expected %s",
			ErrEnrichmentConflict,
			eng.UpdatedAt.UTC().Format(time.RFC3339Nano),
			req.ExpectedUpdatedAt.UTC().Format(time.RFC3339Nano))
	}

	completedSet, err := normalizeExplicitEnrichmentStages(req.StagesCompleted)
	if err != nil {
		return nil, fmt.Errorf("apply enrichment: %w", err)
	}
	if req.Summary != "" {
		completedSet["summary"] = true
	}
	if req.MemoryType != "" || req.TypeLabel != "" {
		completedSet["classification"] = true
	}
	if len(req.Entities) > 0 {
		completedSet["entities"] = true
	}
	if len(req.Relationships) > 0 {
		completedSet["relationships"] = true
	}
	appliedStages := materializeStageSet(completedSet)
	if len(appliedStages) == 0 {
		return nil, fmt.Errorf("apply enrichment: at least one enrichment field or completed stage is required")
	}

	if req.Summary != "" || req.MemoryType != "" || req.TypeLabel != "" {
		if err := e.store.UpdateDigest(ctx, id, req.Summary, nil, req.MemoryType, req.TypeLabel); err != nil {
			return nil, fmt.Errorf("apply enrichment: update digest: %w", err)
		}
	}

	ws := e.store.ResolveVaultPrefix(vault)
	source := req.Source
	if source == "" {
		source = "mcp_agent"
	}

	if completedSet["entities"] {
		linkedNames := make([]string, 0, len(req.Entities))
		for _, entity := range req.Entities {
			if entity.Name == "" || entity.Type == "" {
				return nil, fmt.Errorf("apply enrichment: entity name and type are required")
			}
			record := storage.EntityRecord{
				Name:       entity.Name,
				Type:       entity.Type,
				Confidence: entity.Confidence,
			}
			if err := e.store.UpsertEntityRecord(ctx, record, source); err != nil {
				return nil, fmt.Errorf("apply enrichment: upsert entity %q: %w", entity.Name, err)
			}
			if err := e.store.WriteEntityEngramLink(ctx, ws, id, entity.Name); err != nil {
				return nil, fmt.Errorf("apply enrichment: link entity %q: %w", entity.Name, err)
			}
			linkedNames = append(linkedNames, entity.Name)
		}
		for i := 0; i < len(linkedNames); i++ {
			for j := i + 1; j < len(linkedNames); j++ {
				if err := e.store.IncrementEntityCoOccurrence(ctx, ws, linkedNames[i], linkedNames[j]); err != nil {
					return nil, fmt.Errorf("apply enrichment: increment co-occurrence: %w", err)
				}
			}
		}
		if err := e.store.SetDigestFlag(ctx, plugin.ULID(id), plugin.DigestEntities); err != nil {
			return nil, fmt.Errorf("apply enrichment: set entities digest flag: %w", err)
		}
	}

	if completedSet["relationships"] {
		for _, rel := range req.Relationships {
			if rel.FromEntity == "" || rel.ToEntity == "" || rel.RelType == "" {
				return nil, fmt.Errorf("apply enrichment: relationship from_entity, to_entity, and rel_type are required")
			}
			record := storage.RelationshipRecord{
				FromEntity: rel.FromEntity,
				ToEntity:   rel.ToEntity,
				RelType:    rel.RelType,
				Weight:     rel.Weight,
				Source:     source,
			}
			if err := e.store.UpsertRelationshipRecord(ctx, ws, id, record); err != nil {
				return nil, fmt.Errorf("apply enrichment: upsert relationship %q: %w", rel.RelType, err)
			}
		}
		if err := e.store.SetDigestFlag(ctx, plugin.ULID(id), plugin.DigestRelationships); err != nil {
			return nil, fmt.Errorf("apply enrichment: set relationships digest flag: %w", err)
		}
	}

	if completedSet["classification"] {
		if err := e.store.SetDigestFlag(ctx, plugin.ULID(id), plugin.DigestClassified); err != nil {
			return nil, fmt.Errorf("apply enrichment: set classification digest flag: %w", err)
		}
	}
	if completedSet["summary"] {
		if err := e.store.SetDigestFlag(ctx, plugin.ULID(id), plugin.DigestSummarized); err != nil {
			return nil, fmt.Errorf("apply enrichment: set summary digest flag: %w", err)
		}
	}

	updated, err := e.GetEngram(ctx, vault, id)
	if err != nil {
		return nil, fmt.Errorf("apply enrichment: reload engram: %w", err)
	}
	flags, _ := e.store.GetDigestFlags(ctx, plugin.ULID(id))

	return &EnrichmentApplyResult{
		ID:            id,
		AppliedStages: appliedStages,
		UpdatedAt:     updated.UpdatedAt,
		DigestFlags:   flags,
	}, nil
}

// countNonNilEngrams returns the number of non-nil entries in a slice of engram pointers.
func countNonNilEngrams(engrams []*storage.Engram) int {
	n := 0
	for _, eng := range engrams {
		if eng != nil {
			n++
		}
	}
	return n
}

func normalizeEnrichmentStages(stages []string) (uint8, []string, map[string]bool, error) {
	if len(stages) == 0 {
		stages = defaultReplayStages
	}
	stageMask := uint8(0)
	validStages := make([]string, 0, len(stages))
	seen := make(map[string]bool, len(stages))
	for _, s := range stages {
		if _, ok := stageToFlag[s]; !ok {
			return 0, nil, nil, fmt.Errorf("unknown enrichment stage %q: valid stages are entities, relationships, classification, summary", s)
		}
		if !seen[s] {
			stageMask |= stageToFlag[s]
			validStages = append(validStages, s)
			seen[s] = true
		}
	}
	return stageMask, validStages, seen, nil
}

func normalizeExplicitEnrichmentStages(stages []string) (map[string]bool, error) {
	seen := make(map[string]bool, len(stages))
	for _, s := range stages {
		if _, ok := stageToFlag[s]; !ok {
			return nil, fmt.Errorf("unknown enrichment stage %q: valid stages are entities, relationships, classification, summary", s)
		}
		seen[s] = true
	}
	return seen, nil
}

func missingStagesForFlags(flags uint8, stages []string) []string {
	missing := make([]string, 0, len(stages))
	for _, stage := range stages {
		if flags&stageToFlag[stage] == 0 {
			missing = append(missing, stage)
		}
	}
	return missing
}

func materializeStageSet(stageSet map[string]bool) []string {
	if len(stageSet) == 0 {
		return nil
	}
	out := make([]string, 0, len(defaultReplayStages))
	for _, stage := range defaultReplayStages {
		if stageSet[stage] {
			out = append(out, stage)
		}
	}
	return out
}

// SetEnrichPlugin registers an EnrichPlugin for use by ReplayEnrichment.
// Must be called before ReplayEnrichment is used (not concurrency-safe after start).
func (e *Engine) SetEnrichPlugin(p plugin.EnrichPlugin) {
	e.enrichPlugin = p
}

// SetReplayEnrichTimeout sets a per-engram timeout applied to each Enrich() call
// inside ReplayEnrichment. A value of 0 (default) disables the extra timeout and
// lets the MCP request context govern the full run.
// Useful when the LLM backend (e.g. Ollama) can hang on cold-start.
func (e *Engine) SetReplayEnrichTimeout(d time.Duration) {
	e.replayEnrichTimeout = d
}

// ResetReplayFailCount clears the in-session failure counter for the given engram,
// allowing ReplayEnrichment to attempt it again after a manual reset.
func (e *Engine) ResetReplayFailCount(id storage.ULID) {
	e.replayFailMu.Lock()
	delete(e.replayFailCounts, id)
	e.replayFailMu.Unlock()
}
