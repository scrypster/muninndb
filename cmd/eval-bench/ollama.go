package main

import (
	"context"
	"strings"

	"github.com/scrypster/muninndb/internal/plugin/embed"
)

// OllamaEmbedder adapts embed.OllamaProvider to the activation.Embedder interface.
type OllamaEmbedder struct {
	provider *embed.OllamaProvider
	Dim      int
}

// NewOllamaEmbedder initializes an Ollama embedding provider.
func NewOllamaEmbedder(ctx context.Context, baseURL, model string) (*OllamaEmbedder, error) {
	p := &embed.OllamaProvider{}
	dim, err := p.Init(ctx, embed.ProviderHTTPConfig{
		BaseURL: baseURL,
		Model:   model,
	})
	if err != nil {
		return nil, err
	}
	return &OllamaEmbedder{provider: p, Dim: dim}, nil
}

func (o *OllamaEmbedder) Embed(ctx context.Context, texts []string) ([]float32, error) {
	return o.provider.EmbedBatch(ctx, texts)
}

func (o *OllamaEmbedder) Tokenize(text string) []string {
	return strings.Fields(strings.ToLower(text))
}

func (o *OllamaEmbedder) Close() error {
	return o.provider.Close()
}

// LocalEmbedder adapts embed.LocalProvider (ONNX bge-small) to the activation.Embedder interface.
type LocalEmbedder struct {
	provider *embed.LocalProvider
	Dim      int
}

// NewLocalEmbedder initializes the built-in ONNX bge-small embedder.
// Requires build tag: -tags localassets
func NewLocalEmbedder(ctx context.Context, dataDir string) (*LocalEmbedder, error) {
	p := &embed.LocalProvider{}
	dim, err := p.Init(ctx, embed.ProviderHTTPConfig{DataDir: dataDir})
	if err != nil {
		return nil, err
	}
	return &LocalEmbedder{provider: p, Dim: dim}, nil
}

func (l *LocalEmbedder) Embed(ctx context.Context, texts []string) ([]float32, error) {
	return l.provider.EmbedBatch(ctx, texts)
}

func (l *LocalEmbedder) Tokenize(text string) []string {
	return strings.Fields(strings.ToLower(text))
}

func (l *LocalEmbedder) Close() error {
	return l.provider.Close()
}
