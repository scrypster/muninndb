package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadOnlyFromContext(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{ModeObserve, true},
		{ModeFull, false},
		{ModeWrite, false},
		{"", false},
	}

	for _, tc := range tests {
		ctx := context.WithValue(context.Background(), ContextMode, tc.mode)
		if got := ReadOnlyFromContext(ctx); got != tc.want {
			t.Fatalf("mode=%q: got %v want %v", tc.mode, got, tc.want)
		}
	}
}

func TestReadOnlyGuard_BlocksObserveMode(t *testing.T) {
	reached := false
	handler := ReadOnlyGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ContextMode, ModeObserve))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if reached {
		t.Fatal("inner handler should not be reached")
	}
}

func TestReadOnlyGuard_ErrorEnvelopeMatchesWriteOnlyGuard(t *testing.T) {
	readOnlyReq := httptest.NewRequest(http.MethodPost, "/", nil)
	readOnlyReq = readOnlyReq.WithContext(context.WithValue(readOnlyReq.Context(), ContextMode, ModeObserve))
	readOnlyResp := httptest.NewRecorder()
	ReadOnlyGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("read-only inner handler should not run")
	}))(readOnlyResp, readOnlyReq)

	writeOnlyReq := httptest.NewRequest(http.MethodGet, "/", nil)
	writeOnlyReq = writeOnlyReq.WithContext(context.WithValue(writeOnlyReq.Context(), ContextMode, ModeWrite))
	writeOnlyResp := httptest.NewRecorder()
	WriteOnlyGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("write-only inner handler should not run")
	}))(writeOnlyResp, writeOnlyReq)

	if got, want := readOnlyResp.Header().Get("Content-Type"), writeOnlyResp.Header().Get("Content-Type"); got != want {
		t.Fatalf("content-type mismatch: got %q want %q", got, want)
	}
	if got, want := readOnlyResp.Code, writeOnlyResp.Code; got != want {
		t.Fatalf("status mismatch: got %d want %d", got, want)
	}
	if got := readOnlyResp.Body.String(); got != `{"error":{"code":"FORBIDDEN","message":"read-only key cannot write"}}` {
		t.Fatalf("unexpected read-only body: %s", got)
	}
	if got := writeOnlyResp.Body.String(); got != `{"error":{"code":"FORBIDDEN","message":"write-only key cannot read"}}` {
		t.Fatalf("unexpected write-only body: %s", got)
	}
}
