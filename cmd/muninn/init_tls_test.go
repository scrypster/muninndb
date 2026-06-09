package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCertKey generates a self-signed cert + matching key, writes them as PEM,
// and returns their paths. The pair loads under tls.LoadX509KeyPair.
func writeCertKey(t *testing.T) (certPath, keyPath string) {
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

func TestClientMCPURL(t *testing.T) {
	t.Run("default is http localhost", func(t *testing.T) {
		t.Setenv("MUNINN_MCP_URL", "")
		t.Setenv("MUNINN_TLS_CERT", "")
		t.Setenv("MUNINN_TLS_KEY", "")
		if got := clientMCPURL(); got != "http://127.0.0.1:8750/mcp" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("https when both TLS vars set", func(t *testing.T) {
		t.Setenv("MUNINN_MCP_URL", "")
		t.Setenv("MUNINN_TLS_CERT", "/c.pem")
		t.Setenv("MUNINN_TLS_KEY", "/k.pem")
		if got := clientMCPURL(); got != "https://127.0.0.1:8750/mcp" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("stays http when only cert set", func(t *testing.T) {
		t.Setenv("MUNINN_MCP_URL", "")
		t.Setenv("MUNINN_TLS_CERT", "/c.pem")
		t.Setenv("MUNINN_TLS_KEY", "")
		if got := clientMCPURL(); got != "http://127.0.0.1:8750/mcp" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("MUNINN_MCP_URL override wins and is trimmed", func(t *testing.T) {
		t.Setenv("MUNINN_TLS_CERT", "/c.pem")
		t.Setenv("MUNINN_TLS_KEY", "/k.pem")
		t.Setenv("MUNINN_MCP_URL", "https://muninn.example:8750/mcp/")
		if got := clientMCPURL(); got != "https://muninn.example:8750/mcp" {
			t.Errorf("got %q", got)
		}
	})
}

func TestClientUIURL(t *testing.T) {
	t.Setenv("MUNINN_TLS_CERT", "")
	t.Setenv("MUNINN_TLS_KEY", "")
	if got := clientUIURL(); got != "http://127.0.0.1:8476" {
		t.Errorf("got %q", got)
	}
	t.Setenv("MUNINN_TLS_CERT", "/c.pem")
	t.Setenv("MUNINN_TLS_KEY", "/k.pem")
	if got := clientUIURL(); got != "https://127.0.0.1:8476" {
		t.Errorf("got %q", got)
	}
}

func TestValidateTLSPair(t *testing.T) {
	cert, key := writeCertKey(t)

	if err := validateTLSPair("", ""); err != nil {
		t.Errorf("neither set should be valid (no TLS), got %v", err)
	}
	if err := validateTLSPair(cert, ""); err == nil {
		t.Error("cert without key must error")
	}
	if err := validateTLSPair("", key); err == nil {
		t.Error("key without cert must error")
	}
	if err := validateTLSPair(cert, key); err != nil {
		t.Errorf("valid pair must pass, got %v", err)
	}
	if err := validateTLSPair("/nope/cert.pem", "/nope/key.pem"); err == nil {
		t.Error("nonexistent pair must error")
	}
}

func TestUpsertEnvFileVar(t *testing.T) {
	t.Run("creates file when absent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "muninn.env")
		if err := upsertEnvFileVar(path, "MUNINN_TLS_CERT", "/c.pem"); err != nil {
			t.Fatal(err)
		}
		assertEnvLine(t, path, "MUNINN_TLS_CERT=/c.pem")
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0600 {
			t.Errorf("perm = %v, want 0600", fi.Mode().Perm())
		}
	})

	t.Run("activates a commented template line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "muninn.env")
		os.WriteFile(path, []byte("# ── TLS ──\n# MUNINN_TLS_CERT=/path/to/cert.pem\n# MUNINN_TLS_KEY=/path/to/key.pem\n"), 0600)
		if err := upsertEnvFileVar(path, "MUNINN_TLS_CERT", "/real/cert.pem"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		s := string(b)
		assertEnvLine(t, path, "MUNINN_TLS_CERT=/real/cert.pem")
		if strings.Contains(s, "# MUNINN_TLS_CERT=/path/to/cert.pem") {
			t.Error("old commented cert line should be gone")
		}
		if !strings.Contains(s, "# MUNINN_TLS_KEY=/path/to/key.pem") {
			t.Error("unrelated commented key line must be preserved")
		}
	})

	t.Run("updates an existing active line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "muninn.env")
		os.WriteFile(path, []byte("MUNINN_TLS_CERT=/old.pem\nMUNINN_OTHER=keep\n"), 0600)
		if err := upsertEnvFileVar(path, "MUNINN_TLS_CERT", "/new.pem"); err != nil {
			t.Fatal(err)
		}
		assertEnvLine(t, path, "MUNINN_TLS_CERT=/new.pem")
		b, _ := os.ReadFile(path)
		if strings.Count(string(b), "MUNINN_TLS_CERT=") != 1 {
			t.Errorf("expected exactly one cert line, got:\n%s", b)
		}
		if !strings.Contains(string(b), "MUNINN_OTHER=keep") {
			t.Error("unrelated line must be preserved")
		}
	})

	t.Run("recognizes a commented export line, no duplicate", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "muninn.env")
		os.WriteFile(path, []byte("# export MUNINN_TLS_CERT=/old.pem\n"), 0600)
		if err := upsertEnvFileVar(path, "MUNINN_TLS_CERT", "/new.pem"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		if strings.Count(string(b), "MUNINN_TLS_CERT") != 1 {
			t.Errorf("export line should be replaced, not duplicated:\n%s", b)
		}
		assertEnvLine(t, path, "MUNINN_TLS_CERT=/new.pem")
	})

	t.Run("prefix-collision: longer key untouched", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "muninn.env")
		os.WriteFile(path, []byte("MUNINN_TLS_CERTIFICATE=keepme\n"), 0600)
		if err := upsertEnvFileVar(path, "MUNINN_TLS_CERT", "/c.pem"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		if !strings.Contains(string(b), "MUNINN_TLS_CERTIFICATE=keepme") {
			t.Error("longer-named key must not be clobbered")
		}
		assertEnvLine(t, path, "MUNINN_TLS_CERT=/c.pem")
	})
}

func assertEnvLine(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line == want {
			return
		}
	}
	t.Errorf("missing active line %q in:\n%s", want, b)
}
