package embed

import (
	"context"
	"fmt"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
)

type stubEmbedPlugin struct {
	embedResult []float32
	embedErr    error
	dim         int
}

func (s *stubEmbedPlugin) Name() string            { return "stub" }
func (s *stubEmbedPlugin) Tier() plugin.PluginTier { return plugin.TierEmbed }
func (s *stubEmbedPlugin) Init(_ context.Context, _ plugin.PluginConfig) error {
	return nil
}
func (s *stubEmbedPlugin) Close() error      { return nil }
func (s *stubEmbedPlugin) Dimension() int    { return s.dim }
func (s *stubEmbedPlugin) MaxBatchSize() int { return 32 }
func (s *stubEmbedPlugin) Embed(_ context.Context, _ []string) ([]float32, error) {
	return s.embedResult, s.embedErr
}

func TestNewEmbedServiceAdapter(t *testing.T) {
	stub := &stubEmbedPlugin{dim: 384}
	adapter := NewEmbedServiceAdapter(stub)
	if adapter == nil {
		t.Fatal("expected non-nil adapter")
	}
}

func TestEmbedServiceAdapter_Embed(t *testing.T) {
	expected := []float32{0.1, 0.2, 0.3}
	stub := &stubEmbedPlugin{embedResult: expected}
	adapter := NewEmbedServiceAdapter(stub)

	result, err := adapter.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	if len(result) != len(expected) {
		t.Errorf("expected %d values, got %d", len(expected), len(result))
	}
	for i, v := range result {
		if v != expected[i] {
			t.Errorf("result[%d]: expected %f, got %f", i, expected[i], v)
		}
	}
}

func TestEmbedServiceAdapter_Embed_Error(t *testing.T) {
	stub := &stubEmbedPlugin{embedErr: fmt.Errorf("test error")}
	adapter := NewEmbedServiceAdapter(stub)

	_, err := adapter.Embed(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestEmbedServiceAdapter_Tokenize(t *testing.T) {
	stub := &stubEmbedPlugin{}
	adapter := NewEmbedServiceAdapter(stub)

	a := adapter.(*embedServiceAdapter)
	tokens := a.Tokenize("hello world foo")
	if len(tokens) != 3 {
		t.Errorf("expected 3 tokens, got %d", len(tokens))
	}
	if tokens[0] != "hello" || tokens[1] != "world" || tokens[2] != "foo" {
		t.Errorf("unexpected tokens: %v", tokens)
	}
}

func TestEmbedServiceAdapter_Tokenize_Empty(t *testing.T) {
	stub := &stubEmbedPlugin{}
	adapter := NewEmbedServiceAdapter(stub)

	a := adapter.(*embedServiceAdapter)
	tokens := a.Tokenize("")
	if len(tokens) != 0 {
		t.Errorf("expected 0 tokens for empty string, got %d", len(tokens))
	}
}

// recordingEmbedPlugin captures the texts passed to Embed so prefix wrapping
// can be asserted.
type recordingEmbedPlugin struct {
	stubEmbedPlugin
	gotTexts []string
	hardware bool
}

func (r *recordingEmbedPlugin) Embed(_ context.Context, texts []string) ([]float32, error) {
	r.gotTexts = append([]string(nil), texts...)
	return r.embedResult, r.embedErr
}

func (r *recordingEmbedPlugin) HardwareAccelerated() bool { return r.hardware }

func TestNewPrefixedEmbedPlugin_EmptyPrefixReturnsUnwrapped(t *testing.T) {
	stub := &recordingEmbedPlugin{}
	if got := NewPrefixedEmbedPlugin(stub, ""); got != plugin.EmbedPlugin(stub) {
		t.Fatal("empty prefix must return the plugin unchanged")
	}
}

func TestNewPrefixedEmbedPlugin_PrependsPrefix(t *testing.T) {
	stub := &recordingEmbedPlugin{}
	wrapped := NewPrefixedEmbedPlugin(stub, "passage: ")

	if _, err := wrapped.Embed(context.Background(), []string{"alpha", "beta"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	want := []string{"passage: alpha", "passage: beta"}
	if len(stub.gotTexts) != len(want) {
		t.Fatalf("inner plugin saw %d texts, want %d", len(stub.gotTexts), len(want))
	}
	for i := range want {
		if stub.gotTexts[i] != want[i] {
			t.Errorf("text[%d] = %q, want %q", i, stub.gotTexts[i], want[i])
		}
	}
}

func TestPrefixedEmbedPlugin_ForwardsHardwareFlag(t *testing.T) {
	stub := &recordingEmbedPlugin{hardware: true}
	wrapped := NewPrefixedEmbedPlugin(stub, "query: ")

	h, ok := wrapped.(plugin.HardwareAwarePlugin)
	if !ok {
		t.Fatal("prefixed plugin must still satisfy HardwareAwarePlugin")
	}
	if !h.HardwareAccelerated() {
		t.Error("hardware flag not forwarded")
	}
}
