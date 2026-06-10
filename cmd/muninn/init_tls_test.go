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
	"runtime"
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

// writeLoopbackCertCombined writes a single PEM file with the private KEY block
// FIRST and a cert (IP SAN 127.0.0.1) second — the order a naive first-block
// reader would choke on. Returns the combined path.
func writeLoopbackCertCombined(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	combined := append(
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...,
	)
	path := filepath.Join(t.TempDir(), "combined.pem")
	if err := os.WriteFile(path, combined, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWarnIfCertNotLoopback(t *testing.T) {
	t.Run("key-first combined PEM with loopback SAN is silent", func(t *testing.T) {
		path := writeLoopbackCertCombined(t)
		if out := captureStdout(func() { warnIfCertNotLoopback(path) }); out != "" {
			t.Errorf("a 127.0.0.1 cert must not warn even key-first, got:\n%s", out)
		}
	})
	t.Run("non-loopback cert warns", func(t *testing.T) {
		cert, _ := writeCertKey(t) // CN=test, no loopback SAN
		out := captureStdout(func() { warnIfCertNotLoopback(cert) })
		if !strings.Contains(out, "covers neither 127.0.0.1 nor localhost") {
			t.Errorf("expected a non-loopback warning, got:\n%s", out)
		}
	})
}

func TestWarnIfDaemonSchemeMismatch(t *testing.T) {
	setup := func(t *testing.T, daemonScheme string) {
		dir := t.TempDir()
		t.Setenv("MUNINNDB_DATA", dir)
		if err := writeAddrsFile(dir, daemonAddrs{Scheme: daemonScheme, RestAddr: "127.0.0.1:8475", UIAddr: "127.0.0.1:8476"}); err != nil {
			t.Fatal(err)
		}
		if err := writePID(filepath.Join(dir, "muninn.pid"), os.Getpid()); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("warns when TLS enabled but daemon serves http", func(t *testing.T) {
		setup(t, "http")
		t.Setenv("MUNINN_TLS_CERT", "/c.pem")
		t.Setenv("MUNINN_TLS_KEY", "/k.pem")
		out := captureStdout(warnIfDaemonSchemeMismatch)
		if !strings.Contains(out, "Restart it to serve https") {
			t.Errorf("expected a restart warning, got:\n%s", out)
		}
	})
	t.Run("silent in the adopt direction (daemon https, no TLS env)", func(t *testing.T) {
		setup(t, "https")
		t.Setenv("MUNINN_TLS_CERT", "")
		t.Setenv("MUNINN_TLS_KEY", "")
		if out := captureStdout(warnIfDaemonSchemeMismatch); out != "" {
			t.Errorf("must not advise a restart that breaks the https configs init wrote, got:\n%s", out)
		}
	})
}

func TestClientMCPURL(t *testing.T) {
	// Pin the data dir: clientScheme consults a RUNNING daemon's muninn.addrs,
	// and the host running these tests may have a live (possibly TLS) daemon.
	t.Setenv("MUNINNDB_DATA", t.TempDir())
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
	t.Setenv("MUNINNDB_DATA", t.TempDir())
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

func TestClientScheme(t *testing.T) {
	noTLSEnv := func(t *testing.T) {
		t.Setenv("MUNINN_TLS_CERT", "")
		t.Setenv("MUNINN_TLS_KEY", "")
	}

	t.Run("default is http", func(t *testing.T) {
		t.Setenv("MUNINNDB_DATA", t.TempDir())
		noTLSEnv(t)
		if got := clientScheme(); got != "http" {
			t.Errorf("got %q, want http", got)
		}
	})
	t.Run("TLS env wins", func(t *testing.T) {
		t.Setenv("MUNINNDB_DATA", t.TempDir())
		t.Setenv("MUNINN_TLS_CERT", "/c.pem")
		t.Setenv("MUNINN_TLS_KEY", "/k.pem")
		if got := clientScheme(); got != "https" {
			t.Errorf("got %q, want https", got)
		}
	})
	t.Run("running daemon's sidecar scheme is adopted", func(t *testing.T) {
		// Re-running init on a TLS deployment without flags or env must not
		// downgrade generated configs to http (#443 review finding).
		dir := t.TempDir()
		t.Setenv("MUNINNDB_DATA", dir)
		noTLSEnv(t)
		if err := writeAddrsFile(dir, daemonAddrs{Scheme: "https", RestAddr: "127.0.0.1:8475", UIAddr: "127.0.0.1:8476"}); err != nil {
			t.Fatal(err)
		}
		// Our own PID is alive — the sidecar counts as a running daemon's.
		if err := writePID(filepath.Join(dir, "muninn.pid"), os.Getpid()); err != nil {
			t.Fatal(err)
		}
		if got := clientScheme(); got != "https" {
			t.Errorf("got %q, want https from the running daemon's sidecar", got)
		}
	})
	t.Run("stale sidecar without a daemon is ignored", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("MUNINNDB_DATA", dir)
		noTLSEnv(t)
		if err := writeAddrsFile(dir, daemonAddrs{Scheme: "https"}); err != nil {
			t.Fatal(err)
		}
		// No PID file: a crashed daemon's leftover sidecar must not make a
		// fresh plaintext deployment advertise https.
		if got := clientScheme(); got != "http" {
			t.Errorf("got %q, want http (stale sidecar must be ignored)", got)
		}
	})
}

func TestNormalizeTLSPath(t *testing.T) {
	if got := normalizeTLSPath(""); got != "" {
		t.Errorf("empty must stay empty, got %q", got)
	}
	// Use a path that is absolute on the host OS (t.TempDir is absolute
	// everywhere); a Unix-style "/abs/..." literal is NOT absolute on Windows,
	// where filepath.Abs would prepend a drive letter.
	abs := filepath.Join(t.TempDir(), "cert.pem")
	if got := normalizeTLSPath(abs); got != abs {
		t.Errorf("absolute must be unchanged, got %q want %q", got, abs)
	}
	got := normalizeTLSPath("cert.pem")
	if !filepath.IsAbs(got) || filepath.Base(got) != "cert.pem" {
		t.Errorf("relative must become absolute in cwd, got %q", got)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := normalizeTLSPath("~/certs/c.pem"); got != filepath.Join(home, "certs", "c.pem") {
		t.Errorf("~/ must expand to home, got %q", got)
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
		// Windows doesn't honor Unix permission bits — os.Chmod(0600) surfaces as
		// 0666 there — so only assert the mode on platforms that have them.
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0600 {
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

// loadedEnvValue round-trips path through loadEnvFileFrom — the actual
// consumer — and returns the value the daemon would see for key. The key is
// guaranteed unset before the load and unset again afterwards.
func loadedEnvValue(t *testing.T, path, key string) string {
	t.Helper()
	// loadEnvFileFrom mutates the global process env (set-if-unset) and a fixture
	// may carry several keys — so snapshot every MUNINN* var and restore it on
	// cleanup, unsetting any the load newly created. Without this the helper
	// leaks (e.g. half a TLS pair) into later tests, including the integration
	// tests that exec `muninn start` from this same process and would then see a
	// daemon abort on a lone MUNINN_TLS_KEY.
	snapshotMuninnEnv(t)
	os.Unsetenv(key) // so set-if-unset actually applies the file's value
	loadEnvFileFrom(path)
	return os.Getenv(key)
}

// snapshotMuninnEnv records every MUNINN* env var now and registers a cleanup
// that restores them exactly — re-setting changed ones and unsetting any the
// test newly created.
func snapshotMuninnEnv(t *testing.T) {
	t.Helper()
	saved := map[string]string{}
	for _, kv := range os.Environ() {
		if eq := strings.IndexByte(kv, '='); eq > 0 && strings.HasPrefix(kv, "MUNINN") {
			saved[kv[:eq]] = kv[eq+1:]
		}
	}
	t.Cleanup(func() {
		for _, kv := range os.Environ() {
			if eq := strings.IndexByte(kv, '='); eq > 0 && strings.HasPrefix(kv, "MUNINN") {
				if _, ok := saved[kv[:eq]]; !ok {
					os.Unsetenv(kv[:eq])
				}
			}
		}
		for k, v := range saved {
			os.Setenv(k, v)
		}
	})
}

// TestUpsertEnvFileVars_LoaderRoundTrip pins the matcher to loadEnvFile's
// parsing: every form the loader accepts as an assignment must be replaced,
// not duplicated — a missed form would leave a stale first-active-line that
// shadows the new value after restart.
func TestUpsertEnvFileVars_LoaderRoundTrip(t *testing.T) {
	t.Run("spaced assignment is replaced, not shadowed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "muninn.env")
		os.WriteFile(path, []byte("MUNINN_TLS_CERT = /old.pem\n"), 0600)
		if err := upsertEnvFileVar(path, "MUNINN_TLS_CERT", "/new.pem"); err != nil {
			t.Fatal(err)
		}
		if got := loadedEnvValue(t, path, "MUNINN_TLS_CERT"); got != "/new.pem" {
			t.Errorf("daemon would load %q, want /new.pem", got)
		}
		b, _ := os.ReadFile(path)
		if strings.Count(string(b), "MUNINN_TLS_CERT") != 1 {
			t.Errorf("spaced assignment must be replaced, not duplicated:\n%s", b)
		}
	})

	t.Run("later active duplicate is neutralized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "muninn.env")
		os.WriteFile(path, []byte("# MUNINN_TLS_CERT=/path/to/cert.pem\nMUNINN_TLS_CERT=/user.pem\n"), 0600)
		if err := upsertEnvFileVar(path, "MUNINN_TLS_CERT", "/new.pem"); err != nil {
			t.Fatal(err)
		}
		if got := loadedEnvValue(t, path, "MUNINN_TLS_CERT"); got != "/new.pem" {
			t.Errorf("daemon would load %q, want /new.pem", got)
		}
		b, _ := os.ReadFile(path)
		s := string(b)
		assertEnvLine(t, path, "MUNINN_TLS_CERT=/new.pem")
		if !strings.Contains(s, "# superseded by muninn init: MUNINN_TLS_CERT=/user.pem") {
			t.Errorf("stale active line must be preserved as a comment:\n%s", s)
		}
		active := 0
		for _, line := range strings.Split(s, "\n") {
			if activeEnvLineMatchesKey(line, "MUNINN_TLS_CERT") {
				active++
			}
		}
		if active != 1 {
			t.Errorf("exactly one active assignment expected, got %d:\n%s", active, s)
		}
	})

	t.Run("CRLF endings are preserved", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "muninn.env")
		os.WriteFile(path, []byte("# MUNINN_TLS_CERT=/x.pem\r\nMUNINN_TLS_KEY=/keep.pem\r\n"), 0600)
		if err := upsertEnvFileVar(path, "MUNINN_TLS_CERT", "/new.pem"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		if !strings.Contains(string(b), "MUNINN_TLS_CERT=/new.pem\r\n") {
			t.Errorf("replaced line must keep its CRLF ending:\n%q", b)
		}
		if got := loadedEnvValue(t, path, "MUNINN_TLS_CERT"); got != "/new.pem" {
			t.Errorf("daemon would load %q, want /new.pem", got)
		}
	})
}

// TestLoadedEnvValueNoLeak guards the test helper itself: a multi-key fixture
// must not leak a stray var into the shared process env after the subtest, or
// it would poison the integration tests that exec `muninn start` (a lone
// MUNINN_TLS_KEY makes the daemon abort).
func TestLoadedEnvValueNoLeak(t *testing.T) {
	if _, ok := os.LookupEnv("MUNINN_TLS_KEY"); ok {
		t.Skip("MUNINN_TLS_KEY already set in this environment")
	}
	t.Run("round-trip", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "muninn.env")
		os.WriteFile(path, []byte("MUNINN_TLS_CERT=/c.pem\nMUNINN_TLS_KEY=/k.pem\n"), 0600)
		_ = loadedEnvValue(t, path, "MUNINN_TLS_CERT")
	})
	// The subtest's cleanups have run by now.
	if v, ok := os.LookupEnv("MUNINN_TLS_KEY"); ok {
		t.Errorf("loadedEnvValue leaked MUNINN_TLS_KEY=%q into the process env", v)
	}
	if v, ok := os.LookupEnv("MUNINN_TLS_CERT"); ok {
		t.Errorf("loadedEnvValue leaked MUNINN_TLS_CERT=%q into the process env", v)
	}
}

func TestUpsertEnvFileVars_PairAndGuards(t *testing.T) {
	t.Run("pair lands in one write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "muninn.env")
		err := upsertEnvFileVars(path, [][2]string{
			{"MUNINN_TLS_CERT", "/c.pem"},
			{"MUNINN_TLS_KEY", "/k.pem"},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertEnvLine(t, path, "MUNINN_TLS_CERT=/c.pem")
		assertEnvLine(t, path, "MUNINN_TLS_KEY=/k.pem")
	})

	t.Run("symlinked env file is refused", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real.env")
		link := filepath.Join(dir, "muninn.env")
		os.WriteFile(real, []byte("MUNINN_TLS_KEY=/keep.pem\n"), 0600)
		if err := os.Symlink(real, link); err != nil {
			t.Skip("symlinks unavailable")
		}
		if err := upsertEnvFileVar(link, "MUNINN_TLS_CERT", "/c.pem"); err == nil {
			t.Fatal("symlinked env file must be refused (the loader rejects symlinks)")
		}
		fi, err := os.Lstat(link)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Error("the symlink must survive untouched")
		}
		b, _ := os.ReadFile(real)
		if string(b) != "MUNINN_TLS_KEY=/keep.pem\n" {
			t.Errorf("symlink target must be untouched, got:\n%s", b)
		}
	})

	t.Run("oversized env file is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "muninn.env")
		big := strings.Repeat("# filler\n", envFileMaxBytes/9+1)
		os.WriteFile(path, []byte(big), 0600)
		if err := upsertEnvFileVar(path, "MUNINN_TLS_CERT", "/c.pem"); err == nil {
			t.Fatal("oversized env file must be refused (the loader skips it entirely)")
		}
	})
}
