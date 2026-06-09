package main

import (
	"net/http"
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
}
