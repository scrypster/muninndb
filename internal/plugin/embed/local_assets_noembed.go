//go:build !localassets

package embed

import (
	"context"
	"fmt"
)

// Stub declarations used when building without embedded assets (no -tags localassets).
// Run `make fetch-assets` then use `-tags localassets` to build with real assets.

var embeddedNativeLib []byte
var embeddedModel []byte
var embeddedTokenizer []byte

const nativeLibFilename = ""

// LocalAvailable reports whether the bundled ONNX model and tokenizer were
// embedded at build time. Always returns false when built without -tags localassets.
func LocalAvailable() bool {
	return false
}

// LocalProvider is a stub used when building without -tags localassets.
// Init always returns an error directing the caller to use a network provider.
type LocalProvider struct{}

func (p *LocalProvider) Name() string { return "local" }
func (p *LocalProvider) MaxBatchSize() int { return 0 }
func (p *LocalProvider) Init(_ context.Context, _ ProviderHTTPConfig) (int, error) {
	return 0, errLocalUnavailable
}
func (p *LocalProvider) EmbedBatch(_ context.Context, _ []string) ([]float32, error) {
	return nil, errLocalUnavailable
}
func (p *LocalProvider) Close() error { return nil }

var errLocalUnavailable = fmt.Errorf("local embedder not available: build with -tags localassets or use a network provider (ollama, openai, etc.)")
