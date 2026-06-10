package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// healthURL builds a base URL for a service health probe.
// If envVar is set, it's used verbatim as the base URL (trailing slash
// trimmed), matching the MUNINNDB_ADMIN_URL / MUNINNDB_UI_URL convention
// established in vault_auth.go (see #410 / #424). Otherwise falls back to
// <scheme>://127.0.0.1:<port>; an empty scheme is treated as "http",
// preserving the legacy default for non-TLS deployments.
//
// Callers append the per-service path (e.g. "/api/health").
func healthURL(envVar, scheme, port string) string {
	if v := os.Getenv(envVar); v != "" {
		return strings.TrimRight(v, "/")
	}
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://127.0.0.1:" + port
}

// checkVersionHint prints a one-liner if a newer version is available.
// Returns immediately if the check takes more than 3 seconds.
func checkVersionHint() {
	ch := make(chan string, 1)
	go func() {
		latest, err := latestVersion()
		if err != nil || latest == "" {
			ch <- ""
			return
		}
		if newerVersionAvailable(muninnVersion(), latest) {
			ch <- latest
		} else {
			ch <- ""
		}
	}()
	select {
	case latest := <-ch:
		if latest != "" {
			fmt.Printf("  Update available: %s — run 'muninn upgrade'\n\n", latest)
		}
	case <-time.After(3 * time.Second):
		// timeout — don't block status output
	}
}

type runState int

const (
	stateStopped  runState = iota
	stateDegraded          // some up, some down
	stateRunning           // all up
)

type serviceStatus struct {
	name    string
	port    int
	up      bool
	scheme  string // scheme the probe reached the service on ("http"/"https"/"")
	note    string // optional: "not responding"
	certErr bool   // probe failed specifically due to TLS certificate verification
}

// portFromAddr extracts the port from a "host:port" address, returning fallback
// when addr is empty or unparseable.
func portFromAddr(addr, fallback string) string {
	if addr != "" {
		if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
			return p
		}
	}
	return fallback
}

// resolveScheme returns the daemon's client-facing scheme. When the sidecar
// predates the Scheme field (empty), it recovers the scheme the web-ui health
// probe actually reached, so a legacy TLS daemon is not misreported as http.
func resolveScheme(addrs daemonAddrs, svcs []serviceStatus) string {
	if addrs.Scheme != "" {
		return addrs.Scheme
	}
	for _, s := range svcs {
		if s.name == "web ui" && s.scheme != "" {
			return s.scheme
		}
	}
	return ""
}

// overallState computes the aggregate state from individual service statuses.
func overallState(svcs []serviceStatus) runState {
	up, down := 0, 0
	for _, s := range svcs {
		if s.up {
			up++
		} else {
			down++
		}
	}
	if down == 0 {
		return stateRunning
	}
	if up == 0 {
		return stateStopped
	}
	return stateDegraded
}

// probeServicesFn is the default health-check probe. Tests override it.
var probeServicesFn = probeServicesDefault

// probeServices delegates to probeServicesFn for testability.
func probeServices() []serviceStatus { return probeServicesFn() }

// probeServicesDefault reads the actual bound addresses from the data directory
// and probes the correct ports. Falls back to hardcoded defaults when the
// sidecar file is absent (daemon stopped or pre-fix version).
func probeServicesDefault() []serviceStatus {
	addrs, _ := readAddrsFile(defaultDataDir())
	return probeServicesWithAddrs(addrs)
}

