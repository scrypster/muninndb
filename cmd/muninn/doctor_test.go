package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeCert builds a self-signed leaf for tests and returns both the parsed
// certificate and its PEM encoding. Mirrors internal/tlsutil/expiry_test.go.
func makeCert(t *testing.T, cn string, dns []string, notAfter time.Time) (*x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notAfter.Add(-24 * time.Hour),
		NotAfter:     notAfter,
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func svcsAllUp() []serviceStatus {
	return []serviceStatus{
		{name: "database", port: 8475, up: true, scheme: "https"},
		{name: "mcp", port: 8750, up: true, scheme: "https"},
		{name: "web ui", port: 8476, up: true, scheme: "https"},
	}
}

func TestFormatDoctor(t *testing.T) {
	addrs := daemonAddrs{Scheme: "https", RestAddr: "0.0.0.0:8475", MCPAddr: "127.0.0.1:8750", UIAddr: "127.0.0.1:8476"}

	healthy, _ := makeCert(t, "muninn.example", []string{"muninn.example", "localhost"}, time.Now().Add(300*24*time.Hour))
	expiring, _ := makeCert(t, "soon", nil, time.Now().Add(10*24*time.Hour))
	expired, _ := makeCert(t, "old", nil, time.Now().Add(-1*time.Hour))

	cases := []struct {
		name      string
		report    doctorReport
		verbose   bool
		isTTY     bool
		want      []string // substrings that must appear
		notWant   []string // substrings that must NOT appear
		wantNoESC bool     // assert no ANSI escape leaks
	}{
		{
			name:    "tls disabled, no cert section",
			report:  doctorReport{svcs: svcsAllUp(), scheme: "http", addrs: daemonAddrs{Scheme: "http", RestAddr: "127.0.0.1:8475"}},
			want:    []string{"disabled (http)", "running"},
			notWant: []string{"certificate", "expiry"},
		},
		{
			name:   "https healthy, live socket",
			report: doctorReport{svcs: svcsAllUp(), scheme: "https", addrs: addrs, cert: healthy, certSource: "live socket", tlsVersion: tls.VersionTLS13, cipher: tls.TLS_AES_128_GCM_SHA256},
			want:   []string{"enabled (https)", "CN=muninn.example", "dns sans", "days remaining", "(from live socket)", "0.0.0.0:8475"},
		},
		{
			name:   "expiring soon",
			report: doctorReport{svcs: svcsAllUp(), scheme: "https", addrs: addrs, cert: expiring, certSource: "live socket"},
			want:   []string{"expires in", "days"},
		},
		{
			name:   "expired",
			report: doctorReport{svcs: svcsAllUp(), scheme: "https", addrs: addrs, cert: expired, certSource: "cert file"},
			want:   []string{"EXPIRED", "days ago"},
		},
		{
			name:   "https but cert unobtainable",
			report: doctorReport{svcs: svcsAllUp(), scheme: "https", addrs: addrs, certErr: "could not inspect certificate (boom)"},
			want:   []string{"could not inspect certificate"},
		},
		{
			name:    "verbose adds detail",
			report:  doctorReport{svcs: svcsAllUp(), scheme: "https", addrs: addrs, cert: healthy, certSource: "cert file"},
			verbose: true,
			want:    []string{"serial", "sig alg", "n/a (cert file)"},
		},
		{
			name:      "non-tty has no ansi or unicode glyphs",
			report:    doctorReport{svcs: svcsAllUp(), scheme: "https", addrs: addrs, cert: healthy, certSource: "live socket"},
			isTTY:     false,
			want:      []string{"[up]"},
			notWant:   []string{"●", "○", "⚠"},
			wantNoESC: true,
		},
		{
			name:      "non-tty tls-disabled uses ascii off-marker, not a raw glyph",
			report:    doctorReport{svcs: svcsAllUp(), scheme: "http", addrs: daemonAddrs{Scheme: "http", RestAddr: "127.0.0.1:8475"}},
			isTTY:     false,
			want:      []string{"[off]", "disabled (http)"},
			notWant:   []string{"○"},
			wantNoESC: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := formatDoctor(tc.report, tc.verbose, tc.isTTY)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q\n--- got ---\n%s", w, out)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(out, nw) {
					t.Errorf("output unexpectedly contains %q\n--- got ---\n%s", nw, out)
				}
			}
			if tc.wantNoESC && strings.Contains(out, "\033") {
				t.Errorf("non-tty output leaked ANSI escape:\n%s", out)
			}
		})
	}
}

