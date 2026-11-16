package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The cmd/muninn test package must never reach a developer's running daemon,
// their real home directory, or the network. The CLI's defaults point at all
// three: REST on 127.0.0.1:8475, the UI login on :8476, MCP on :8750, data and
// tokens under ~/.muninn. Several tests used those defaults directly, so running
// `go test ./cmd/muninn/` next to a live daemon posted the default admin
// credentials to it.
//
// installHermeticGuard runs from TestMain, before any test, and makes that
// structurally impossible rather than relying on each test to remember:
//
//   - every address variable the CLI reads points at 127.0.0.1:1, which refuses;
//   - the package-level admin bases (set by vault_auth.go's init, which runs
//     before TestMain) are reassigned to the same address;
//   - HOME is a temp directory, and the variables that would steer the CLI back
//     to real state (MUNINNDB_DATA, MUNINN_MCP_TOKEN, TLS paths, proxies) are
//     cleared;
//   - the release lookup is stubbed;
//   - a dial tripwire on http.DefaultTransport, insecureLoopbackTransport and
//     dialServedCertFn refuses any dial to a default daemon port or to a
//     non-loopback host, and records it, so the run fails even when the test
//     that made the dial passed. (applyBehaviorToVault builds its own transport
//     for a loopback https admin base; it is reached only after a successful
//     login, which goes through a guarded transport.)
//
// Tests that need a server keep using their own httptest servers and override
// the variable or package var for their own duration.

// hermeticUnreachableURL is where every CLI address points during tests. Port 1
// is not a port anything in this project binds, so a dial is refused at once.
const hermeticUnreachableURL = "http://127.0.0.1:1"

// defaultDaemonPorts are the ports a default daemon binds (server.go flag
// defaults: mbp 8474, rest 8475, ui 8476, grpc 8477, mcp 8750).
var defaultDaemonPorts = map[string]bool{
	"8474": true, "8475": true, "8476": true, "8477": true, "8750": true,
}

// hermeticAddrEnv are the address variables the CLI honours.
//
//	MUNINNDB_ADMIN_URL  vault_auth.go init, status.go healthURL (REST probe), repl_client.go cmdShowVaults
//	MUNINNDB_UI_URL     vault_auth.go init, status.go healthURL + webUIDisplay, repl.go runShell, init.go alignLocalAdminBasesToDaemon
//	MUNINNDB_MCP_URL    status.go healthURL (MCP probe), lifecycle.go runStart health wait, repl.go runShell
//	MUNINN_MCP_URL      init.go clientMCPURL, mcp_stdio.go proxy target
var hermeticAddrEnv = []string{
	"MUNINNDB_ADMIN_URL", "MUNINNDB_UI_URL", "MUNINNDB_MCP_URL", "MUNINN_MCP_URL",
}

// hermeticClearEnv are variables inherited from a developer's shell that would
// steer the CLI at real state. They are unset, so each test starts from the
// CLI's own defaults under the temp HOME.
var hermeticClearEnv = []string{
	"MUNINNDB_DATA", "MUNINN_MCP_TOKEN", "MUNINN_TLS_CERT", "MUNINN_TLS_KEY",
	"MUNINN_UI_ADDR", "MUNINN_LISTEN_HOST", "XDG_CONFIG_HOME",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy",
}

var errHermeticRefused = errors.New("hermetic test guard refused the dial")

var hermetic struct {
	root string // temp dir holding the test HOME; "" until installHermeticGuard ran

	// allowDefaultPorts lets the integration suite, which drives a real daemon
	// on the default ports after its own pre-flight check, dial them.
	allowDefaultPorts bool

	mu         sync.Mutex
	violations []string
	selfCheck  bool // TestHermeticGuard's own probes are not violations
}