// probeHealth reports whether the service health endpoint at url is up (a 2xx
// response) and the scheme that actually worked ("http" or "https"; "" when
// down). If an http:// probe fails — transport error or non-2xx — it retries
// once over https://, so a TLS deployment is detected with no configuration;
// the returned scheme then lets callers correct a stale display URL. An
// https:// URL (e.g. an env-var override) is probed directly.
//
// The https attempt skips certificate verification only for a loopback URL:
// there it is a localhost liveness check, not a security boundary, and an
// internal-CA or self-signed cert must not make a healthy server look [down].
// An off-host https URL (e.g. a MUNINNDB_*_URL env override) keeps full
// verification, consistent with httpClientForURL — so a remote server behind
// an untrusted cert reads as [down] rather than silently probed insecurely.
// probeHealth additionally reports certErr=true when an https probe failed
// specifically because the certificate didn't verify (untrusted CA, hostname
// mismatch, or expired) — distinct from a connection refusal/timeout — so the
// status display can tell "server up, cert untrusted" apart from "down".
func probeHealth(url string) (up bool, scheme string, certErr bool) {
	switch {
	case strings.HasPrefix(url, "https://"):
		ok, err := probeOnce(url, isLoopbackURL(url))
		if ok {
			return true, "https", false
		}
		return false, "", isCertVerificationError(err)
	case strings.HasPrefix(url, "http://"):
		if ok, _ := probeOnce(url, false); ok {
			return true, "http", false
		}
		// The http probe failed — the server may actually speak TLS on this
		// port. Retry once over https before concluding it is down.
		httpsURL := "https://" + strings.TrimPrefix(url, "http://")
		ok, err := probeOnce(httpsURL, isLoopbackURL(httpsURL))
		if ok {
			return true, "https", false
		}
		return false, "", isCertVerificationError(err)
	default:
		ok, _ := probeOnce(url, false)
		return ok, "", false
	}
}

// probeOnce performs a single health-check GET and returns the underlying error
// so callers can distinguish a TLS trust failure from a dead server. When
// insecure is set the TLS client skips certificate verification (see probeHealth
// for the rationale).
func probeOnce(url string, insecure bool) (bool, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	if insecure {
		client.Transport = insecureLoopbackTransport
		// Same as httpClientForURL: an insecure (loopback) probe must not carry
		// the verification skip off-host via a redirect.
		client.CheckRedirect = loopbackOnlyRedirect
	}
	resp, err := client.Get(url)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
}

// isCertVerificationError reports whether err is a TLS certificate trust or
// validity failure (untrusted CA, hostname mismatch, expired/not-yet-valid) —
// as opposed to a connection refusal or timeout.
func isCertVerificationError(err error) bool {
	if err == nil {
		return false
	}
	var ce *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	return errors.As(err, &ce) || errors.As(err, &ua) ||
		errors.As(err, &he) || errors.As(err, &ci)
}

// probeServicesWithAddrs is the testable implementation. addrs contains the
// actual addresses (and scheme) the daemon bound to; empty fields fall back to
// the default ports and an "http" scheme.
func probeServicesWithAddrs(addrs daemonAddrs) []serviceStatus {
	restPort := portFromAddr(addrs.RestAddr, "8475")
	mcpPort := portFromAddr(addrs.MCPAddr, "8750")
	uiPort := portFromAddr(addrs.UIAddr, "8476")

	restPortInt, _ := strconv.Atoi(restPort)
	mcpPortInt, _ := strconv.Atoi(mcpPort)
	uiPortInt, _ := strconv.Atoi(uiPort)

	svcs := []serviceStatus{
		{name: "database", port: restPortInt},
		{name: "mcp", port: mcpPortInt},
		{name: "web ui", port: uiPortInt},
	}
	urls := []string{
		healthURL("MUNINNDB_ADMIN_URL", addrs.Scheme, restPort) + "/api/health",
		healthURL("MUNINNDB_MCP_URL", addrs.Scheme, mcpPort) + "/mcp/health",
		healthURL("MUNINNDB_UI_URL", addrs.Scheme, uiPort) + "/",
	}
	// Probe concurrently: with the http→https retry a serial sweep could take
	// several seconds when services are unreachable. Each goroutine writes its
	// own fixed index, so result order stays [database, mcp, web ui].
	var wg sync.WaitGroup
	for i := range svcs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			svcs[i].up, svcs[i].scheme, svcs[i].certErr = probeHealth(urls[i])
		}(i)
	}
	wg.Wait()
	return svcs
}