// TestFormatDoctor_NoCertPanic guards the formatter invariant: scheme=="https"
// with a nil cert (failed dial) must render, not panic.
func TestFormatDoctor_NoCertPanic(t *testing.T) {
	r := doctorReport{svcs: svcsAllUp(), scheme: "https", addrs: daemonAddrs{Scheme: "https"}, cert: nil, certErr: "boom"}
	_ = formatDoctor(r, true, true)
}

func TestGatherDoctor_LiveCertRewritesToLoopback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MUNINNDB_DATA", tmp)
	if err := writeAddrsFile(tmp, daemonAddrs{Scheme: "https", RestAddr: "0.0.0.0:8475", MCPAddr: "127.0.0.1:8750", UIAddr: "127.0.0.1:8476"}); err != nil {
		t.Fatal(err)
	}

	origProbe := probeServicesFn
	t.Cleanup(func() { probeServicesFn = origProbe })
	probeServicesFn = svcsAllUp

	cert, _ := makeCert(t, "live", []string{"live"}, time.Now().Add(300*24*time.Hour))
	origDial := dialServedCertFn
	t.Cleanup(func() { dialServedCertFn = origDial })
	var dialed string
	dialServedCertFn = func(hostport string) (*x509.Certificate, uint16, uint16, error) {
		dialed = hostport
		return cert, tls.VersionTLS13, tls.TLS_AES_128_GCM_SHA256, nil
	}

	r := gatherDoctor()
	if dialed != "127.0.0.1:8475" {
		t.Errorf("dial must target loopback (recorded host was 0.0.0.0), got %q", dialed)
	}
	if r.certSource != "live socket" || r.cert == nil {
		t.Errorf("expected live-socket cert, got source=%q cert=%v", r.certSource, r.cert)
	}
}

