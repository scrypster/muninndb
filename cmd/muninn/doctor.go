package main

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/tlsutil"
)

// doctorReport is the gathered, render-free view of the daemon's TLS and
// connectivity state. Populated by gatherDoctor (the only I/O); rendered by the
// pure formatDoctor.
type doctorReport struct {
	svcs       []serviceStatus
	scheme     string              // "https" | "http" | "" (unknown / server stopped)
	addrs      daemonAddrs         // real bound addresses — displayed verbatim
	cert       *x509.Certificate   // leaf; nil when TLS off or cert unobtainable
	chain      []*x509.Certificate // additional chain certs (cert-file path only)
	certSource string              // "live socket" | "cert file" | ""
	tlsVersion uint16              // negotiated version (live path only; 0 otherwise)
	cipher     uint16              // negotiated cipher (live path only; 0 otherwise)
	certErr    string              // why the cert could not be inspected, if applicable
}

// dialServedCert opens a TLS connection to hostport and returns the leaf
// certificate the server presents, plus the negotiated version and cipher.
// Verification is intentionally skipped: doctor inspects the served cert, it
// does not trust it, and gatherDoctor only ever points this at loopback.
func dialServedCert(hostport string) (*x509.Certificate, uint16, uint16, error) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 3 * time.Second},
		"tcp", hostport,
		&tls.Config{InsecureSkipVerify: true}, //nolint:gosec // inspect-not-trust; loopback self-inspection
	)
	if err != nil {
		return nil, 0, 0, err
	}
	defer conn.Close()
	st := conn.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return nil, 0, 0, errors.New("server presented no certificate")
	}
	return st.PeerCertificates[0], st.Version, st.CipherSuite, nil
}

// dialServedCertFn indirects dialServedCert so tests can stub the live dial.
var dialServedCertFn = dialServedCert

// runDoctor implements `muninn doctor`: a TLS/connectivity self-describe.
func runDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	// stdlib flag does not alias short/long forms — register both.
	v := fs.Bool("v", false, "Verbose: SANs, serial, signature algorithm, TLS version/cipher, chain")
	verbose := fs.Bool("verbose", false, "Verbose: SANs, serial, signature algorithm, TLS version/cipher, chain")
	fs.Usage = func() { subcommandHelp["doctor"]() }
	_ = fs.Parse(args)

	fmt.Print(formatDoctor(gatherDoctor(), *v || *verbose, isatty()))
}

// gatherDoctor collects the daemon's connectivity and TLS state. This is the
// only function in the doctor path that performs I/O.
func gatherDoctor() doctorReport {
	r := doctorReport{}
	// running/degraded/stopped is owned by the live probe (status.go), so a
	// systemd-managed server with a stale or absent sidecar is still seen up.
	r.svcs = probeServices()

	if addrs, err := readAddrsFile(defaultDataDir()); err == nil {
		r.addrs = addrs
	}
	// Resolve the scheme BEFORE deciding to dial — resolveScheme recovers it
	// from the web-ui probe when the sidecar predates the Scheme field, else a
	// running pre-scheme-field TLS daemon would be reported as "TLS disabled".
	r.scheme = resolveScheme(r.addrs, r.svcs)

	// (a) Live socket — ground truth, what clients actually receive. Dial
	// loopback on the REST port; the recorded host may be 0.0.0.0 (not reliably
	// dialable), and may be absent entirely when the daemon is systemd-managed
	// with no sidecar — fall back to the default REST port in that case.
	if r.scheme == "https" {
		if cert, ver, suite, err := dialServedCertFn(loopbackHostPort(r.addrs.RestAddr, "8475")); err == nil {
			r.cert, r.tlsVersion, r.cipher, r.certSource = cert, ver, suite, "live socket"
		}
	}
	// (b) Cert-file fallback — env-only, certificate-only (never the key). Works
	// whether the live dial failed or the server is stopped. Surface the failure
	// reason when TLS is known-on, or when the operator explicitly pointed
	// MUNINN_TLS_CERT at a file to inspect offline (the documented stopped-server
	// case) — otherwise a corrupt/unreadable cert would render nothing at all.
	if r.cert == nil {
		if leaf, chain, err := certFromEnvFile(); err == nil {
			r.cert, r.chain, r.certSource = leaf, chain, "cert file"
		} else if r.scheme == "https" || os.Getenv("MUNINN_TLS_CERT") != "" {
			r.certErr = "could not inspect certificate (" + err.Error() + ")"
		}
	}
	return r
}

