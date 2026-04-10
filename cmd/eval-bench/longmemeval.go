package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// LongMemEvalItem represents one question from the LongMemEval dataset.
type LongMemEvalItem struct {
	QuestionID         string          `json:"question_id"`
	QuestionType       string          `json:"question_type"`
	Question           string          `json:"question"`
	RawAnswer          json.RawMessage `json:"answer"`
	QuestionDate       string          `json:"question_date"`
	HaystackSessionIDs []string        `json:"haystack_session_ids"`
	HaystackDates      []string        `json:"haystack_dates"`
	HaystackSessions   [][]HSTurn      `json:"haystack_sessions"`
	AnswerSessionIDs   []string        `json:"answer_session_ids"`
}

func (item *LongMemEvalItem) Answer() string {
	var s string
	if err := json.Unmarshal(item.RawAnswer, &s); err == nil {
		return s
	}
	return strings.Trim(string(item.RawAnswer), "\"")
}

// HSTurn is a single turn within a haystack session.
type HSTurn struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	HasAnswer bool   `json:"has_answer"`
}

type LongMemEvalResult struct {
	QuestionID   string
	QuestionType string
	Ability      string
	Question     string
	Answer       string
	Retrieved    string
	JudgeVerdict string
	Accuracy     float64
	LatencyMS    float64
}

type LongMemEvalSummary struct {
	TotalQuestions  int
	OverallAccuracy float64
	ByAbility       map[string]float64
	AvgLatencyMS    float64
}

// MapAbility maps a LongMemEval question_type to its ability category.
func MapAbility(questionType string) string {
	if strings.HasSuffix(questionType, "_abs") {
		return "Abstention"
	}
	switch questionType {
	case "single-session-user", "single-session-assistant", "single-session-preference":
		return "Information Extraction"
	case "multi-session":
		return "Multi-Session Reasoning"
	case "temporal-reasoning":
		return "Temporal Reasoning"
	case "knowledge-update":
		return "Knowledge Updates"
	default:
		return "Unknown"
	}
}

// RunLongMemEval loads and evaluates the full LongMemEval dataset.
func RunLongMemEval(ctx context.Context, path string, factory EngineFactory, judge Judge, verbose bool) ([]LongMemEvalResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read longmemeval: %w", err)
	}
	var items []LongMemEvalItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parse longmemeval: %w", err)
	}

	var results []LongMemEvalResult
	for i, item := range items {
		if verbose {
			fmt.Fprintf(os.Stderr, "[%d/%d] %s (%s)...\n", i+1, len(items), item.QuestionID, item.QuestionType)
		}

		// Each question gets its own engine (unique haystack).
		tmpDir, err := os.MkdirTemp("", "lme-eval-*")
		if err != nil {
			return results, err
		}

		eng, err := factory(tmpDir)
		if err != nil {
			os.RemoveAll(tmpDir)
			return results, fmt.Errorf("create engine for %s: %w", item.QuestionID, err)
		}

		// Ingest haystack.
		if err := ingestLongMemHaystack(ctx, eng, &item); err != nil {
			eng.Close()
			return results, fmt.Errorf("ingest %s: %w", item.QuestionID, err)
		}

		// Activate and judge.
		result, err := evalLongMemQuestion(ctx, eng, &item, judge)
		eng.Close()
		if err != nil {
			return results, fmt.Errorf("eval %s: %w", item.QuestionID, err)
		}

		results = append(results, *result)

		if verbose {
			fmt.Fprintf(os.Stderr, "  → %s (%.1fms)\n", result.JudgeVerdict, result.LatencyMS)
		}
	}

	return results, nil
}

func ingestLongMemHaystack(ctx context.Context, eng *evalEngine, item *LongMemEvalItem) error {
	for i, session := range item.HaystackSessions {
		sessionID := "unknown"
		if i < len(item.HaystackSessionIDs) {
			sessionID = item.HaystackSessionIDs[i]
		}

		var createdAt *time.Time
		if i < len(item.HaystackDates) {
			if t, err := parseLongMemDate(item.HaystackDates[i]); err == nil {
				createdAt = &t
			}
		}

		for turnIdx, turn := range session {
			content := turn.Content
			if len(content) > 16000 {
				content = content[:16000]
			}
			_, err := eng.WriteWithEmbedding(ctx, &mbp.WriteRequest{
				Vault:     "eval",
				Concept:   fmt.Sprintf("%s: turn %d of %s", turn.Role, turnIdx+1, sessionID),
				Content:   content,
				Tags:      []string{"longmemeval", item.QuestionID, sessionID},
				CreatedAt: createdAt,
			})
			if err != nil {
				return fmt.Errorf("write turn %d of %s: %w", turnIdx, sessionID, err)
			}
		}
	}
	return nil
}

func evalLongMemQuestion(ctx context.Context, eng *evalEngine, item *LongMemEvalItem, judge Judge) (*LongMemEvalResult, error) {
	start := time.Now()

	resp, err := eng.Engine.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "eval",
		Context:    []string{item.Question},
		MaxResults: 50,
			Threshold:  0.1,
	})
	if err != nil {
		return nil, err
	}

	var parts []string
	for _, act := range resp.Activations {
		parts = append(parts, act.Content)
	}
	retrieved := strings.Join(parts, "\n---\n")
	latency := float64(time.Since(start).Microseconds()) / 1000.0

	verdict := "no"
	if judge != nil {
		v, err := judge.Score(ctx, item.Question, item.Answer(), retrieved, item.QuestionType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "judge error for %s: %v\n", item.QuestionID, err)
		} else {
			verdict = v
		}
	}

	accuracy := 0.0
	if verdict == "yes" {
		accuracy = 1.0
	}

	return &LongMemEvalResult{
		QuestionID:   item.QuestionID,
		QuestionType: item.QuestionType,
		Ability:      MapAbility(item.QuestionType),
		Question:     item.Question,
		Answer:       item.Answer(),
		Retrieved:    retrieved,
		JudgeVerdict: verdict,
		Accuracy:     accuracy,
		LatencyMS:    latency,
	}, nil
}

// parseLongMemDate parses dates like "2023/04/10 (Mon) 23:07".
func parseLongMemDate(s string) (time.Time, error) {
	clean := s
	if lp := strings.Index(s, "("); lp >= 0 {
		if rp := strings.Index(s[lp:], ")"); rp >= 0 {
			clean = strings.TrimSpace(s[:lp]) + " " + strings.TrimSpace(s[lp+rp+1:])
		}
	}
	return time.Parse("2006/01/02 15:04", strings.TrimSpace(clean))
}

// SummarizeLongMemEval computes aggregate metrics.
func SummarizeLongMemEval(results []LongMemEvalResult) LongMemEvalSummary {
	if len(results) == 0 {
		return LongMemEvalSummary{}
	}

	abilityCorrect := make(map[string]float64)
	abilityCount := make(map[string]float64)
	totalCorrect := 0.0
	totalLatency := 0.0

	for _, r := range results {
		totalCorrect += r.Accuracy
		totalLatency += r.LatencyMS
		abilityCorrect[r.Ability] += r.Accuracy
		abilityCount[r.Ability]++
	}

	byAbility := make(map[string]float64)
	for ability, count := range abilityCount {
		if count > 0 {
			byAbility[ability] = abilityCorrect[ability] / count
		}
	}

	return LongMemEvalSummary{
		TotalQuestions:  len(results),
		OverallAccuracy: totalCorrect / float64(len(results)),
		ByAbility:       byAbility,
		AvgLatencyMS:    totalLatency / float64(len(results)),
	}
}
