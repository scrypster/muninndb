package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Judge scores whether retrieved content answers a question correctly.
type Judge interface {
	Score(ctx context.Context, question, expectedAnswer, retrievedContent, questionType string) (verdict string, err error)
}

// LocalJudge calls a local llama.cpp-compatible OpenAI API.
type LocalJudge struct {
	URL   string
	Model string
}

// OpenRouterJudge calls the OpenRouter API with rate limiting.
type OpenRouterJudge struct {
	APIKey string
	Model  string

	mu       sync.Mutex
	lastCall time.Time
}

// NewJudge returns a Judge preferring local when available.
func NewJudge(localURL, openrouterKey string) Judge {
	if localURL != "" {
		return &LocalJudge{URL: localURL, Model: "llama3.3:latest"}
	}
	return &OpenRouterJudge{
		APIKey: openrouterKey,
		Model:  "nousresearch/hermes-3-llama-3.1-405b:free",
	}
}

func (j *LocalJudge) Score(ctx context.Context, question, expectedAnswer, retrievedContent, questionType string) (string, error) {
	prompt := buildJudgePrompt(question, expectedAnswer, retrievedContent, questionType)
	return callChatCompletion(ctx, j.URL, j.Model, "", prompt)
}

func (j *OpenRouterJudge) Score(ctx context.Context, question, expectedAnswer, retrievedContent, questionType string) (string, error) {
	// Rate limit: 20 req/min → minimum 3s between calls.
	// Hold lock during sleep to prevent concurrent goroutines from passing the check.
	j.mu.Lock()
	elapsed := time.Since(j.lastCall)
	if elapsed < 3*time.Second {
		time.Sleep(3*time.Second - elapsed)
	}
	j.lastCall = time.Now()
	j.mu.Unlock()

	prompt := buildJudgePrompt(question, expectedAnswer, retrievedContent, questionType)
	return callChatCompletion(ctx, "https://openrouter.ai/api/v1", j.Model, j.APIKey, prompt)
}

func buildJudgePrompt(question, expectedAnswer, retrievedContent, questionType string) string {
	if strings.HasSuffix(questionType, "_abs") {
		return fmt.Sprintf(`You are evaluating whether a memory system correctly identified a question as unanswerable.

Question: %s
Expected behavior: The system should indicate this question cannot be answered from the available information.
Retrieved content: %s

Did the retrieved content correctly indicate the question is unanswerable, or fail to provide a confident answer?
Answer with exactly "yes" or "no".`, question, retrievedContent)
	}

	switch {
	case strings.Contains(questionType, "temporal"):
		return fmt.Sprintf(`You are evaluating whether a memory system retrieved relevant information to answer a temporal reasoning question.

Question: %s
Expected answer: %s
Retrieved content: %s

Does the retrieved content contain sufficient information to answer the question correctly? Allow for off-by-one errors in day counts.
Answer with exactly "yes" or "no".`, question, expectedAnswer, retrievedContent)

	case strings.Contains(questionType, "knowledge-update"):
		return fmt.Sprintf(`You are evaluating whether a memory system retrieved the most up-to-date information.

Question: %s
Expected answer (updated): %s
Retrieved content: %s

Does the retrieved content include the updated answer?
Answer with exactly "yes" or "no".`, question, expectedAnswer, retrievedContent)

	default:
		return fmt.Sprintf(`You are evaluating whether a memory system retrieved relevant information to answer a question.

Question: %s
Expected answer: %s
Retrieved content: %s

Does the retrieved content contain sufficient information to answer the question correctly?
Answer with exactly "yes" or "no".`, question, expectedAnswer, retrievedContent)
	}
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func callChatCompletion(ctx context.Context, baseURL, model, apiKey, prompt string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:       model,
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		Temperature: 0,
		MaxTokens:   10,
	})
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("judge request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("judge API %d: %s", resp.StatusCode, string(respBody))
	}

	var cr chatResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("judge error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("judge returned no choices")
	}

	raw := strings.TrimSpace(strings.ToLower(cr.Choices[0].Message.Content))
	if strings.HasPrefix(raw, "yes") {
		return "yes", nil
	}
	return "no", nil
}
