package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// captureTagsEngine records what handleUpdateTags forwards to the engine.
// Embeds fakeEngine so it satisfies the full EngineInterface.
type captureTagsEngine struct {
	fakeEngine
	calls   int
	gotID   string
	gotTags []string
}

func (e *captureTagsEngine) UpdateTags(_ context.Context, _, id string, tags []string) error {
	e.calls++
	e.gotID = id
	e.gotTags = tags
	return nil
}

const testEngramID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// TestUpdateTags_ForwardedToEngine is the assertion whose absence let #720
// exist: tags were settable only at creation, and passing `tags` to
// muninn_evolve returned success with the tags silently discarded.
func TestUpdateTags_ForwardedToEngine(t *testing.T) {
	eng := &captureTagsEngine{}
	srv := New(":0", eng, "", nil, nil, nil)

	body := mkToolCallBody("muninn_update_tags", map[string]any{
		"vault": "default",
		"id":    testEngramID,
		"tags":  []any{"due:2026-08-01", "project:muninn"},
	})
	w := doAuthenticatedPost(srv, "", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}
	if eng.calls != 1 {
		t.Fatalf("engine UpdateTags called %d times, want 1", eng.calls)
	}
	if eng.gotID != testEngramID {
		t.Errorf("engine got id %q, want %q", eng.gotID, testEngramID)
	}
	want := []string{"due:2026-08-01", "project:muninn"}
	if len(eng.gotTags) != len(want) {
		t.Fatalf("engine got %d tags (%v), want %d", len(eng.gotTags), eng.gotTags, len(want))
	}
	for i := range want {
		if eng.gotTags[i] != want[i] {
			t.Errorf("tag %d = %q, want %q", i, eng.gotTags[i], want[i])
		}
	}
}

// TestUpdateTags_EmptyArrayClears: an explicit empty array clears the set,
// matching REST, which coerces a nil body field to []string{}.
func TestUpdateTags_EmptyArrayClears(t *testing.T) {
	eng := &captureTagsEngine{}
	srv := New(":0", eng, "", nil, nil, nil)

	body := mkToolCallBody("muninn_update_tags", map[string]any{
		"id":   testEngramID,
		"tags": []any{},
	})
	w := doAuthenticatedPost(srv, "", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}
	if eng.calls != 1 {
		t.Fatalf("engine UpdateTags called %d times, want 1", eng.calls)
	}
	if len(eng.gotTags) != 0 {
		t.Errorf("engine got %v, want an empty tag set", eng.gotTags)
	}
}

// TestUpdateTags_Normalization mirrors muninn_remember's coercion: non-strings
// and empty strings are skipped, tags longer than 128 chars are skipped, and
// the set is capped at 50.
func TestUpdateTags_Normalization(t *testing.T) {
	tooLong := strings.Repeat("x", 129)
	raw := []any{"keep", "", 42, nil, tooLong, strings.Repeat("y", 128), "also-keep"}
	for i := 0; i < 60; i++ {
		raw = append(raw, "bulk")
	}

	eng := &captureTagsEngine{}
	srv := New(":0", eng, "", nil, nil, nil)
	body := mkToolCallBody("muninn_update_tags", map[string]any{
		"id":   testEngramID,
		"tags": raw,
	})
	w := doAuthenticatedPost(srv, "", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}
	if len(eng.gotTags) != 50 {
		t.Fatalf("got %d tags, want the 50-tag cap", len(eng.gotTags))
	}
	for _, got := range eng.gotTags {
		if got == "" || len(got) > 128 {
			t.Errorf("normalization let through %q (len %d)", got, len(got))
		}
	}
	if eng.gotTags[0] != "keep" || eng.gotTags[1] != strings.Repeat("y", 128) || eng.gotTags[2] != "also-keep" {
		t.Errorf("unexpected kept order: %v", eng.gotTags[:3])
	}
}

// failingTagsEngine reports an engine-level failure (e.g. engram not found).
type failingTagsEngine struct {
	fakeEngine
}

func (e *failingTagsEngine) UpdateTags(_ context.Context, _, _ string, _ []string) error {
	return errors.New("engram not found")
}

// TestUpdateTags_EngineError: an engine failure is a tool error (-32000), not
// an invalid-params error (-32602) — the arguments were well-formed.
func TestUpdateTags_EngineError(t *testing.T) {
	srv := New(":0", &failingTagsEngine{}, "", nil, nil, nil)
	body := mkToolCallBody("muninn_update_tags", map[string]any{
		"id":   testEngramID,
		"tags": []any{"a"},
	})
	w := doAuthenticatedPost(srv, "", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected an error when the engine fails, got success")
	}
	if resp.Error.Code != -32000 {
		t.Errorf("code = %d, want -32000", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "engram not found") {
		t.Errorf("the engine's message must survive, got: %q", resp.Error.Message)
	}
}

// TestEvolve_RejectsTags: passing `tags` to muninn_evolve returned success
// with the tags silently discarded (#720). It must now fail loudly and name
// the tool that does the job.
func TestEvolve_RejectsTags(t *testing.T) {
	eng := &captureTagsEngine{}
	srv := New(":0", eng, "", nil, nil, nil)

	body := mkToolCallBody("muninn_evolve", map[string]any{
		"vault":       "default",
		"id":          testEngramID,
		"new_content": "updated content",
		"reason":      "test",
		"tags":        []any{"due:2026-08-01"},
	})
	w := doAuthenticatedPost(srv, "", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected muninn_evolve to reject 'tags', got success (the tags would be silently dropped)")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("code = %d, want -32602", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "muninn_update_tags") {
		t.Errorf("the error must teach the correct tool, got: %q", resp.Error.Message)
	}
}

// TestUpdateTags_Validation: bad args are rejected before any engine call.
func TestUpdateTags_Validation(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		wantMsg string
	}{
		{"missing id", map[string]any{"tags": []any{"a"}}, "'id' is required"},
		{"empty id", map[string]any{"id": "", "tags": []any{"a"}}, "'id' is required"},
		{"missing tags", map[string]any{"id": testEngramID}, "'tags' is required"},
		{"tags not an array", map[string]any{"id": testEngramID, "tags": "a,b"}, "'tags' must be an array"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &captureTagsEngine{}
			srv := New(":0", eng, "", nil, nil, nil)
			w := doAuthenticatedPost(srv, "", mkToolCallBody("muninn_update_tags", tc.args))

			var resp JSONRPCResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error == nil {
				t.Fatalf("expected an error, got success")
			}
			if resp.Error.Code != -32602 {
				t.Errorf("code = %d, want -32602", resp.Error.Code)
			}
			if !strings.Contains(resp.Error.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", resp.Error.Message, tc.wantMsg)
			}
			if eng.calls != 0 {
				t.Errorf("engine was called %d times on a rejected request", eng.calls)
			}
		})
	}
}