// webUIDisplay returns the Web UI URL(s) to show the operator, derived from the
// daemon's recorded bind address. It always returns at least one element, so
// callers may safely index [0].
//   - MUNINNDB_UI_URL set        → that URL, verbatim.
//   - UI bound to loopback/empty → just the localhost URL.
//   - UI bound to 0.0.0.0 / LAN  → a routable host first, then the localhost URL
//     as an always-works fallback. Under TLS, the routable host is the cert's
//     DNS SAN (addrs.CertHost) when available, so the printed URL passes TLS
//     verification; otherwise it falls back to os.Hostname().
func webUIDisplay(addrs daemonAddrs) []string {
	if v := os.Getenv("MUNINNDB_UI_URL"); v != "" {
		return []string{strings.TrimRight(v, "/")}
	}
	scheme := addrs.Scheme
	if scheme == "" {
		scheme = "http"
	}
	host, port := "", "8476"
	if addrs.UIAddr != "" {
		if h, p, err := net.SplitHostPort(addrs.UIAddr); err == nil {
			host = h
			if p != "" {
				port = p
			}
		}
	}
	local := scheme + "://127.0.0.1:" + port
	if hostIsRoutable(host) {
		if hn := routableUIHost(scheme, addrs.CertHost); hn != "" {
			return []string{scheme + "://" + hn + ":" + port, local}
		}
	}
	return []string{local}
}

