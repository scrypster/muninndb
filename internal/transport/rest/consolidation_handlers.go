package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/scrypster/muninndb/internal/consolidation"
)

// consolidationRequest is the request body for POST /v1/vaults/{vault}/consolidate
type consolidationRequest struct {
	DryRun bool `json:"dry_run"`
}

// consolidationResponse wraps a ConsolidationReport for JSON serialization
type consolidationResponse struct {
	Vault          string    `json:"vault"`
	StartedAt      time.Time `json:"started_at"`
	Duration       string    `json:"duration"`
	DedupClusters  int       `json:"dedup_clusters"`
	MergedEngrams  int       `json:"merged_engrams"`
	PromotedNodes  int       `json:"promoted_nodes"`
	DecayedEngrams int       `json:"decayed_engrams"`
	InferredEdges  int       `json:"inferred_edges"`
	DryRun         bool      `json:"dry_run"`
	Errors         []string  `json:"errors,omitempty"`
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