// installHermeticGuard applies the guard described above. The returned cleanup
// removes the temp HOME.
func installHermeticGuard() (cleanup func(), err error) {
	root, err := os.MkdirTemp("", "muninn-cli-test-*")
	if err != nil {
		return nil, err
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	os.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		os.Setenv("USERPROFILE", home)
		os.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		os.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	}
	for _, k := range hermeticClearEnv {
		os.Unsetenv(k)
	}
	for _, k := range hermeticAddrEnv {
		os.Setenv(k, hermeticUnreachableURL)
	}

	// vault_auth.go's init() read the environment before TestMain ran.
	vaultAdminBase = hermeticUnreachableURL
	vaultUIBase = hermeticUnreachableURL
	vaultCookie = ""

	// The real lookup calls the GitHub API. Keep its dev-build behaviour (no
	// lookup, no error) and refuse otherwise; tests that need a release inject
	// their own.
	latestReleaseFn = func() (releaseInfo, error) {
		if muninnVersion() == "dev" {
			return releaseInfo{}, nil
		}
		return releaseInfo{}, fmt.Errorf("%w: release lookup is disabled in tests", errHermeticRefused)
	}

	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	guarded := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if err := hermeticCheckDial(addr); err != nil {
			return nil, err
		}
		return dialer.DialContext(ctx, network, addr)
	}
	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		os.RemoveAll(root)
		return nil, fmt.Errorf("http.DefaultTransport is %T, cannot install the dial guard", http.DefaultTransport)
	}
	dt.DialContext = guarded
	insecureLoopbackTransport.DialContext = guarded
	realDialServedCert := dialServedCertFn
	dialServedCertFn = func(hostport string) (*x509.Certificate, uint16, uint16, error) {
		if err := hermeticCheckDial(hostport); err != nil {
			return nil, 0, 0, err
		}
		return realDialServedCert(hostport)
	}

	hermetic.root = root
	return func() { os.RemoveAll(root) }, nil
}

// hermeticCheckDial refuses, and records, a dial to a default daemon port or to
// anything that is not loopback.
func hermeticCheckDial(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, ""
	}
	reason := ""
	switch {
	case defaultDaemonPorts[port] && !hermetic.allowDefaultPorts:
		reason = "default daemon port"
	case !hermeticIsLoopback(host):
		reason = "non-loopback host"
	default:
		return nil
	}
	v := fmt.Sprintf("%s (%s)", addr, reason)
	if c := hermeticCaller(); c != "" {
		v += " from " + c
	}
	hermetic.mu.Lock()
	selfCheck := hermetic.selfCheck
	if !selfCheck {
		hermetic.violations = append(hermetic.violations, v)
	}
	hermetic.mu.Unlock()
	if !selfCheck {
		// Also report at once, so under -v the line sits next to the test that
		// made the dial.
		fmt.Fprintf(os.Stderr, "hermetic guard: refused %s\n", v)
	}
	return fmt.Errorf("%w: %s", errHermeticRefused, v)
}

func hermeticIsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hermeticCaller names the test or package function that made the dial, when
// the dial runs on its goroutine. net/http dials on a goroutine of its own, so
// for HTTP it returns "": the immediate stderr line, which go test -v prints
// inside the offending test's RUN block, identifies it instead.
func hermeticCaller() string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	fallback := ""
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.File, "_test.go") && strings.Contains(f.Function, ".Test") {
			return f.Function
		}
		if fallback == "" && !strings.HasSuffix(f.File, "hermetic_test.go") &&
			(strings.HasPrefix(f.Function, "main.") || strings.Contains(f.Function, "/muninndb/")) {
			fallback = f.Function
		}
		if !more {
			return fallback
		}
	}
}

func hermeticViolations() []string {
	hermetic.mu.Lock()
	defer hermetic.mu.Unlock()
	return append([]string(nil), hermetic.violations...)
}

// runHermetic is the body of the package's TestMain.
func runHermetic(m *testing.M) int {
	cleanup, err := installHermeticGuard()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermetic guard: %v\n", err)
		return 1
	}
	code := m.Run()
	cleanup()
	return hermeticReport(code)
}

// hermeticReport fails the run when any dial was refused, even if the test
// that made it passed.
func hermeticReport(code int) int {
	if v := hermeticViolations(); len(v) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL\thermetic guard refused %d dial(s) that would have left the test process:\n", len(v))
		for _, s := range v {
			fmt.Fprintf(os.Stderr, "\t%s\n", s)
		}
		if code == 0 {
			code = 1
		}
	}
	return code
}