func TestGatherDoctor_FileFallbackWhenDialFails(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MUNINNDB_DATA", tmp)
	if err := writeAddrsFile(tmp, daemonAddrs{Scheme: "https", RestAddr: "127.0.0.1:8475"}); err != nil {
		t.Fatal(err)
	}
	origProbe := probeServicesFn
	t.Cleanup(func() { probeServicesFn = origProbe })
	probeServicesFn = svcsAllUp

	origDial := dialServedCertFn
	t.Cleanup(func() { dialServedCertFn = origDial })
	dialServedCertFn = func(string) (*x509.Certificate, uint16, uint16, error) {
		return nil, 0, 0, errors.New("connection refused")
	}

	cert, pemBytes := makeCert(t, "from-file", []string{"from-file"}, time.Now().Add(200*24*time.Hour))
	certPath := filepath.Join(tmp, "cert.pem")
	if err := os.WriteFile(certPath, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUNINN_TLS_CERT", certPath)

	r := gatherDoctor()
	if r.certSource != "cert file" {
		t.Fatalf("expected cert-file fallback, got source=%q certErr=%q", r.certSource, r.certErr)
	}
	if r.cert == nil || r.cert.Subject.CommonName != cert.Subject.CommonName {
		t.Errorf("wrong cert from file: %v", r.cert)
	}
	if r.tlsVersion != 0 {
		t.Errorf("cert-file path must not report a negotiated TLS version, got %d", r.tlsVersion)
	}
}

func TestGatherDoctor_CertErrWhenNothingAvailable(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MUNINNDB_DATA", tmp)
	if err := writeAddrsFile(tmp, daemonAddrs{Scheme: "https", RestAddr: "127.0.0.1:8475"}); err != nil {
		t.Fatal(err)
	}
	origProbe := probeServicesFn
	t.Cleanup(func() { probeServicesFn = origProbe })
	probeServicesFn = svcsAllUp

	origDial := dialServedCertFn
	t.Cleanup(func() { dialServedCertFn = origDial })
	dialServedCertFn = func(string) (*x509.Certificate, uint16, uint16, error) {
		return nil, 0, 0, errors.New("refused")
	}
	t.Setenv("MUNINN_TLS_CERT", "")

	r := gatherDoctor()
	if r.certErr == "" {
		t.Error("expected certErr when neither live socket nor cert file is available")
	}
}

// TestGatherDoctor_CertErrWhenEnvFileBadAndServerStopped covers the documented
// offline-inspection path: server stopped (scheme unknown) but the operator set
// MUNINN_TLS_CERT to an unreadable file — the failure must be surfaced, not
// swallowed.
func TestGatherDoctor_CertErrWhenEnvFileBadAndServerStopped(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MUNINNDB_DATA", tmp) // no addrs file → server stopped, scheme ""
	origProbe := probeServicesFn
	t.Cleanup(func() { probeServicesFn = origProbe })
	probeServicesFn = func() []serviceStatus {
		return []serviceStatus{{name: "database"}, {name: "mcp"}, {name: "web ui"}}
	}

	bad := filepath.Join(tmp, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUNINN_TLS_CERT", bad)

	r := gatherDoctor()
	if r.scheme != "" {
		t.Fatalf("expected unknown scheme for a stopped server, got %q", r.scheme)
	}
	if r.certErr == "" {
		t.Error("expected certErr when MUNINN_TLS_CERT points at an unreadable cert, even with the server stopped")
	}
}

// TestGatherDoctor_LiveDialUsesDefaultPortWhenSidecarAbsent covers the
// systemd-managed daemon with no muninn.addrs sidecar: scheme is recovered from
// the probe, RestAddr is empty, and the live dial must still target the default
// REST port on loopback.
func TestGatherDoctor_LiveDialUsesDefaultPortWhenSidecarAbsent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MUNINNDB_DATA", tmp) // no addrs file → RestAddr ""
	origProbe := probeServicesFn
	t.Cleanup(func() { probeServicesFn = origProbe })
	probeServicesFn = svcsAllUp // web-ui probe reports https → scheme recovered

	cert, _ := makeCert(t, "sys", []string{"sys"}, time.Now().Add(100*24*time.Hour))
	origDial := dialServedCertFn
	t.Cleanup(func() { dialServedCertFn = origDial })
	var dialed string
	dialServedCertFn = func(hostport string) (*x509.Certificate, uint16, uint16, error) {
		dialed = hostport
		return cert, tls.VersionTLS13, tls.TLS_AES_128_GCM_SHA256, nil
	}

	r := gatherDoctor()
	if dialed != "127.0.0.1:8475" {
		t.Errorf("expected dial to default REST port when sidecar absent, got %q", dialed)
	}
	if r.certSource != "live socket" {
		t.Errorf("expected live-socket cert via default port, got %q", r.certSource)
	}
}

func TestCertFromEnvFile_LeafFirstWithChain(t *testing.T) {
	tmp := t.TempDir()
	_, leafPEM := makeCert(t, "leaf", []string{"leaf"}, time.Now().Add(100*24*time.Hour))
	_, interPEM := makeCert(t, "intermediate", nil, time.Now().Add(3650*24*time.Hour))
	bundle := append(append([]byte{}, leafPEM...), interPEM...)
	path := filepath.Join(tmp, "fullchain.pem")
	if err := os.WriteFile(path, bundle, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUNINN_TLS_CERT", path)

	leaf, chain, err := certFromEnvFile()
	if err != nil {
		t.Fatalf("certFromEnvFile: %v", err)
	}
	if leaf.Subject.CommonName != "leaf" {
		t.Errorf("first CERTIFICATE block must be the leaf, got CN=%q", leaf.Subject.CommonName)
	}
	if len(chain) != 1 || chain[0].Subject.CommonName != "intermediate" {
		t.Errorf("expected one intermediate in chain, got %v", chain)
	}
}

// TestDialServedCert_Live exercises the real production dialer end-to-end
// against a live TLS listener.
func TestDialServedCert_Live(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	cert, ver, suite, err := dialServedCert(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dialServedCert: %v", err)
	}
	if cert == nil {
		t.Fatal("expected a served certificate")
	}
	if ver == 0 || suite == 0 {
		t.Errorf("expected negotiated version+cipher, got ver=%d suite=%d", ver, suite)
	}
}
