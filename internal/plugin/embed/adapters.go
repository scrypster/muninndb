package embed

import (
	"context"
	"strings"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/plugin"
)

// embedServiceAdapter wraps plugin.EmbedPlugin to satisfy activation.Embedder.
type embedServiceAdapter struct {
	svc plugin.EmbedPlugin
}

func (a *embedServiceAdapter) Embed(ctx context.Context, texts []string) ([]float32, error) {
	return a.svc.Embed(ctx, texts)
}

func (a *embedServiceAdapter) Tokenize(text string) []string {
	return strings.Fields(text)
}

// NewEmbedServiceAdapter returns an activation.Embedder backed by the given EmbedPlugin.
func NewEmbedServiceAdapter(svc plugin.EmbedPlugin) activation.Embedder {
	return &embedServiceAdapter{svc: svc}
}

// NewPrefixedEmbedPlugin wraps an EmbedPlugin so every embedded text carries
// prefix — the passage side of instruction prefixes (e.g. "passage: " for the
// e5 family, #583). An empty prefix returns the plugin unchanged.
func NewPrefixedEmbedPlugin(svc plugin.EmbedPlugin, prefix string) plugin.EmbedPlugin {
	if prefix == "" {
		return svc
	}
	return &prefixedEmbedPlugin{EmbedPlugin: svc, prefix: prefix}
}

// prefixedEmbedPlugin forwards everything to the wrapped EmbedPlugin and
// prepends a fixed prefix to every text passed to Embed.
type prefixedEmbedPlugin struct {
	plugin.EmbedPlugin
	prefix string
}

func (p *prefixedEmbedPlugin) Embed(ctx context.Context, texts []string) ([]float32, error) {
	prefixed := make([]string, len(texts))
	for i, t := range texts {
		prefixed[i] = p.prefix + t
	}
	return p.EmbedPlugin.Embed(ctx, prefixed)
}

// HardwareAccelerated forwards the wrapped plugin's hardware flag so the
// wrapper does not hide plugin.HardwareAwarePlugin from type assertions.
func (p *prefixedEmbedPlugin) HardwareAccelerated() bool {
	if h, ok := p.EmbedPlugin.(plugin.HardwareAwarePlugin); ok {
		return h.HardwareAccelerated()
	}
	return false
}
