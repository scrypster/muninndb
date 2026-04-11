package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/scrypster/muninndb/internal/consolidation"
	enrichpkg "github.com/scrypster/muninndb/internal/plugin/enrich"
)

// buildDreamProviders initialises LLM providers for dream Phase 2b from
// environment variables. Returns a slice ordered by preference: local (Ollama)
// first, then restricted-safe (Anthropic), then open-only (OpenAI). Only
// successfully initialised providers are included.
func buildDreamProviders(ctx context.Context) []consolidation.LLMProvider {
	var providers []consolidation.LLMProvider

	if ollamaURL := os.Getenv("MUNINN_OLLAMA_URL"); ollamaURL != "" {
		p := enrichpkg.NewOllamaLLMProvider()
		model := os.Getenv("MUNINN_OLLAMA_MODEL")
		if model == "" {
			model = "llama3.2"
		}
		pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
		if err := p.Init(pctx, enrichpkg.LLMProviderConfig{BaseURL: ollamaURL, Model: model}); err != nil {
			slog.Warn("dream: ollama LLM init failed", "error", err)
		} else {
			providers = append(providers, p)
		}
		pcancel()
	}

	if apiKey := os.Getenv("MUNINN_ANTHROPIC_KEY"); apiKey != "" {
		p := enrichpkg.NewAnthropicLLMProvider()
		model := os.Getenv("MUNINN_ANTHROPIC_MODEL")
		if model == "" {
			model = "claude-sonnet-4-20250514"
		}
		pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
		if err := p.Init(pctx, enrichpkg.LLMProviderConfig{
			BaseURL: "https://api.anthropic.com", Model: model, APIKey: apiKey,
		}); err != nil {
			slog.Warn("dream: anthropic LLM init failed", "error", err)
		} else {
			providers = append(providers, p)
		}
		pcancel()
	}

	if apiKey := os.Getenv("MUNINN_OPENAI_KEY"); apiKey != "" {
		p := enrichpkg.NewOpenAILLMProvider()
		model := os.Getenv("MUNINN_OPENAI_MODEL")
		if model == "" {
			model = "gpt-4o-mini"
		}
		baseURL := os.Getenv("MUNINN_OPENAI_URL")
		if baseURL == "" {
			baseURL = "https://api.openai.com"
		}
		pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
		if err := p.Init(pctx, enrichpkg.LLMProviderConfig{
			BaseURL: baseURL, Model: model, APIKey: apiKey,
		}); err != nil {
			slog.Warn("dream: openai LLM init failed", "error", err)
		} else {
			providers = append(providers, p)
		}
		pcancel()
	}

	return providers
}