// TestHermeticGuard pins the guard: with TestMain in effect, every base URL the
// CLI computes resolves to a non-default port, HOME and the data directory are
// not the real home, the release lookup is stubbed, and the transports refuse a
// default-port dial. It never dials anything itself: the transport checks use a
// cancelled context, which a stock dialer rejects before connecting.
func TestHermeticGuard(t *testing.T) {
	if hermetic.root == "" {
		t.Error("hermetic guard not installed: TestMain must call installHermeticGuard before any test runs")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if !hermeticWithin(hermetic.root, home) {
		t.Errorf("home directory %q is outside the test root %q", home, hermetic.root)
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" && filepath.Clean(u.HomeDir) == filepath.Clean(home) {
		t.Errorf("home directory %q is the real home %q", home, u.HomeDir)
	}
	for name, p := range map[string]string{"defaultDataDir": defaultDataDir(), "configPath": configPath()} {
		if !hermeticWithin(hermetic.root, p) {
			t.Errorf("%s = %q is outside the test root %q", name, p, hermetic.root)
		}
	}

	// Every base URL the CLI would dial. clientUIURL and clientRESTURL are not
	// listed: they are only printed or written into generated configs.
	urls := map[string]string{
		"vaultAdminBase":                vaultAdminBase,
		"vaultUIBase":                   vaultUIBase,
		"healthURL(MUNINNDB_ADMIN_URL)": healthURL("MUNINNDB_ADMIN_URL", "", "8475"),
		"healthURL(MUNINNDB_MCP_URL)":   healthURL("MUNINNDB_MCP_URL", "", "8750"),
		"healthURL(MUNINNDB_UI_URL)":    healthURL("MUNINNDB_UI_URL", "", "8476"),
		"webUIDisplay":                  webUIDisplay(daemonAddrs{})[0],
		"clientMCPURL":                  clientMCPURL(),
	}
	for _, k := range hermeticAddrEnv {
		urls["$"+k] = os.Getenv(k)
	}
	for name, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			t.Errorf("%s = %q does not parse as a URL", name, raw)
			continue
		}
		if defaultDaemonPorts[u.Port()] || !hermeticIsLoopback(u.Hostname()) {
			t.Errorf("%s = %q can reach a real daemon or the network", name, raw)
		}
	}

	if reflect.ValueOf(latestReleaseFn).Pointer() == reflect.ValueOf(latestReleaseDefault).Pointer() {
		t.Error("latestReleaseFn is the real GitHub lookup")
	}
	if reflect.ValueOf(dialServedCertFn).Pointer() == reflect.ValueOf(dialServedCert).Pointer() {
		t.Error("dialServedCertFn is the unguarded TLS dial")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hermetic.mu.Lock()
	hermetic.selfCheck = true
	hermetic.mu.Unlock()
	defer func() {
		hermetic.mu.Lock()
		hermetic.selfCheck = false
		hermetic.mu.Unlock()
	}()
	for name, tr := range map[string]*http.Transport{
		"http.DefaultTransport":     http.DefaultTransport.(*http.Transport),
		"insecureLoopbackTransport": insecureLoopbackTransport,
	} {
		if tr.DialContext == nil {
			t.Errorf("%s has no dial guard", name)
			continue
		}
		_, err := tr.DialContext(ctx, "tcp", "127.0.0.1:8475")
		if !hermetic.allowDefaultPorts && !errors.Is(err, errHermeticRefused) {
			t.Errorf("%s did not refuse a default-port dial (err=%v)", name, err)
		}
		_, err = tr.DialContext(ctx, "tcp", "203.0.113.7:443")
		if !errors.Is(err, errHermeticRefused) {
			t.Errorf("%s did not refuse a non-loopback dial (err=%v)", name, err)
		}
	}
}

func hermeticWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// withNoServer points every CLI address at hermeticUnreachableURL, and the data
// directory at an empty temp dir (no muninn.addrs sidecar to steer the probe),
// for the test's duration. TestMain already does the address part for the whole
// package; a test whose meaning is "no daemon is running" says so itself, so it
// keeps that meaning without the package default and never falls back to a
// default port.
func withNoServer(t *testing.T) {
	t.Helper()
	for _, k := range hermeticAddrEnv {
		t.Setenv(k, hermeticUnreachableURL)
	}
	t.Setenv("MUNINNDB_DATA", t.TempDir())
	oldAdmin, oldUI, oldCookie := vaultAdminBase, vaultUIBase, vaultCookie
	vaultAdminBase, vaultUIBase = hermeticUnreachableURL, hermeticUnreachableURL
	t.Cleanup(func() { vaultAdminBase, vaultUIBase, vaultCookie = oldAdmin, oldUI, oldCookie })
}
