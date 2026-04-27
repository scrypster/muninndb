package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// SupabaseClient stores evaluation results via the Supabase REST API.
type SupabaseClient struct {
	URL string
	Key string
}

// NewSupabaseClient creates a client for the MuninnDB eval project.
func NewSupabaseClient(key string) *SupabaseClient {
	return &SupabaseClient{
		URL: "https://fcleqqzstijiarlqhaoe.supabase.co",
		Key: key,
	}
}

// EvalRun represents a row in eval.runs.
type EvalRun struct {
	GitCommit           string   `json:"git_commit"`
	GitBranch           string   `json:"git_branch"`
	Dataset             string   `json:"dataset"`
	Embedder            string   `json:"embedder"`
	EpisodesEnabled     bool     `json:"episodes_enabled"`
	SimilarityThreshold *float64 `json:"similarity_threshold,omitempty"`
	ReplayEnabled       bool     `json:"replay_enabled"`
	ReplayIntervalHours *float64 `json:"replay_interval_hours,omitempty"`
	SeparationEnabled   bool     `json:"separation_enabled"`
	SeparationAlpha     *float64 `json:"separation_alpha,omitempty"`
	LociEnabled         bool     `json:"loci_enabled"`
	CompletionEnabled   bool     `json:"completion_enabled"`
	CompositeScore      *float64 `json:"composite_score,omitempty"`
	AvgF1               *float64 `json:"avg_f1,omitempty"`
	AvgAccuracy         *float64 `json:"avg_accuracy,omitempty"`
	AvgMRR              *float64 `json:"avg_mrr,omitempty"`
	AvgLatencyMS        *float64 `json:"avg_latency_ms,omitempty"`
	TotalQuestions      int      `json:"total_questions"`
	IngestionTimeMS     *float64 `json:"ingestion_time_ms,omitempty"`
	SearchIteration     *int     `json:"search_iteration,omitempty"`
	SearchSession       *string  `json:"search_session,omitempty"`
}

// EvalResult represents a row in eval.results.
type EvalResult struct {
	RunID        string   `json:"run_id"`
	QuestionID   string   `json:"question_id"`
	Category     string   `json:"category"`
	Score        float64  `json:"score"`
	MRR          *float64 `json:"mrr,omitempty"`
	RecallAtK    *float64 `json:"recall_at_k,omitempty"`
	Question     string   `json:"question,omitempty"`
	Expected     string   `json:"expected_answer,omitempty"`
	ModelAnswer  string   `json:"model_answer,omitempty"`
	JudgeVerdict string   `json:"judge_verdict,omitempty"`
	LatencyMS    *float64 `json:"latency_ms,omitempty"`
	NumResults   *int     `json:"num_results,omitempty"`
}

// CategoryScore represents a row in eval.category_scores.
type CategoryScore struct {
	RunID       string   `json:"run_id"`
	Category    string   `json:"category"`
	AvgScore    float64  `json:"avg_score"`
	MedianScore *float64 `json:"median_score,omitempty"`
	Count       int      `json:"count"`
}

// InsertRun inserts a run and returns the generated UUID.
func (c *SupabaseClient) InsertRun(ctx context.Context, run EvalRun) (string, error) {
	body, err := json.Marshal(run)
	if err != nil {
		return "", fmt.Errorf("marshal run: %w", err)
	}
	respBody, err := c.post(ctx, "/rest/v1/runs", body)
	if err != nil {
		return "", fmt.Errorf("insert run: %w", err)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &rows); err != nil {
		return "", fmt.Errorf("decode run response: %w (body: %s)", err, string(respBody))
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("insert run returned no rows")
	}
	return rows[0].ID, nil
}

// InsertResults inserts evaluation results in batch.
func (c *SupabaseClient) InsertResults(ctx context.Context, results []EvalResult) error {
	body, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("marshal results: %w", err)
	}
	_, err = c.post(ctx, "/rest/v1/results", body)
	return err
}

// InsertCategoryScores inserts aggregated category scores.
func (c *SupabaseClient) InsertCategoryScores(ctx context.Context, scores []CategoryScore) error {
	body, err := json.Marshal(scores)
	if err != nil {
		return fmt.Errorf("marshal scores: %w", err)
	}
	_, err = c.post(ctx, "/rest/v1/category_scores", body)
	return err
}

func (c *SupabaseClient) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", c.Key)
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Profile", "eval")
	req.Header.Set("Content-Profile", "eval")
	req.Header.Set("Prefer", "return=representation")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("supabase %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}
