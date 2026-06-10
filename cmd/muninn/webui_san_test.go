package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSANCert generates a self-signed cert with the given DNS/IP SANs, writes
// cert.pem + key.pem to a temp dir, and returns their paths. certRoutableHost
// takes file paths, so the fixtures must be real files.
func writeSANCert(t *testing.T, dnsNames []string, ips []net.IP) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshalkey: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestCertRoutableHost(t *testing.T) {
	t.Run("routable DNS SAN is returned", func(t *testing.T) {
		c, k := writeSANCert(t, []string{"muninn.example"}, nil)
		if got := certRoutableHost(c, k); got != "muninn.example" {
			t.Errorf("got %q, want muninn.example", got)
		}
	})
	t.Run("localhost is skipped, next routable SAN wins", func(t *testing.T) {
		c, k := writeSANCert(t, []string{"localhost", "muninn.example"}, nil)
		if got := certRoutableHost(c, k); got != "muninn.example" {
			t.Errorf("got %q, want muninn.example", got)
		}
	})
	t.Run("wildcard SAN is skipped", func(t *testing.T) {
		c, k := writeSANCert(t, []string{"*.example.com"}, nil)
		if got := certRoutableHost(c, k); got != "" {
			t.Errorf("wildcard must be skipped, got %q", got)
		}
	})
	t.Run("wildcard skipped, next routable SAN wins", func(t *testing.T) {
		c, k := writeSANCert(t, []string{"*.example.com", "muninn.example"}, nil)
		if got := certRoutableHost(c, k); got != "muninn.example" {
			t.Errorf("got %q, want muninn.example", got)
		}
	})
	t.Run("only localhost yields empty", func(t *testing.T) {
		c, k := writeSANCert(t, []string{"localhost"}, nil)
		if got := certRoutableHost(c, k); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("IP-only cert (no DNS SAN) yields empty", func(t *testing.T) {
		c, k := writeSANCert(t, nil, []net.IP{net.ParseIP("203.0.113.10")})
		if got := certRoutableHost(c, k); got != "" {
			t.Errorf("got %q, want empty (IP SANs are not DNS names)", got)
		}
	})
	t.Run("empty paths yield empty", func(t *testing.T) {
		if got := certRoutableHost("", ""); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("one of pair yields empty", func(t *testing.T) {
		c, _ := writeSANCert(t, []string{"muninn.example"}, nil)
		if got := certRoutableHost(c, ""); got != "" {
			t.Errorf("got %q, want empty for cert without key", got)
		}
	})
	t.Run("nonexistent paths yield empty", func(t *testing.T) {
		if got := certRoutableHost("/nope/cert.pem", "/nope/key.pem"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestWriteReadAddrsFile_CertHost(t *testing.T) {
	dir := t.TempDir()
	want := daemonAddrs{Scheme: "https", RestAddr: "127.0.0.1:8475", UIAddr: "0.0.0.0:8476", CertHost: "muninn.example"}
	if err := writeAddrsFile(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := readAddrsFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round-trip: got %+v, want %+v", got, want)
	}
	// An old-format sidecar (no cert_host key) reads with CertHost empty.
	old := `{"scheme":"https","rest_addr":"127.0.0.1:8475","ui_addr":"0.0.0.0:8476"}`
	if err := os.WriteFile(filepath.Join(dir, addrsFileName), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	got, _ = readAddrsFile(dir)
	if got.CertHost != "" {
		t.Errorf("old sidecar CertHost = %q, want empty", got.CertHost)
	}
}

func TestWebUIDisplay_CertHost(t *testing.T) {
	t.Run("https routable bind uses the cert SAN", func(t *testing.T) {
		t.Setenv("MUNINNDB_UI_URL", "")
		lines := webUIDisplay(daemonAddrs{Scheme: "https", UIAddr: "0.0.0.0:8476", CertHost: "muninn.example"})
		if lines[0] != "https://muninn.example:8476" {
			t.Errorf("got %q, want https://muninn.example:8476", lines[0])
		}
		if lines[len(lines)-1] != "https://127.0.0.1:8476" {
			t.Errorf("last line should be the localhost fallback, got %q", lines[len(lines)-1])
		}
	})
	t.Run("no CertHost falls back to os.Hostname (hostname-agnostic)", func(t *testing.T) {
		t.Setenv("MUNINNDB_UI_URL", "")
		lines := webUIDisplay(daemonAddrs{Scheme: "https", UIAddr: "0.0.0.0:8476"})
		if !strings.HasPrefix(lines[0], "https://") {
			t.Errorf("routable line should be https, got %q", lines[0])
		}
		if strings.Contains(lines[0], "127.0.0.1") {
			t.Errorf("routable line should be a hostname, not loopback: %q", lines[0])
		}
	})
	t.Run("http ignores CertHost", func(t *testing.T) {
		t.Setenv("MUNINNDB_UI_URL", "")
		lines := webUIDisplay(daemonAddrs{Scheme: "http", UIAddr: "0.0.0.0:8476", CertHost: "muninn.example"})
		if strings.Contains(lines[0], "muninn.example") {
			t.Errorf("http path must not use CertHost, got %q", lines[0])
		}
	})
	t.Run("loopback bind ignores CertHost", func(t *testing.T) {
		t.Setenv("MUNINNDB_UI_URL", "")
		lines := webUIDisplay(daemonAddrs{Scheme: "https", UIAddr: "127.0.0.1:8476", CertHost: "muninn.example"})
		if len(lines) != 1 || lines[0] != "https://127.0.0.1:8476" {
			t.Errorf("loopback bind must not advertise the cert SAN, got %v", lines)
		}
	})
	t.Run("MUNINNDB_UI_URL wins over CertHost", func(t *testing.T) {
		t.Setenv("MUNINNDB_UI_URL", "https://override.lan:8476")
		lines := webUIDisplay(daemonAddrs{Scheme: "https", UIAddr: "0.0.0.0:8476", CertHost: "muninn.example"})
		if len(lines) != 1 || lines[0] != "https://override.lan:8476" {
			t.Errorf("env override must win over CertHost, got %v", lines)
		}
	})
}
