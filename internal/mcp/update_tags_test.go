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

// TestUpdateTags_AllEntriesRejectedIsAnError pins the loud-rejection rule
// (#720 review, finding 4).
//
// Normalization silently drops entries, and an explicit empty array is a
// deliberate CLEAR. Composed, those two reasonable rules produce an
// unreasonable outcome: a caller that sends a non-empty tags array where every
// entry is unusable — the shape an LLM produces when it emits numbers, nulls,
// or an over-long generated tag — gets `"ok": true` for a call that WIPED the
// engram's tags. Destructive, silent, and reported as success. That is the
// silently-wrong class this project treats as the worst failure mode, and
// nothing in the response distinguishes it from a clear the caller meant.
//
// The error must also NAME the offending entry, because a caller that cannot
// see which entry failed cannot fix its output.
//
// RED (handleUpdateTags reverted to plain normalizeTags with no guard): each
// case returns success and the engine is called with an empty tag set.
func TestUpdateTags_AllEntriesRejectedIsAnError(t *testing.T) {
	cases := []struct {
		name    string
		tags    []any
		wantMsg string
	}{
		{"all non-strings", []any{42, nil, true}, "not a string"},
		{"all empty strings", []any{"", ""}, "empty string"},
		{"all over the byte limit", []any{strings.Repeat("x", 129)}, "over the 128-byte limit"},
		{"mixed rejects", []any{42, "", nil}, "no usable tag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &captureTagsEngine{}
			srv := New(":0", eng, "", nil, nil, nil)
			w := doAuthenticatedPost(srv, "", mkToolCallBody("muninn_update_tags", map[string]any{
				"id":   testEngramID,
				"tags": tc.tags,
			}))

			var resp JSONRPCResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error == nil {
				t.Fatalf("a non-empty tags array that normalizes to empty returned success — "+
					"the engram's tags were wiped and the caller was told %v", resp.Result)
			}
			if resp.Error.Code != -32602 {
				t.Errorf("code = %d, want -32602 (the arguments are the problem)", resp.Error.Code)
			}
			if !strings.Contains(resp.Error.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to name the offending entry (%q)", resp.Error.Message, tc.wantMsg)
			}
			if !strings.Contains(resp.Error.Message, "empty array") {
				t.Errorf("message = %q, want it to point at the deliberate-clear path", resp.Error.Message)
			}
			if eng.calls != 0 {
				t.Errorf("engine was called %d times — the destructive write must not reach it", eng.calls)
			}
		})
	}
}

// TestUpdateTags_PartialNormalizationReportsDropped: when SOME entries survive,
// the call still succeeds (the caller's intent is recoverable), but the response
// says how many entries were dropped and why. Without this the caller cannot
// tell a 3-tag request that stored 3 from one that stored 1.
func TestUpdateTags_PartialNormalizationReportsDropped(t *testing.T) {
	eng := &captureTagsEngine{}
	srv := New(":0", eng, "", nil, nil, nil)
	w := doAuthenticatedPost(srv, "", mkToolCallBody("muninn_update_tags", map[string]any{
		"id":   testEngramID,
		"tags": []any{"keep-me", 42, "", strings.Repeat("z", 200)},
	}))

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := extractInnerJSON(t, resp)

	if eng.calls != 1 {
		t.Fatalf("engine UpdateTags called %d times, want 1", eng.calls)
	}
	if len(eng.gotTags) != 1 || eng.gotTags[0] != "keep-me" {
		t.Errorf("engine got %v, want the one usable tag", eng.gotTags)
	}
	dropped, ok := out["dropped"].(float64)
	if !ok {
		t.Fatalf("response has no 'dropped' count on a partially-normalized call: %v", out)
	}
	if int(dropped) != 3 {
		t.Errorf("dropped = %d, want 3", int(dropped))
	}
	detail, ok := out["dropped_detail"].([]any)
	if !ok || len(detail) != 3 {
		t.Fatalf("dropped_detail = %v, want three per-entry reasons", out["dropped_detail"])
	}
	joined := ""
	for _, d := range detail {
		s, _ := d.(string)
		joined += s + "\n"
	}
	for _, want := range []string{"not a string", "empty string", "over the 128-byte limit"} {
		if !strings.Contains(joined, want) {
			t.Errorf("dropped_detail is missing the %q reason:\n%s", want, joined)
		}
	}
}

// TestUpdateTags_CleanCallReportsNoDropped is the negative control: the
// dropped fields must be ABSENT when nothing was dropped, so their presence
// stays a reliable signal rather than noise on every response.
func TestUpdateTags_CleanCallReportsNoDropped(t *testing.T) {
	eng := &captureTagsEngine{}
	srv := New(":0", eng, "", nil, nil, nil)
	w := doAuthenticatedPost(srv, "", mkToolCallBody("muninn_update_tags", map[string]any{
		"id":   testEngramID,
		"tags": []any{"alpha", "beta"},
	}))

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := extractInnerJSON(t, resp)
	if _, present := out["dropped"]; present {
		t.Errorf("clean call reported 'dropped' = %v, want the field absent", out["dropped"])
	}
	if _, present := out["dropped_detail"]; present {
		t.Errorf("clean call reported 'dropped_detail', want the field absent")
	}
}

// TestUpdateTags_EmptyArrayStillClears re-pins the deliberate-clear path
// against the new guard: the rejection is keyed on "the caller SENT entries and
// none survived", not on "the resulting set is empty". An empty array must keep
// working, or the guard has broken the documented way to remove all tags.
func TestUpdateTags_EmptyArrayStillClears(t *testing.T) {
	eng := &captureTagsEngine{}
	srv := New(":0", eng, "", nil, nil, nil)
	w := doAuthenticatedPost(srv, "", mkToolCallBody("muninn_update_tags", map[string]any{
		"id":   testEngramID,
		"tags": []any{},
	}))

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("an explicit empty array must still clear, got error: %v", resp.Error.Message)
	}
	if eng.calls != 1 || len(eng.gotTags) != 0 {
		t.Errorf("engine calls=%d tags=%v, want one call with an empty set", eng.calls, eng.gotTags)
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