// loopbackHostPort keeps only addr's port and pins the host to loopback.
func loopbackHostPort(addr, fallbackPort string) string {
	return "127.0.0.1:" + portFromAddr(addr, fallbackPort)
}

// certFromEnvFile parses the certificate at $MUNINN_TLS_CERT. It reads only the
// certificate, never the private key. The first CERTIFICATE block is the leaf
// (PEM leaf-first convention, matching tls.LoadX509KeyPair); any further blocks
// form the chain.
func certFromEnvFile() (leaf *x509.Certificate, chain []*x509.Certificate, err error) {
	path := os.Getenv("MUNINN_TLS_CERT")
	if path == "" {
		return nil, nil, errors.New("server not reachable and MUNINN_TLS_CERT unset")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	for rest := pemBytes; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, perr := x509.ParseCertificate(block.Bytes)
		if perr != nil {
			return nil, nil, perr
		}
		if leaf == nil {
			leaf = c
		} else {
			chain = append(chain, c)
		}
	}
	if leaf == nil {
		return nil, nil, fmt.Errorf("no certificate found in %s", path)
	}
	return leaf, chain, nil
}

// formatDoctor renders a doctorReport. Pure: no I/O, no global state beyond the
// passed isTTY. verbose adds SANs, serial, signature algorithm, the negotiated
// TLS version/cipher, and the cert chain.
func formatDoctor(r doctorReport, verbose, isTTY bool) string {
	color := func(code, s string) string {
		if !isTTY {
			return s
		}
		return code + s + "\033[0m"
	}
	yellow := func(s string) string { return color("\033[33m", s) }
	red := func(s string) string { return color("\033[31m", s) }
	dim := func(s string) string { return color("\033[2m", s) }

	// mark renders a status glyph in a TTY, or a plain ASCII token otherwise, so
	// piped/log output never carries a bare Unicode glyph.
	mark := func(code, glyph, text string) string {
		if !isTTY {
			return text
		}
		return color(code, glyph)
	}
	dotGreen := mark("\033[32m", "●", "[up]")
	dotRed := mark("\033[31m", "○", "[down]")
	dotOff := mark("\033[2m", "○", "[off]")
	warnMark := mark("\033[33m", "⚠", "[warn]")

	label := func(s string) string { return fmt.Sprintf("  %-10s ", s) }
	indent := strings.Repeat(" ", len(label(""))) // align continuation rows under the value column

	var b strings.Builder
	b.WriteString("\n  muninn doctor\n\n")

	// server
	switch overallState(r.svcs) {
	case stateRunning:
		fmt.Fprintf(&b, "%s%s  running\n", label("server"), dotGreen)
	case stateDegraded:
		fmt.Fprintf(&b, "%s%s  %s\n", label("server"), warnMark, yellow("degraded"))
	case stateStopped:
		fmt.Fprintf(&b, "%s%s  stopped\n", label("server"), dotRed)
	}

	// TLS mode
	switch r.scheme {
	case "https":
		fmt.Fprintf(&b, "%s%s  enabled (https)\n", label("TLS"), dotGreen)
	case "http":
		fmt.Fprintf(&b, "%s%s  %s\n", label("TLS"), dotOff, dim("disabled (http)"))
	default:
		fmt.Fprintf(&b, "%s%s  %s\n", label("TLS"), dotOff, dim("unknown (server not running)"))
	}

	// bind addresses — show the real bound host (may be 0.0.0.0); fall back to
	// the probed port when the sidecar is absent.
	addrFor := func(name string) string {
		switch name {
		case "database":
			return r.addrs.RestAddr
		case "mcp":
			return r.addrs.MCPAddr
		case "web ui":
			return r.addrs.UIAddr
		}
		return ""
	}
	for i, s := range r.svcs {
		addr := addrFor(s.name)
		if addr == "" {
			addr = fmt.Sprintf("127.0.0.1:%d", s.port)
		}
		lead := indent
		if i == 0 {
			lead = label("bind")
		}
		fmt.Fprintf(&b, "%s%-9s %s\n", lead, s.name, addr)
	}

	// TLS disabled → no certificate section.
	if r.scheme == "http" {
		return b.String()
	}

	// certificate
	if r.cert == nil {
		if r.certErr != "" {
			fmt.Fprintf(&b, "\n  certificate  %s\n", yellow(r.certErr))
		}
		return b.String()
	}

	b.WriteString("\n")
	fmt.Fprintf(&b, "  certificate  %s\n", dim("(from "+r.certSource+")"))
	fmt.Fprintf(&b, "    subject    %s\n", certName(r.cert.Subject))
	fmt.Fprintf(&b, "    issuer     %s\n", certName(r.cert.Issuer))
	if len(r.cert.DNSNames) > 0 {
		fmt.Fprintf(&b, "    dns sans   %s\n", strings.Join(r.cert.DNSNames, ", "))
	}
	fmt.Fprintf(&b, "    valid      %s → %s (UTC)\n",
		r.cert.NotBefore.UTC().Format("2006-01-02"),
		r.cert.NotAfter.UTC().Format("2006-01-02"))

	// expiry — classified inline (no tlsutil.CheckCertExpiry: it logs via slog,
	// which would corrupt this formatted output).
	remaining := time.Until(r.cert.NotAfter)
	expMark, expMsg := dotGreen, fmt.Sprintf("%d days remaining", tlsutil.DaysRemaining(remaining))
	switch {
	case remaining <= 0:
		expMark, expMsg = dotRed, red(fmt.Sprintf("EXPIRED %d days ago", tlsutil.DaysRemaining(-remaining)))
	case remaining < tlsutil.ExpiryWarnWindow:
		expMark, expMsg = warnMark, yellow(fmt.Sprintf("expires in %d days", tlsutil.DaysRemaining(remaining)))
	}
	fmt.Fprintf(&b, "    expiry     %s  %s\n", expMark, expMsg)

	if verbose {
		if len(r.cert.IPAddresses) > 0 {
			ips := make([]string, len(r.cert.IPAddresses))
			for i, ip := range r.cert.IPAddresses {
				ips[i] = ip.String()
			}
			fmt.Fprintf(&b, "    ip sans    %s\n", strings.Join(ips, ", "))
		}
		fmt.Fprintf(&b, "    serial     %s\n", r.cert.SerialNumber)
		fmt.Fprintf(&b, "    sig alg    %s\n", r.cert.SignatureAlgorithm)
		if r.tlsVersion != 0 {
			fmt.Fprintf(&b, "    tls        %s, %s\n", tls.VersionName(r.tlsVersion), tls.CipherSuiteName(r.cipher))
		} else {
			fmt.Fprintf(&b, "    tls        %s\n", dim("n/a (cert file)"))
		}
		for i, c := range r.chain {
			fmt.Fprintf(&b, "    chain[%d]   %s\n", i, certName(c.Subject))
		}
	}

	return b.String()
}

// certName renders a certificate Subject/Issuer compactly: the CommonName when
// present, otherwise the full RFC2253 distinguished name.
func certName(n pkix.Name) string {
	if n.CommonName != "" {
		return "CN=" + n.CommonName
	}
	if s := n.String(); s != "" {
		return s
	}
	return "(none)"
}
