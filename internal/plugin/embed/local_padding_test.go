//go:build localassets

package embed

import (
	"context"
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
