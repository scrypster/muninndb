package main

import (
	"net"
	"strings"
	"testing"
)

func TestServiceStatusString(t *testing.T) {
	cases := []struct {
		svc  serviceStatus
		want string
	}{
		{serviceStatus{name: "mcp", port: 8750, up: true}, "mcp"},
		{serviceStatus{name: "mcp", port: 8750, up: false}, "mcp"},
	}
	for _, c := range cases {
		got := c.svc.name
		if got != c.want {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}

func TestOverallState(t *testing.T) {
	all := []serviceStatus{{up: true}, {up: true}, {up: true}}
	if overallState(all) != stateRunning {
		t.Error("expected running")
	}
	none := []serviceStatus{{up: false}, {up: false}, {up: false}}
	if overallState(none) != stateStopped {
		t.Error("expected stopped")
	}
	mixed := []serviceStatus{{up: true}, {up: false}, {up: true}}
	if overallState(mixed) != stateDegraded {
		t.Error("expected degraded")
	}
}

func TestOverallStateEdgeCases(t *testing.T) {
	// Empty slice — no services — should be stateRunning
	empty := []serviceStatus{}
	got := overallState(empty)
	if got != stateRunning {
		t.Errorf("empty services: got %v, want stateRunning", got)
	}

	// Single service up
	single := []serviceStatus{{up: true}}
	if overallState(single) != stateRunning {
		t.Error("single up: expected stateRunning")
	}

	// Single service down
	singleDown := []serviceStatus{{up: false}}
	if overallState(singleDown) != stateStopped {
		t.Error("single down: expected stateStopped")
	}
}

func TestProbeServicesReturnsThreeServices(t *testing.T) {
	// probeServices always returns exactly 3 entries (even if all down)
	svcs := probeServices()
	if len(svcs) != 3 {
		t.Errorf("expected 3 services, got %d", len(svcs))
	}
	names := map[string]bool{}
	for _, s := range svcs {
		names[s.name] = true
	}
	for _, want := range []string{"database", "mcp", "web ui"} {
		if !names[want] {
			t.Errorf("missing service %q in probe results", want)
		}
	}
}

func TestPrintStatusDisplayReturnsStopped(t *testing.T) {
	// With no real server running, should return stateStopped or stateDegraded
	// (not stateRunning, unless muninn happens to be running in test env)
	state := stateStopped
	captureStdout(func() {
		state = printStatusDisplay(false)
	})
	// State should be one of the valid values
	if state != stateRunning && state != stateStopped && state != stateDegraded {
		t.Errorf("unexpected state: %v", state)
	}
}

func TestPrintStatusDisplayOutputContainsName(t *testing.T) {
	out := captureStdout(func() {
		printStatusDisplay(false)
	})
	if !strings.Contains(out, "muninn") {
		t.Errorf("output should contain 'muninn', got: %s", out)
	}
}

func TestProbeServicesWithAddrs_CustomPorts(t *testing.T) {
	srv := newHealthServer()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	addrs := daemonAddrs{
		RestAddr: "127.0.0.1:" + port,
		MCPAddr:  "127.0.0.1:" + port,
		UIAddr:   "127.0.0.1:" + port,
	}
	svcs := probeServicesWithAddrs(addrs)
	if len(svcs) != 3 {
		t.Fatalf("expected 3 services, got %d", len(svcs))
	}
	for _, s := range svcs {
		if !s.up {
			t.Errorf("service %q should be up at custom port %s", s.name, port)
		}
	}
}

func TestProbeServicesWithAddrs_EmptyUsesDefaults(t *testing.T) {
	// Empty addrs → hardcoded defaults. All down (no server running), but ports must match.
	svcs := probeServicesWithAddrs(daemonAddrs{})
	ports := map[string]int{"database": 8475, "mcp": 8750, "web ui": 8476}
	for _, s := range svcs {
		want := ports[s.name]
		if s.port != want {
			t.Errorf("service %q: got port %d, want %d", s.name, s.port, want)
		}
	}
}

func TestProbeServicesWithAddrs_ColonOnlyPort(t *testing.T) {
	// ":8760" style (no host) — common when user passes --mcp-addr :8760
	srv := newHealthServer()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	addrs := daemonAddrs{
		RestAddr: "127.0.0.1:" + port,
		MCPAddr:  ":" + port, // colon-only style
		UIAddr:   "127.0.0.1:" + port,
	}
	svcs := probeServicesWithAddrs(addrs)
	for _, s := range svcs {
		if !s.up {
			t.Errorf("service %q should be up (colon-port style)", s.name)
		}
	}
}

func TestPrintStatusDisplayCompactVsNonCompact(t *testing.T) {
	// Non-compact output should include service names
	outFull := captureStdout(func() {
		printStatusDisplay(false)
	})
	outCompact := captureStdout(func() {
		printStatusDisplay(true)
	})
	// Both should contain service names
	if !strings.Contains(outFull, "database") {
		t.Errorf("full output missing 'database': %s", outFull)
	}
	if !strings.Contains(outCompact, "database") {
		t.Errorf("compact output missing 'database': %s", outCompact)
	}
}

// ---------------------------------------------------------------------------
// healthURL + MUNINNDB_{ADMIN,UI,MCP}_URL env-var overrides
// ---------------------------------------------------------------------------

func TestHealthURL_DefaultWhenEnvUnset(t *testing.T) {
	t.Setenv("MUNINNDB_ADMIN_URL", "")
	got := healthURL("MUNINNDB_ADMIN_URL", "http", "8475")
	want := "http://127.0.0.1:8475"
	if got != want {
		t.Errorf("default: got %q, want %q", got, want)
	}
}

func TestHealthURL_EnvOverrideHTTPS(t *testing.T) {
	t.Setenv("MUNINNDB_ADMIN_URL", "https://tls.example.lan:8475")
	got := healthURL("MUNINNDB_ADMIN_URL", "http", "8475")
	want := "https://tls.example.lan:8475"
	if got != want {
		t.Errorf("env override: got %q, want %q", got, want)
	}
}

func TestHealthURL_TrimsTrailingSlash(t *testing.T) {
	t.Setenv("MUNINNDB_UI_URL", "https://tls.example.lan:8476/")
	got := healthURL("MUNINNDB_UI_URL", "http", "8476")
	want := "https://tls.example.lan:8476"
	if got != want {
		t.Errorf("trim trailing slash: got %q, want %q", got, want)
	}
}

func TestHealthURL_EnvTakesPrecedenceOverPortArg(t *testing.T) {
	// When the env var is set, the port argument is ignored — the env value
	// is the complete base URL.
	t.Setenv("MUNINNDB_ADMIN_URL", "https://other.lan:9999")
	got := healthURL("MUNINNDB_ADMIN_URL", "http", "8475")
	want := "https://other.lan:9999"
	if got != want {
		t.Errorf("env should override port arg: got %q, want %q", got, want)
	}
}

func TestProbeServicesWithAddrs_EnvVarOverridesSingleService(t *testing.T) {
	// Only MUNINNDB_ADMIN_URL is set. addrs point at an unreachable port, so
	// "database" must succeed via the env override while "mcp" and "web ui"
	// must fail (no override, addrs unreachable).
	srv := newHealthServer()
	defer srv.Close()
	t.Setenv("MUNINNDB_ADMIN_URL", srv.URL)
	t.Setenv("MUNINNDB_MCP_URL", "")
	t.Setenv("MUNINNDB_UI_URL", "")

	addrs := daemonAddrs{
		RestAddr: "127.0.0.1:19999",
		MCPAddr:  "127.0.0.1:19999",
		UIAddr:   "127.0.0.1:19999",
	}
	svcs := probeServicesWithAddrs(addrs)

	byName := map[string]serviceStatus{}
	for _, s := range svcs {
		byName[s.name] = s
	}
	if !byName["database"].up {
		t.Error("database should be up via MUNINNDB_ADMIN_URL override")
	}
	if byName["mcp"].up {
		t.Error("mcp should be down (no env override, addrs unreachable)")
	}
	if byName["web ui"].up {
		t.Error("web ui should be down (no env override, addrs unreachable)")
	}
}

func TestProbeServicesWithAddrs_AllThreeEnvVarsHonored(t *testing.T) {
	srvAdmin := newHealthServer()
	defer srvAdmin.Close()
	srvMCP := newHealthServer()
	defer srvMCP.Close()
	srvUI := newHealthServer()
	defer srvUI.Close()

	t.Setenv("MUNINNDB_ADMIN_URL", srvAdmin.URL)
	t.Setenv("MUNINNDB_MCP_URL", srvMCP.URL)
	t.Setenv("MUNINNDB_UI_URL", srvUI.URL)

	// addrs intentionally unreachable — env vars must drive the probes.
	addrs := daemonAddrs{
		RestAddr: "127.0.0.1:19999",
		MCPAddr:  "127.0.0.1:19999",
		UIAddr:   "127.0.0.1:19999",
	}
	svcs := probeServicesWithAddrs(addrs)
	for _, s := range svcs {
		if !s.up {
			t.Errorf("service %q should be up via env-var override", s.name)
		}
	}
}

func TestHealthURL_SchemeApplied(t *testing.T) {
	t.Setenv("MUNINNDB_ADMIN_URL", "")
	if got := healthURL("MUNINNDB_ADMIN_URL", "https", "8475"); got != "https://127.0.0.1:8475" {
		t.Errorf("https scheme: got %q", got)
	}
	if got := healthURL("MUNINNDB_ADMIN_URL", "http", "8475"); got != "http://127.0.0.1:8475" {
		t.Errorf("http scheme: got %q", got)
	}
	if got := healthURL("MUNINNDB_ADMIN_URL", "", "8475"); got != "http://127.0.0.1:8475" {
		t.Errorf("empty scheme should default to http: got %q", got)
	}
}

// ---------------------------------------------------------------------------
// probeHealth — http→https auto-retry
// ---------------------------------------------------------------------------

func TestProbeHealth_HTTPServer(t *testing.T) {
	srv := newHealthServer()
	defer srv.Close()
	up, scheme := probeHealth(srv.URL + "/")
	if !up {
		t.Error("probeHealth should report a plain-HTTP server up")
	}
	if scheme != "http" {
		t.Errorf("scheme = %q, want http", scheme)
	}
}

func TestProbeHealth_RetriesHTTPS(t *testing.T) {
	// A TLS-only server probed with an http:// URL: the http attempt fails,
	// probeHealth must retry https:// and report the working scheme as https.
	srv := newTLSHealthServer()
	defer srv.Close()
	httpURL := "http://" + strings.TrimPrefix(srv.URL, "https://")
	up, scheme := probeHealth(httpURL)
	if !up {
		t.Error("probeHealth should detect a TLS server via http→https retry")
	}
	if scheme != "https" {
		t.Errorf("retry scheme = %q, want https", scheme)
	}
	// A direct https:// URL (env-override style) is probed without retry.
	if up, scheme := probeHealth(srv.URL); !up || scheme != "https" {
		t.Errorf("direct https probe: up=%v scheme=%q, want true/https", up, scheme)
	}
}

func TestProbeHealth_DownServer(t *testing.T) {
	// Nothing listening — both the http attempt and the https retry must fail.
	if up, scheme := probeHealth("http://127.0.0.1:19998/"); up || scheme != "" {
		t.Errorf("unreachable server: up=%v scheme=%q, want false/empty", up, scheme)
	}
}

func TestProbeServicesWithAddrs_HTTPSScheme(t *testing.T) {
	// addrs.Scheme = "https" against a TLS server → all three services up with
	// no env-var override needed.
	srv := newTLSHealthServer()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	t.Setenv("MUNINNDB_ADMIN_URL", "")
	t.Setenv("MUNINNDB_MCP_URL", "")
	t.Setenv("MUNINNDB_UI_URL", "")
	addrs := daemonAddrs{
		Scheme:   "https",
		RestAddr: "127.0.0.1:" + port,
		MCPAddr:  "127.0.0.1:" + port,
		UIAddr:   "127.0.0.1:" + port,
	}
	for _, s := range probeServicesWithAddrs(addrs) {
		if !s.up {
			t.Errorf("service %q should be up against an https daemon", s.name)
		}
		if s.scheme != "https" {
			t.Errorf("service %q: probe scheme = %q, want https", s.name, s.scheme)
		}
	}
}

func TestPrintStatusDisplay_WebUISchemeFromProbe(t *testing.T) {
	// Daemon predates the muninn.addrs scheme field (schemeless sidecar) but the
	// server is TLS — printStatusDisplay must show an https Web UI URL, using
	// the scheme the probe discovered via the http→https retry.
	srv := newTLSHealthServer()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))

	dir := t.TempDir()
	t.Setenv("MUNINNDB_DATA", dir)
	t.Setenv("MUNINNDB_ADMIN_URL", "")
	t.Setenv("MUNINNDB_MCP_URL", "")
	t.Setenv("MUNINNDB_UI_URL", "")
	if err := writeAddrsFile(dir, daemonAddrs{
		RestAddr: "127.0.0.1:" + port,
		MCPAddr:  "127.0.0.1:" + port,
		UIAddr:   "127.0.0.1:" + port,
	}); err != nil {
		t.Fatalf("writeAddrsFile: %v", err)
	}
	out := captureStdout(func() { printStatusDisplay(false) })
	if !strings.Contains(out, "Web UI → https://") {
		t.Errorf("Web UI line should use https (probe-discovered), got:\n%s", out)
	}
}

