package tlsutil

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"
)

func newTestCert(t *testing.T, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-cert"},
		NotBefore:    notAfter.Add(-24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert
}

// recordHandler is a minimal slog.Handler that captures the highest severity
// and the rendered messages. Avoids depending on slogtest or a JSON parser.
type recordHandler struct {
	buf      *bytes.Buffer
	maxLevel slog.Level
}

func (h *recordHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level > h.maxLevel {
		h.maxLevel = r.Level
	}
	h.buf.WriteString(r.Message)
	h.buf.WriteByte('\n')
	return nil
}
func (h *recordHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(_ string) slog.Handler      { return h }

func newRecorder() (*slog.Logger, *recordHandler) {
	h := &recordHandler{buf: &bytes.Buffer{}, maxLevel: slog.LevelDebug - 1}
	return slog.New(h), h
}

func TestCheckCertExpiry_Healthy(t *testing.T) {
	cert := newTestCert(t, time.Now().Add(365*24*time.Hour))
	logger, rec := newRecorder()

	remaining := CheckCertExpiry(logger, cert, "test")

	if remaining <= 0 {
		t.Fatalf("expected positive remaining, got %v", remaining)
	}
	if rec.maxLevel >= slog.LevelWarn {
		t.Fatalf("expected silent for healthy cert, got level %v with output: %q",
			rec.maxLevel, rec.buf.String())
	}
}

func TestCheckCertExpiry_Warn(t *testing.T) {
	cert := newTestCert(t, time.Now().Add(10*24*time.Hour))
	logger, rec := newRecorder()

	remaining := CheckCertExpiry(logger, cert, "test")

	if remaining <= 0 || remaining >= ExpiryWarnWindow {
		t.Fatalf("expected 0 < remaining < ExpiryWarnWindow, got %v", remaining)
	}
	if rec.maxLevel != slog.LevelWarn {
		t.Fatalf("expected Warn level, got %v with output: %q", rec.maxLevel, rec.buf.String())
	}
	if !strings.Contains(rec.buf.String(), "expires soon") {
		t.Fatalf("expected 'expires soon' in log output, got: %q", rec.buf.String())
	}
}

func TestCheckCertExpiry_Expired(t *testing.T) {
	cert := newTestCert(t, time.Now().Add(-1*time.Hour))
	logger, rec := newRecorder()

	remaining := CheckCertExpiry(logger, cert, "test")

	if remaining > 0 {
		t.Fatalf("expected non-positive remaining for expired cert, got %v", remaining)
	}
	if rec.maxLevel != slog.LevelError {
		t.Fatalf("expected Error level, got %v with output: %q", rec.maxLevel, rec.buf.String())
	}
	if !strings.Contains(rec.buf.String(), "expired") {
		t.Fatalf("expected 'expired' in log output, got: %q", rec.buf.String())
	}
}

func TestCheckCertExpiry_NilLogger(t *testing.T) {
	// Must not panic — falls back to slog.Default(). We only assert no panic
	// and a sensible return value; output goes to the global default handler.
	cert := newTestCert(t, time.Now().Add(365*24*time.Hour))
	remaining := CheckCertExpiry(nil, cert, "test")
	if remaining <= 0 {
		t.Fatalf("expected positive remaining, got %v", remaining)
	}
}

func TestDaysRemaining(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want int
	}{
		{"zero", 0, 0},
		{"negative", -5 * time.Hour, 0},
		{"sub-day rounds up to 1", 23*time.Hour + 59*time.Minute, 1},
		{"exactly one day", 24 * time.Hour, 1},
		{"one day plus a second rounds up to 2", 24*time.Hour + time.Second, 2},
		{"ten days", 10 * 24 * time.Hour, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DaysRemaining(tc.in); got != tc.want {
				t.Errorf("DaysRemaining(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
