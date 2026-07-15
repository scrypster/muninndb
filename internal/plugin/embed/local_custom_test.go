//go:build localassets

package embed

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	ort "github.com/yalue/onnxruntime_go"
)

// TestUserModel covers the user-supplied local model path (#583) with real
// inference, using the bundled bge-small assets materialized as plain files —
// a real model and tokenizer with no extra downloads. The main CI job runs
// this (localassets tests execute with fetched assets); skipped under -short.
func TestUserModel(t *testing.T) {
	if testing.Short() {
		t.Skip("real ORT inference")
	}
	if !LocalAvailable() {
		t.Skip("assets not embedded — run `make fetch-assets` and rebuild with -tags localassets")
	}

	ctx := context.Background()

	fixtureDir := t.TempDir()
	modelPath := filepath.Join(fixtureDir, "user-model.onnx")
	tokPath := filepath.Join(fixtureDir, "tokenizer.json")
	if err := atomicWrite(modelPath, embeddedModel); err != nil {
		t.Fatalf("write model fixture: %v", err)
	}
	if err := atomicWrite(tokPath, embeddedTokenizer); err != nil {
		t.Fatalf("write tokenizer fixture: %v", err)
	}

	// Shared DataDir so the ORT shared library is extracted once.
	dataDir := t.TempDir()

	newUser := func(t *testing.T, pooling string, maxTokens int) *LocalProvider {
		t.Helper()
		p := &LocalProvider{}
		dim, err := p.Init(ctx, ProviderHTTPConfig{
			DataDir:            dataDir,
			LocalModelPath:     modelPath,
			LocalTokenizerPath: tokPath,
			LocalPooling:       pooling,
			LocalMaxTokens:     maxTokens,
		})
		if err != nil {
			t.Fatalf("user model Init: %v", err)
		}
		t.Cleanup(func() { p.Close() })
		if dim != localModelDim {
			t.Fatalf("probed dimension = %d, want %d", dim, localModelDim)
		}
		return p
	}

	embedOne := func(t *testing.T, p *LocalProvider, text string) []float32 {
		t.Helper()
		vec, err := p.EmbedBatch(ctx, []string{text})
		if err != nil {
			t.Fatalf("EmbedBatch(%q): %v", text, err)
		}
		if len(vec) != localModelDim {
			t.Fatalf("embedding has %d floats, want %d", len(vec), localModelDim)
		}
		var norm float64
		for _, v := range vec {
			norm += float64(v) * float64(v)
		}
		if diff := math.Abs(math.Sqrt(norm) - 1.0); diff > 1e-4 {
			t.Fatalf("embedding norm = %f, want 1.0", math.Sqrt(norm))
		}
		return vec
	}

	const text = "The quick brown fox jumps over the lazy dog"

	t.Run("ProbeDerivesDimensionAndClsMatchesBundled", func(t *testing.T) {
		bundled := &LocalProvider{}
		if _, err := bundled.Init(ctx, ProviderHTTPConfig{DataDir: dataDir}); err != nil {
			t.Fatalf("bundled Init: %v", err)
		}
		t.Cleanup(func() { bundled.Close() })

		user := newUser(t, "cls", 0)

		vb := embedOne(t, bundled, text)
		vu := embedOne(t, user, text)
		for i := range vb {
			if math.Abs(float64(vb[i]-vu[i])) > 1e-6 {
				t.Fatalf("user[%d]=%f differs from bundled[%d]=%f — same model files must produce identical embeddings", i, vu[i], i, vb[i])
			}
		}
	})

	t.Run("MeanPoolingDiffersFromCls", func(t *testing.T) {
		userCls := newUser(t, "cls", 0)
		userMean := newUser(t, "mean", 0)

		vc := embedOne(t, userCls, text)
		vm := embedOne(t, userMean, text)
		differs := false
		for i := range vc {
			if math.Abs(float64(vc[i]-vm[i])) > 1e-4 {
				differs = true
				break
			}
		}
		if !differs {
			t.Fatal("mean-pooled embedding equals CLS embedding — pooling config had no effect")
		}
	})

	t.Run("FailLoud", func(t *testing.T) {
		cases := []struct {
			name string
			cfg  ProviderHTTPConfig
		}{
			{"missing model file", ProviderHTTPConfig{DataDir: dataDir, LocalModelPath: filepath.Join(fixtureDir, "nope.onnx"), LocalTokenizerPath: tokPath}},
			{"missing tokenizer file", ProviderHTTPConfig{DataDir: dataDir, LocalModelPath: modelPath, LocalTokenizerPath: filepath.Join(fixtureDir, "nope.json")}},
			{"model path without tokenizer path", ProviderHTTPConfig{DataDir: dataDir, LocalModelPath: modelPath}},
			{"unknown pooling", ProviderHTTPConfig{DataDir: dataDir, LocalModelPath: modelPath, LocalTokenizerPath: tokPath, LocalPooling: "max"}},
			{"negative max tokens", ProviderHTTPConfig{DataDir: dataDir, LocalModelPath: modelPath, LocalTokenizerPath: tokPath, LocalMaxTokens: -1}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				p := &LocalProvider{}
				if _, err := p.Init(ctx, tc.cfg); err == nil {
					p.Close()
					t.Fatal("Init succeeded, want a loud error")
				}
			})
		}

		t.Run("garbage model bytes", func(t *testing.T) {
			garbagePath := filepath.Join(fixtureDir, "garbage.onnx")
			if err := os.WriteFile(garbagePath, []byte("this is not an onnx model"), 0o600); err != nil {
				t.Fatalf("write garbage: %v", err)
			}
			p := &LocalProvider{}
			if _, err := p.Init(ctx, ProviderHTTPConfig{DataDir: dataDir, LocalModelPath: garbagePath, LocalTokenizerPath: tokPath}); err == nil {
				p.Close()
				t.Fatal("Init succeeded on a garbage model file, want a loud error")
			}
		})
	})
}