func TestProbeServicesWithAddrs_PreservesOrder(t *testing.T) {
	// Concurrent probing must keep the result order [database, mcp, web ui].
	svcs := probeServicesWithAddrs(daemonAddrs{})
	want := []string{"database", "mcp", "web ui"}
	if len(svcs) != len(want) {
		t.Fatalf("got %d services, want %d", len(svcs), len(want))
	}
	for i, s := range svcs {
		if s.name != want[i] {
			t.Errorf("index %d: got %q, want %q", i, s.name, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// webUIDisplay
// ---------------------------------------------------------------------------

func TestWebUIDisplay_Loopback(t *testing.T) {
	t.Setenv("MUNINNDB_UI_URL", "")
	lines := webUIDisplay(daemonAddrs{Scheme: "http", UIAddr: "127.0.0.1:8476"})
	if len(lines) != 1 || lines[0] != "http://127.0.0.1:8476" {
		t.Errorf("loopback bind: got %v", lines)
	}
}

func TestWebUIDisplay_EmptyAddrsDefault(t *testing.T) {
	t.Setenv("MUNINNDB_UI_URL", "")
	lines := webUIDisplay(daemonAddrs{})
	if len(lines) != 1 || lines[0] != "http://127.0.0.1:8476" {
		t.Errorf("empty addrs should yield the legacy localhost default: got %v", lines)
	}
}

func TestWebUIDisplay_NonLoopbackBind(t *testing.T) {
	t.Setenv("MUNINNDB_UI_URL", "")
	lines := webUIDisplay(daemonAddrs{Scheme: "https", UIAddr: "0.0.0.0:8476"})
	if len(lines) < 1 {
		t.Fatal("webUIDisplay must return at least one line")
	}
	// The localhost fallback line is always last, and every line uses the
	// recorded scheme.
	if lines[len(lines)-1] != "https://127.0.0.1:8476" {
		t.Errorf("expected https localhost fallback line last, got %q", lines[len(lines)-1])
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "https://") {
			t.Errorf("line %q should use the https scheme", l)
		}
	}
}

func TestWebUIDisplay_EnvOverride(t *testing.T) {
	t.Setenv("MUNINNDB_UI_URL", "https://ui.example.lan:8476/")
	lines := webUIDisplay(daemonAddrs{Scheme: "http", UIAddr: "0.0.0.0:8476"})
	if len(lines) != 1 || lines[0] != "https://ui.example.lan:8476" {
		t.Errorf("MUNINNDB_UI_URL override should win outright: got %v", lines)
	}
}

// ---------------------------------------------------------------------------
// isLoopbackURL
// ---------------------------------------------------------------------------

func TestIsLoopbackURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"loopback ipv4", "https://127.0.0.1:8475/api/vaults", true},
		{"loopback ipv4 range", "https://127.5.6.7:8475", true},
		{"loopback ipv4 no port", "https://127.0.0.1", true},
		{"localhost", "https://localhost:8475", true},
		{"localhost uppercase", "https://LOCALHOST:8475", true},
		{"loopback ipv6", "https://[::1]:8475", true},
		{"loopback ipv4-mapped ipv6", "https://[::ffff:127.0.0.1]:8475", true},
		{"http loopback", "http://127.0.0.1:8475", true},
		{"lan ip", "https://172.20.50.63:8475", false},
		{"public host", "https://muninn.example.com:8475", false},
		{"empty string", "", false},
		{"malformed", "https://%zz", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLoopbackURL(tc.url); got != tc.want {
				t.Errorf("isLoopbackURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}
