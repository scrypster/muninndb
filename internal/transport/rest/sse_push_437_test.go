package rest

// sse_push_437_test.go covers the fixes for issue #437: SSE push events not
// delivered to SDK clients.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHandleSubscribe_PushOnWriteParamNames asserts that both the original
// server param ("on_write") and the param the SDKs actually send
// ("push_on_write") enable PushOnWrite on the created subscription. The root
// cause of #437 was that the server only read "on_write" while every SDK sent
// "push_on_write", so PushOnWrite was always false and pushes never fired.
func TestHandleSubscribe_PushOnWriteParamNames(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"on_write=true", "/api/subscribe?vault=default&on_write=true", true},
		{"on_write=1", "/api/subscribe?vault=default&on_write=1", true},
		{"push_on_write=true", "/api/subscribe?vault=default&push_on_write=true", true},
		{"push_on_write=1", "/api/subscribe?vault=default&push_on_write=1", true},
		{"neither set", "/api/subscribe?vault=default", false},
		{"push_on_write=false", "/api/subscribe?vault=default&push_on_write=false", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &MockEngine{}
			server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

			ctx, cancel := context.WithCancel(context.Background())
			req := httptest.NewRequest("GET", tc.query, nil)
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()

			done := make(chan struct{})
			go func() {
				defer close(done)
				server.handleSubscribe(w, req)
			}()
			cancel()
			<-done

			if eng.lastSubscribeReq == nil {
				t.Fatalf("expected SubscribeWithDeliver to be called")
			}
			if got := eng.lastSubscribeReq.PushOnWrite; got != tc.want {
				t.Errorf("PushOnWrite = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStatusRecorderUnwrap asserts that statusRecorder exposes the underlying
// http.ResponseWriter via Unwrap so http.NewResponseController can reach the
// connection to clear the write deadline. Without this, SSE streams on the REST
// port were killed after the 15s WriteTimeout (#437).
func TestStatusRecorderUnwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner}

	// statusRecorder must satisfy the rwUnwrapper interface that
	// http.NewResponseController relies on.
	var unwrapper interface {
		Unwrap() http.ResponseWriter
	} = rec
	if got := unwrapper.Unwrap(); got != http.ResponseWriter(inner) {
		t.Fatalf("Unwrap() returned %v, want the wrapped ResponseWriter", got)
	}

	// http.NewResponseController.SetWriteDeadline should reach through the
	// recorder. httptest.ResponseRecorder does not implement deadline control,
	// so we expect http.ErrNotSupported — crucially NOT a "not implemented by
	// the wrapping middleware" failure, which is what happened before Unwrap.
	rc := http.NewResponseController(rec)
	err := rc.SetWriteDeadline(time.Time{})
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("unexpected error from SetWriteDeadline: %v", err)
	}
}
