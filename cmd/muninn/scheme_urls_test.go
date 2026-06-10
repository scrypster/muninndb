package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withSidecarScheme points MUNINNDB_DATA at a temp dir and writes a muninn.addrs
// with the given scheme (empty scheme = no file written, i.e. absent sidecar).
func withSidecarScheme(t *testing.T, scheme string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MUNINNDB_DATA", dir)
	if scheme != "" {
		if err := writeAddrsFile(dir, daemonAddrs{Scheme: scheme, RestAddr: "127.0.0.1:8475", UIAddr: "127.0.0.1:8476"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalScheme(t *testing.T) {
	t.Run("https sidecar", func(t *testing.T) {
		withSidecarScheme(t, "https")
		if got := localScheme(); got != "https" {
			t.Errorf("got %q, want https", got)
		}
	})
	t.Run("http sidecar", func(t *testing.T) {
		withSidecarScheme(t, "http")
		if got := localScheme(); got != "http" {
			t.Errorf("got %q, want http", got)
		}
	})
	t.Run("absent sidecar defaults to http", func(t *testing.T) {
		withSidecarScheme(t, "")
		if got := localScheme(); got != "http" {
			t.Errorf("got %q, want http", got)
		}
	})
}

func TestDefaultMCPProxyURL(t *testing.T) {
	withSidecarScheme(t, "https")
	if got := defaultMCPProxyURL(); got != "https://127.0.0.1:"+defaultMCPPort+"/mcp" {
		t.Errorf("got %q", got)
	}
	withSidecarScheme(t, "")
	if got := defaultMCPProxyURL(); got != "http://127.0.0.1:"+defaultMCPPort+"/mcp" {
		t.Errorf("got %q", got)
	}
}

func TestClusterAddrDefault(t *testing.T) {
	withSidecarScheme(t, "https")
	if got := clusterAddrDefault(); got != "https://127.0.0.1:"+defaultRESTPort {
		t.Errorf("got %q", got)
	}
	withSidecarScheme(t, "")
	if got := clusterAddrDefault(); got != "http://127.0.0.1:"+defaultRESTPort {
		t.Errorf("got %q", got)
	}
}

func TestHTTPClientForURL(t *testing.T) {
	skipsVerify := func(c *http.Client) bool {
		tr, ok := c.Transport.(*http.Transport)
		return ok && tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify
	}

	t.Run("loopback https skips verification", func(t *testing.T) {
		c := httpClientForURL("https://127.0.0.1:8750/mcp", 5*time.Second)
		if !skipsVerify(c) {
			t.Error("loopback https must skip verification")
		}
		if c.Timeout != 5*time.Second {
			t.Errorf("timeout = %v, want 5s", c.Timeout)
		}
	})
	t.Run("remote https keeps verification", func(t *testing.T) {
		c := httpClientForURL("https://remote.example:8750/mcp", 5*time.Second)
		if skipsVerify(c) {
			t.Error("remote https must NOT skip verification")
		}
	})
	t.Run("loopback http: no custom transport", func(t *testing.T) {
		c := httpClientForURL("http://127.0.0.1:8750/mcp", 5*time.Second)
		if skipsVerify(c) {
			t.Error("http must not set an insecure transport")
		}
	})
	t.Run("insecure clients share one transport", func(t *testing.T) {
		a := httpClientForURL("https://127.0.0.1:8750/mcp", 5*time.Second)
		b := httpClientForURL("https://127.0.0.1:8475/api", 10*time.Second)
		if a.Transport != b.Transport {
			t.Error("loopback https clients must reuse the shared transport (connection pooling)")
		}
	})
}

// TestHTTPClientForURL_RefusesOffLoopbackRedirect proves the insecure-for-loopback
// client cannot be steered off-host: a redirect to a non-loopback target errors
// instead of being followed with verification still skipped.
func TestHTTPClientForURL_RefusesOffLoopbackRedirect(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://muninn.example.com/api/auth/login", http.StatusTemporaryRedirect)
	}))
	defer ts.Close()

	c := httpClientForURL(ts.URL, 5*time.Second)
	resp, err := c.Post(ts.URL+"/api/auth/login", "application/json", strings.NewReader(`{"password":"x"}`))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("redirect to non-loopback must fail, got status %d", resp.StatusCode)
	}
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("error should come from the redirect guard, got: %v", err)
	}

	// A loopback-to-loopback redirect keeps working.
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	hop := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer hop.Close()
	resp, err = httpClientForURL(hop.URL, 5*time.Second).Get(hop.URL)
	if err != nil {
		t.Fatalf("loopback-to-loopback redirect must succeed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestHTTPClientForURL_RedirectLoopBounded: a self-redirecting loopback server
// must stop at the default hop cap, not loop forever — a custom CheckRedirect
// replaces net/http's default 10-redirect limit, so the guard must re-impose it
// (the timeout-0 clients in the REPL/vault paths have no other bound).
func TestHTTPClientForURL_RedirectLoopBounded(t *testing.T) {
	var hits int32
	var ts *httptest.Server
	ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, ts.URL+"/again", http.StatusTemporaryRedirect)
	}))
	defer ts.Close()

	// Timeout 0 (no deadline): only the hop cap can stop this.
	resp, err := httpClientForURL(ts.URL, 0).Get(ts.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("a redirect loop must error, not return a response")
	}
	if !strings.Contains(err.Error(), "stopped after") {
		t.Errorf("expected the hop-cap error, got: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got > maxRedirects+1 {
		t.Errorf("server hit %d times, want <= %d (hop cap not enforced)", got, maxRedirects+1)
	}
}

// TestProbeOnce_VerifiesWhenSecure pins the off-host gating change: a self-signed
// TLS server probed with insecure=false (the off-host case) must read as down —
// i.e. verification actually happens. This fails on the pre-commit code, where
// probeHealth probed every https URL insecurely.
func TestProbeOnce_VerifiesWhenSecure(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	ok, err := probeOnce(ts.URL, false)
	if ok {
		t.Error("a self-signed server must read as down when verification is on")
	}
	// The failure must be classifiable as a cert problem (not a generic outage),
	// so the status display can say "untrusted cert" instead of "Run: muninn restart".
	if !isCertVerificationError(err) {
		t.Errorf("expected a cert-verification error, got: %v", err)
	}
	if ok, _ := probeOnce(ts.URL, true); !ok {
		t.Error("the same server must read as up when verification is skipped")
	}
}

func TestIsCertVerificationError(t *testing.T) {
	// A connection-refused / timeout error is NOT a cert problem.
	_, err := probeOnce("https://127.0.0.1:1/health", false) // nothing listens on port 1
	if isCertVerificationError(err) {
		t.Errorf("a connection error must not be classified as a cert error: %v", err)
	}
	if isCertVerificationError(nil) {
		t.Error("nil must not be a cert error")
	}
}

// TestProbeHealthLoopbackSelfSigned: a loopback https daemon with a self-signed
// cert must still read as up (the probe skips verification on loopback only).
func TestProbeHealthLoopbackSelfSigned(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	up, scheme, _ := probeHealth(ts.URL)
	if !up || scheme != "https" {
		t.Errorf("probeHealth(%q) = (%v, %q), want (true, https)", ts.URL, up, scheme)
	}
}
