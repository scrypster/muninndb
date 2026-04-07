package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/scrypster/muninndb/internal/consolidation"
)

// dreamRequest is the request body for POST /api/dream
type dreamRequest struct {
	DryRun bool   `json:"dry_run"`
	Force  bool   `json:"force"`
	Scope  string `json:"scope,omitempty"` // limit to single vault ("" = all)
}

// dreamResponse wraps a DreamReport for JSON serialization.
type dreamResponse struct {
	TotalDuration string                  `json:"total_duration"`
	Skipped       []string                `json:"skipped,omitempty"`
	Reports       []consolidationResponse `json:"reports"`
	JournalEntry  string                  `json:"journal_entry,omitempty"`
}

// handleDream processes POST /api/dream
func (s *Server) handleDream() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req dreamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
			return
		}

		ew, ok := s.engine.(*RESTEngineWrapper)
		if !ok {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, "engine type not supported for dream")
			return
		}
		worker := consolidation.NewWorker(ew.engine)
		worker.OllamaLLM = s.dreamOllama
		worker.AnthropicLLM = s.dreamAnthropic
		worker.OpenAILLM = s.dreamOpenAI

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()

		report, err := worker.DreamOnce(ctx, consolidation.DreamOpts{
			DryRun: req.DryRun,
			Force:  req.Force,
			Scope:  req.Scope,
		})
		if err != nil {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
			return
		}

		resp := dreamResponse{
			TotalDuration: report.TotalDuration.String(),
			Skipped:       report.Skipped,
			JournalEntry:  report.JournalEntry,
		}
		for _, r := range report.Reports {
			resp.Reports = append(resp.Reports, consolidationResponse{
				Vault:                 r.Vault,
				StartedAt:             r.StartedAt,
				Duration:              r.Duration.String(),
				DedupClusters:         r.DedupClusters,
				MergedEngrams:         r.MergedEngrams,
				PromotedNodes:         r.PromotedNodes,
				DecayedEngrams:        r.DecayedEngrams,
				InferredEdges:         r.InferredEdges,
				StabilityStrengthened: r.StabilityStrengthened,
				StabilityWeakened:     r.StabilityWeakened,
				LLMMerged:             r.LLMMerged,
				LLMContradictions:     r.LLMContradictions,
				Journal:               r.Journal,
				DryRun:                r.DryRun,
				Errors:                r.Errors,
			})
		}

		s.sendJSON(w, http.StatusOK, resp)
	}
}

// consolidationRequest is the request body for POST /v1/vaults/{vault}/consolidate
type consolidationRequest struct {
	DryRun bool `json:"dry_run"`
}

// consolidationResponse wraps a ConsolidationReport for JSON serialization
type consolidationResponse struct {
	Vault                 string   `json:"vault"`
	StartedAt             time.Time `json:"started_at"`
	Duration              string   `json:"duration"`
	DedupClusters         int      `json:"dedup_clusters"`
	MergedEngrams         int      `json:"merged_engrams"`
	PromotedNodes         int      `json:"promoted_nodes"`
	DecayedEngrams        int      `json:"decayed_engrams"`
	InferredEdges         int      `json:"inferred_edges"`
	StabilityStrengthened int      `json:"stability_strengthened,omitempty"`
	StabilityWeakened     int      `json:"stability_weakened,omitempty"`
	LLMMerged             int      `json:"llm_merged,omitempty"`
	LLMContradictions     int      `json:"llm_contradictions,omitempty"`
	Journal               string   `json:"journal,omitempty"`
	DryRun                bool     `json:"dry_run"`
	Errors                []string `json:"errors,omitempty"`
}

// handleConsolidate processes POST /v1/vaults/{vault}/consolidate
func (s *Server) handleConsolidate() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vault := r.PathValue("vault")
		if vault == "" {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name is required")
			return
		}

		var req consolidationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
			return
		}

		// Create consolidation worker
		worker := consolidation.NewWorker(s.engine.(consolidation.EngineInterface))
		worker.DryRun = req.DryRun

		// Run consolidation with timeout
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()

		report, err := worker.RunOnce(ctx, vault)
		if err != nil {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
			return
		}

		// Convert report to response
		resp := consolidationResponse{
			Vault:          report.Vault,
			StartedAt:      report.StartedAt,
			Duration:       report.Duration.String(),
			DedupClusters:  report.DedupClusters,
			MergedEngrams:  report.MergedEngrams,
			PromotedNodes:  report.PromotedNodes,
			DecayedEngrams: report.DecayedEngrams,
			InferredEdges:  report.InferredEdges,
			DryRun:         report.DryRun,
			Errors:         report.Errors,
		}

		s.sendJSON(w, http.StatusOK, resp)
	}
}
