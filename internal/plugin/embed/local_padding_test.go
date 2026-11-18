//go:build localassets

package embed

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	ort "github.com/yalue/onnxruntime_go"
)

// tokenCount is the number of tokens the provider's tokenizer produces for
// text, special tokens included: the length a batch holding only this text
// should be padded to.
func tokenCount(t *testing.T, p *LocalProvider, text string) int {
	t.Helper()
	enc, err := p.tok.EncodeSingle(text, true)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	return len(enc.GetIds())
}

// A batch is padded to its longest text, not to the model's maximum length:
// a short recall query must not pay for a full window of padding (on a
// 2-core host, a 512-token model spent about a second per query on it).
func TestLocalProvider_PadsToLongestInBatch(t *testing.T) {
	p := newLaneProvider(t)
	var got []int
	p.onPacked = func(n int) { got = append(got, n) }
	ctx := context.Background()

	short := laneQuery
	long := laneTexts(1)[0]
	huge := strings.Repeat(long+" ", 20) // well past the model's window

	if _, err := p.EmbedBatch(ctx, []string{short}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.EmbedBatch(ctx, []string{short, long}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.EmbedBatch(ctx, []string{short, huge}); err != nil {
		t.Fatal(err)
	}

	want := []int{tokenCount(t, p, short), tokenCount(t, p, long), p.maxTokens}
	if want[0] >= want[1] || want[1] >= p.maxTokens {
		t.Fatalf("fixture lengths %v must be strictly increasing below the window %d", want[:2], p.maxTokens)
	}
	if len(got) != len(want) {
		t.Fatalf("packed %d batches, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("batch %d padded to %d tokens, want %d (all: got %v want %v)", i, got[i], want[i], got, want)
		}
	}
}

func TestHasFixedSequenceLength(t *testing.T) {
	in := func(dims ...int64) ort.InputOutputInfo {
		return ort.InputOutputInfo{Name: "input_ids", Dimensions: ort.NewShape(dims...)}
	}
	cases := []struct {
		name   string
		inputs []ort.InputOutputInfo
		want   bool
	}{
		{"dynamic batch and sequence", []ort.InputOutputInfo{in(-1, -1), in(-1, -1)}, false},
		{"dynamic sequence reported as zero", []ort.InputOutputInfo{in(-1, 0)}, false},
		{"fixed sequence", []ort.InputOutputInfo{in(-1, 512), in(-1, 512)}, true},
		{"fixed sequence on one input only", []ort.InputOutputInfo{in(-1, -1), in(1, 256)}, true},
		{"unexpected rank is left alone", []ort.InputOutputInfo{in(-1, 128, 4)}, false},
	}
	for _, c := range cases {
		if got := hasFixedSequenceLength(c.inputs); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// userModelFixture writes the bundled model and tokenizer to files so they
// can be loaded through the user-model path.
func userModelFixture(t *testing.T) (modelPath, tokPath string) {
	t.Helper()
	if !LocalAvailable() {
		t.Skip("assets not embedded — run `make fetch-assets` and rebuild with -tags localassets")
	}
	dir := t.TempDir()
	modelPath = filepath.Join(dir, "user-model.onnx")
	tokPath = filepath.Join(dir, "tokenizer.json")
	if err := atomicWrite(modelPath, embeddedModel); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(tokPath, embeddedTokenizer); err != nil {
		t.Fatal(err)
	}
	return modelPath, tokPath
}

// An embed_max_tokens beyond the model's position capacity must fail Init,
// as it did when every batch was padded to the window: if it passes, every
// batch holding a long text fails at runtime instead, and the retroactive
// processor flags all of that batch's memories as unembeddable.
func TestUserModel_MaxTokensBeyondCapacityFailsInit(t *testing.T) {
	modelPath, tokPath := userModelFixture(t)
	p := &LocalProvider{}
	_, err := p.Init(context.Background(), ProviderHTTPConfig{
		DataDir: t.TempDir(), LocalModelPath: modelPath, LocalTokenizerPath: tokPath,
		LocalMaxTokens: 1024, // the bundled export has 512 positions
	})
	if err == nil {
		_ = p.Close()
		t.Fatal("Init accepted embed_max_tokens 1024 on a 512-position model")
	}
}

// A model that only runs at one sequence length keeps being fed full-window
// batches, whether it declares that length on its inputs or hides it behind a
// dynamic axis (testdata/gen_fixedlen.py).
func TestUserModel_FixedLengthModelsStillEmbed(t *testing.T) {
	_, tokPath := userModelFixture(t)
	for _, name := range []string{"fixedlen8_declared.onnx", "fixedlen8_hidden.onnx"} {
		t.Run(name, func(t *testing.T) {
			p := &LocalProvider{}
			dim, err := p.Init(context.Background(), ProviderHTTPConfig{
				DataDir: t.TempDir(), LocalModelPath: filepath.Join("testdata", name),
				LocalTokenizerPath: tokPath, LocalMaxTokens: 8,
			})
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			defer p.Close()
			if dim != 4 {
				t.Fatalf("dim = %d, want 4", dim)
			}
			if !p.padToMax {
				t.Error("padToMax = false for a model that only runs at its full window")
			}
			// Every text here is shorter than the 8-token window, so only
			// full-window padding lets the model run.
			for _, batch := range [][]string{{"short"}, {"short", "a"}} {
				if n := tokenCount(t, p, batch[0]); n >= p.maxTokens {
					t.Fatalf("fixture %q is %d tokens, must be under the window %d", batch[0], n, p.maxTokens)
				}
				v, err := p.EmbedBatch(context.Background(), batch)
				if err != nil {
					t.Fatalf("EmbedBatch(%q): %v", batch, err)
				}
				if len(v) != len(batch)*dim {
					t.Fatalf("got %d floats, want %d", len(v), len(batch)*dim)
				}
			}
		})
	}
}

// The bundled export's sequence axis is dynamic: loaded as a user model it
// must get fit padding, not the fixed-length fallback.
func TestUserModel_DynamicModelGetsFitPadding(t *testing.T) {
	modelPath, tokPath := userModelFixture(t)
	p := &LocalProvider{}
	if _, err := p.Init(context.Background(), ProviderHTTPConfig{
		DataDir: t.TempDir(), LocalModelPath: modelPath, LocalTokenizerPath: tokPath,
	}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.padToMax {
		t.Error("padToMax = true for a dynamic-axis model")
	}
}
