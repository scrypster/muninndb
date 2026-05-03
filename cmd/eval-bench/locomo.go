package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// LocomoSample is one of the 10 conversations in locomo10.json.
type LocomoSample struct {
	SampleID     string                     `json:"sample_id"`
	Conversation map[string]json.RawMessage `json:"conversation"`
	QA           []LocomoQA                 `json:"qa"`
}

type LocomoQA struct {
	Question string          `json:"question"`
	RawAnswer json.RawMessage `json:"answer"`
	Evidence []string         `json:"evidence"`
	Category int              `json:"category"`
}

// Answer returns the answer as a string, handling both string and numeric JSON values.
func (q *LocomoQA) Answer() string {
	var s string
	if err := json.Unmarshal(q.RawAnswer, &s); err == nil {
		return s
	}
	// Numeric or other type — use raw JSON representation.
	return strings.Trim(string(q.RawAnswer), "\"")
}

type LocomoTurn struct {
	Speaker string `json:"speaker"`
	DiaID   string `json:"dia_id"`
	Text    string `json:"text"`
}

type LocomoResult struct {
	SampleID     string
	Category     int
	Question     string
	Answer       string
	Retrieved    string
	F1Score      float64
	AnswerRecall float64
	LatencyMS    float64
}

type LocomoSummary struct {
	Total         int
	OverallF1     float64
	OverallRecall float64
	ByCategory    map[int]CategoryMetrics
}

type CategoryMetrics struct {
	Count     int
	AvgF1     float64
	AvgRecall float64
}

var sessionKeyRe = regexp.MustCompile(`^session_(\d+)$`)

// RunLocomo loads the dataset and evaluates all QA pairs.
func RunLocomo(ctx context.Context, path string, factory EngineFactory, verbose bool) ([]LocomoResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read locomo: %w", err)
	}
	var samples []LocomoSample
	if err := json.Unmarshal(data, &samples); err != nil {
		return nil, fmt.Errorf("parse locomo: %w", err)
	}

	var results []LocomoResult

	for si, sample := range samples {
		if verbose {
			fmt.Fprintf(os.Stderr, "[%d/%d] %s (%d QA pairs)...\n", si+1, len(samples), sample.SampleID, len(sample.QA))
		}

		// Create engine for this sample.
		tmpDir, err := os.MkdirTemp("", "locomo-eval-*")
		if err != nil {
			return results, err
		}

		eng, err := factory(tmpDir)
		if err != nil {
			os.RemoveAll(tmpDir)
			return results, fmt.Errorf("create engine for %s: %w", sample.SampleID, err)
		}

		// Ingest all sessions.
		if err := ingestLocomoSample(ctx, eng, &sample); err != nil {
			eng.Close()
			return results, fmt.Errorf("ingest %s: %w", sample.SampleID, err)
		}

		// Evaluate all QA pairs for this sample.
		for qi, qa := range sample.QA {
			start := time.Now()

			resp, err := eng.Engine.Activate(ctx, &mbp.ActivateRequest{
				Vault:      "eval",
				Context:    []string{qa.Question},
				MaxResults: 10,
				Threshold:  0.1,
			})
			if err != nil {
				eng.Close()
				return results, fmt.Errorf("activate %s q%d: %w", sample.SampleID, qi, err)
			}

			var parts []string
			for _, act := range resp.Activations {
				parts = append(parts, act.Content)
			}
			retrieved := strings.Join(parts, " ")
			latency := float64(time.Since(start).Microseconds()) / 1000.0

			answer := qa.Answer()
			var f1, recall float64
			switch qa.Category {
			case 1: // multi-hop
				f1 = TokenF1MultiHop(retrieved, answer)
				recall = AnswerRecallMultiHop(retrieved, answer)
			case 5: // adversarial
				f1 = AdversarialScore(retrieved)
				recall = f1 // same metric for adversarial
			default:
				f1 = TokenF1(retrieved, answer)
				recall = AnswerRecall(retrieved, answer)
			}

			results = append(results, LocomoResult{
				SampleID:     sample.SampleID,
				Category:     qa.Category,
				Question:     qa.Question,
				Answer:       answer,
				Retrieved:    retrieved,
				F1Score:      f1,
				AnswerRecall: recall,
				LatencyMS:    latency,
			})
		}

		eng.Close()

		if verbose {
			fmt.Fprintf(os.Stderr, "  → %d QA evaluated\n", len(sample.QA))
		}
	}

	return results, nil
}

func ingestLocomoSample(ctx context.Context, eng *evalEngine, sample *LocomoSample) error {
	// Extract sorted session keys.
	var sessionKeys []string
	for k := range sample.Conversation {
		if sessionKeyRe.MatchString(k) {
			sessionKeys = append(sessionKeys, k)
		}
	}
	sort.Strings(sessionKeys)

	for _, key := range sessionKeys {
		// Parse session turns.
		var turns []LocomoTurn
		if err := json.Unmarshal(sample.Conversation[key], &turns); err != nil {
			continue // might be a non-array value
		}

		// Parse session date.
		dateKey := key + "_date_time"
		var createdAt *time.Time
		if raw, ok := sample.Conversation[dateKey]; ok {
			var dateStr string
			if err := json.Unmarshal(raw, &dateStr); err == nil {
				if t, err := parseLocomoDate(dateStr); err == nil {
					createdAt = &t
				}
			}
		}

		for _, turn := range turns {
			_, err := eng.WriteWithEmbedding(ctx, &mbp.WriteRequest{
				Vault:     "eval",
				Concept:   fmt.Sprintf("%s (%s)", turn.Speaker, turn.DiaID),
				Content:   turn.Text,
				Tags:      []string{"locomo", sample.SampleID, key},
				CreatedAt: createdAt,
			})
			if err != nil {
				return fmt.Errorf("write %s: %w", turn.DiaID, err)
			}
		}
	}
	return nil
}

// parseLocomoDate parses dates like "March 5, 2023, 2:00 PM".
func parseLocomoDate(s string) (time.Time, error) {
	layouts := []string{
		"January 2, 2006, 3:04 PM",
		"January 2, 2006, 3:04PM",
		"January 2, 2006",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable date: %q", s)
}

// SummarizeLocomo computes aggregate metrics from LocOMo results.
func SummarizeLocomo(results []LocomoResult) LocomoSummary {
	if len(results) == 0 {
		return LocomoSummary{}
	}

	catF1 := make(map[int]float64)
	catRecall := make(map[int]float64)
	catCount := make(map[int]int)
	totalF1 := 0.0
	totalRecall := 0.0

	for _, r := range results {
		totalF1 += r.F1Score
		totalRecall += r.AnswerRecall
		catF1[r.Category] += r.F1Score
		catRecall[r.Category] += r.AnswerRecall
		catCount[r.Category]++
	}

	byCategory := make(map[int]CategoryMetrics)
	for cat, count := range catCount {
		byCategory[cat] = CategoryMetrics{
			Count:     count,
			AvgF1:     catF1[cat] / float64(count),
			AvgRecall: catRecall[cat] / float64(count),
		}
	}

	return LocomoSummary{
		Total:         len(results),
		OverallF1:     totalF1 / float64(len(results)),
		OverallRecall: totalRecall / float64(len(results)),
		ByCategory:    byCategory,
	}
}