// routableUIHost picks the host for the routable Web UI URL: under TLS, the
// cert's DNS SAN (so the URL passes verification) when the daemon recorded one,
// otherwise os.Hostname(). Returns "" only if os.Hostname() fails and no SAN.
func routableUIHost(scheme, certHost string) string {
	if scheme == "https" && certHost != "" {
		return certHost
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}

// hostIsRoutable reports whether host is a non-loopback bind — i.e. the UI is
// reachable beyond this machine, so a routable hostname is worth showing
// alongside the localhost URL.
func hostIsRoutable(host string) bool {
	switch host {
	case "", "127.0.0.1", "::1", "localhost":
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return true
}

// isLoopbackURL reports whether rawURL targets this machine — its host is
// "localhost", a loopback IP, or any address in the 127.0.0.0/8 range. It is
// the URL-level guard for deciding when skipping TLS certificate verification
// is acceptable: a loopback connection never leaves the machine, an off-host
// one can be intercepted in transit. (Loopback is a convenience boundary, not
// a hard one — when the daemon is down, another local process could bind its
// port. Verifying against the muninn-managed CA instead is a possible future
// hardening.) Unparseable input is treated as non-loopback so callers fail
// closed (keep verification on).
func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// insecureLoopbackTransport is shared by every client httpClientForURL builds
// for a loopback https target, so repeated calls (REPL commands, the job-status
// poll) reuse TLS connections instead of handshaking — and abandoning idle
// sockets — per request. Idle bounds match http.DefaultTransport's spirit.
var insecureLoopbackTransport = &http.Transport{
	// loopback self-signed/internal-CA cert; see isLoopbackURL for the rationale
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	MaxIdleConns:    10,
	IdleConnTimeout: 90 * time.Second,
}

// httpClientForURL returns an http.Client for rawURL. It skips TLS verification
// only for a loopback https target — a self-signed/internal-CA daemon on the
// same machine — matching probeOnce's loopback rationale. A remote https
// endpoint keeps full verification. The insecure client also refuses to follow
// any redirect away from loopback: the verification skip (and a 307/308's
// replayed request body) must never travel to an off-host peer.
func httpClientForURL(rawURL string, timeout time.Duration) *http.Client {
	c := &http.Client{Timeout: timeout}
	// ToLower: the scheme is case-insensitive, and isLoopbackURL (via url.Parse)
	// already is — keep this prefix check consistent with it.
	if strings.HasPrefix(strings.ToLower(rawURL), "https://") && isLoopbackURL(rawURL) {
		c.Transport = insecureLoopbackTransport
		c.CheckRedirect = loopbackOnlyRedirect
	}
	return c
}

// maxRedirects mirrors net/http's default cap. A custom CheckRedirect REPLACES
// the default policy entirely, so any guard that sets one must re-impose the
// hop limit itself — otherwise a loopback responder that redirects to itself in
// a cycle is followed forever (bounded only by Client.Timeout, and not at all
// for the timeout-0 clients in the REPL/vault paths).
const maxRedirects = 10

// loopbackOnlyRedirect refuses any redirect whose target leaves loopback (the
// verification skip and a 307/308's replayed body must never reach an off-host
// peer) and re-imposes net/http's default hop cap.
func loopbackOnlyRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if !isLoopbackURL(req.URL.String()) {
		return fmt.Errorf("refusing redirect from loopback TLS endpoint to non-loopback %q", req.URL)
	}
	return nil
}

// printStatusDisplay prints the unified status view.
// compact=true omits the trailing hint lines (used before dropping into shell).
// Returns the overall state so callers can act on it.
func printStatusDisplay(compact bool) runState {
	svcs := probeServices()
	state := overallState(svcs)

	isTTY := isatty()
	bullet := func(up bool) string {
		if !isTTY {
			if up {
				return "[up]"
			}
			return "[down]"
		}
		if up {
			return "\033[32m●\033[0m" // green
		}
		return "\033[31m○\033[0m" // red
	}
	warn := func(s string) string {
		if isTTY {
			return "\033[33m" + s + "\033[0m"
		}
		return s
	}

	fmt.Println()

	switch state {
	case stateRunning:
		fmt.Printf("  muninn  %s  running\n", bullet(true))
	case stateStopped:
		fmt.Printf("  muninn  %s  stopped\n", bullet(false))
	case stateDegraded:
		fmt.Printf("  muninn  %s  %s\n", warn("⚠"), warn("degraded"))
	}

	fmt.Println()
	for _, s := range svcs {
		if s.up {
			fmt.Printf("    %-10s %d   %s\n", s.name, s.port, bullet(true))
		} else {
			fmt.Printf("    %-10s      %s\n", s.name, bullet(false))
		}
	}

	// Degraded: surface which service is down and how to fix
	if state == stateDegraded {
		fmt.Println()
		certIssue := false
		for _, s := range svcs {
			if s.up {
				continue
			}
			if s.certErr {
				certIssue = true
				fmt.Printf("  %s is reachable but its TLS certificate failed verification", s.name)
			} else {
				fmt.Printf("  %s is not responding", s.name)
			}
			if s.name == "mcp" {
				fmt.Print(" — your AI tools won't have memory access")
			}
			fmt.Println(".")
		}
		if certIssue {
			// A restart won't fix a trust problem — the server is up.
			fmt.Println("  The server is up but its certificate isn't trusted. Check that the")
			fmt.Println("  cert is valid for this host and that your CA is trusted (or, for a")
			fmt.Println("  remote endpoint, that MUNINNDB_*_URL points at a host the cert covers).")
		} else {
			fmt.Println("  Run: muninn restart")
		}
	}

	if !compact {
		if state == stateStopped {
			fmt.Println()
			fmt.Println("  muninn start  →  start all services")
			fmt.Println("  muninn help   →  see all commands")
		}
		if state == stateRunning {
			fmt.Println()
			addrs, _ := readAddrsFile(defaultDataDir())
			addrs.Scheme = resolveScheme(addrs, svcs)
			lines := webUIDisplay(addrs)
			fmt.Printf("  Web UI → %s\n", lines[0])
			for _, l := range lines[1:] {
				fmt.Printf("           %s\n", l)
			}
			checkVersionHint()
		}
	}

	fmt.Println()
	return state
}

// isatty returns true if stdout is an interactive terminal.
func isatty() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
