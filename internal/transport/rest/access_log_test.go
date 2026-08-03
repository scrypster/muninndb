package rest

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withCapturedDefaultLog swaps slog's default logger for one writing to buf
// (text handler, INFO level) for the duration of fn, then restores it.
func withCapturedDefaultLog(t *testing.T, buf *bytes.Buffer, fn func()) {
	t.Helper()
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(old)
	fn()
}

func newTestServerForAccessLog() *Server {
	return NewServer("localhost:0", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
}

// TestLoggingMiddleware_AccessLogDefaultsOn is the control: with no env
// override, the per-request "request" INFO line is emitted, matching
// today's behavior before #851.
func TestLoggingMiddleware_AccessLogDefaultsOn(t *testing.T) {
	s := newTestServerForAccessLog()
	handler := s.loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	var buf bytes.Buffer
	withCapturedDefaultLog(t, &buf, func() {
		req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
	})

	if !strings.Contains(buf.String(), "msg=request") {
		t.Errorf("expected the access log line by default, got: %s", buf.String())
	}
}

// TestLoggingMiddleware_AccessLogDisabledByEnv is #851's fix: setting
// MUNINN_ACCESS_LOG=0 silences the per-request line independently of
// --log-level — an operator no longer has to raise the global level to
// warn (and lose every other INFO line) just to stop request chatter.
func TestLoggingMiddleware_AccessLogDisabledByEnv(t *testing.T) {
	t.Setenv("MUNINN_ACCESS_LOG", "0")
	s := newTestServerForAccessLog()
	handler := s.loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	var buf bytes.Buffer
	withCapturedDefaultLog(t, &buf, func() {
		req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
	})

	if strings.Contains(buf.String(), "msg=request") {
		t.Errorf("MUNINN_ACCESS_LOG=0 must silence the access log line, got: %s", buf.String())
	}
}

// TestLoggingMiddleware_AccessLogDisabledDoesNotSuppressOtherInfo proves the
// two are decoupled: silencing the access log must not touch the global
// log level or any other INFO emission — that welded-together behavior
// (raising --log-level to warn) is exactly what #851 was filed to avoid.
func TestLoggingMiddleware_AccessLogDisabledDoesNotSuppressOtherInfo(t *testing.T) {
	t.Setenv("MUNINN_ACCESS_LOG", "0")
	s := newTestServerForAccessLog()
	handler := s.loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		slog.Info("subsystem event", "detail", "migration complete")
		w.WriteHeader(http.StatusOK)
	})

	var buf bytes.Buffer
	withCapturedDefaultLog(t, &buf, func() {
		req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
	})

	if strings.Contains(buf.String(), "msg=request") {
		t.Errorf("access log must be silenced, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "migration complete") {
		t.Errorf("subsystem INFO lines must survive MUNINN_ACCESS_LOG=0, got: %s", buf.String())
	}
}