func TestResolveModelInputs(t *testing.T) {
	mk := func(names ...string) []ort.InputOutputInfo {
		infos := make([]ort.InputOutputInfo, len(names))
		for i, n := range names {
			infos[i] = ort.InputOutputInfo{Name: n}
		}
		return infos
	}

	got, err := resolveModelInputs(mk("input_ids", "attention_mask", "token_type_ids"))
	if err != nil || len(got) != 3 {
		t.Fatalf("full BERT inputs: got %v, %v", got, err)
	}

	// XLM-R-style export without token_type_ids.
	got, err = resolveModelInputs(mk("input_ids", "attention_mask"))
	if err != nil || len(got) != 2 {
		t.Fatalf("two-input model: got %v, %v", got, err)
	}

	if _, err = resolveModelInputs(mk("input_ids", "pixel_values")); err == nil {
		t.Fatal("unknown input must be refused")
	}
	// input_ids and attention_mask are required — a model missing either is
	// not a BERT-style text encoder this provider can serve.
	if _, err = resolveModelInputs(mk("input_ids")); err == nil {
		t.Fatal("model without attention_mask must be refused")
	}
	if _, err = resolveModelInputs(nil); err == nil {
		t.Fatal("no inputs must be refused")
	}
}

func TestResolveModelOutput(t *testing.T) {
	mk := func(names ...string) []ort.InputOutputInfo {
		infos := make([]ort.InputOutputInfo, len(names))
		for i, n := range names {
			infos[i] = ort.InputOutputInfo{Name: n}
		}
		return infos
	}

	if name, err := resolveModelOutput(mk("last_hidden_state", "pooler_output")); err != nil || name != "last_hidden_state" {
		t.Fatalf("last_hidden_state preferred: got %q, %v", name, err)
	}
	if name, err := resolveModelOutput(mk("hidden")); err != nil || name != "hidden" {
		t.Fatalf("single output used: got %q, %v", name, err)
	}
	if _, err := resolveModelOutput(mk("a", "b")); err == nil {
		t.Fatal("ambiguous outputs must be refused")
	}
}
