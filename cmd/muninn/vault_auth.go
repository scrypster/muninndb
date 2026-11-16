package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// Package-level session state set by runVault before dispatching subcommands.
// Tests don't touch these, so doVaultRequestForce and friends work unchanged.
var (
	vaultAdminBase = "http://127.0.0.1:8475" // REST API
	vaultUIBase    = "http://127.0.0.1:8476" // login endpoint lives here
	vaultCookie    string                    // muninn_session value
	// vaultCookieHost is the host that issued vaultCookie: where the login
	// that returned it was sent. The cookie goes back to that host
	// (sessionCookieFor).
	vaultCookieHost string
	// vaultPairedHost is the admin API host that is taken to be the same
	// server as the sign-in address although its name differs. It is set only
	// when BOTH MUNINNDB_ADMIN_URL and MUNINNDB_UI_URL are given AND the two
	// are on the same side of this machine's edge (cliBases), and is ""
	// otherwise. A server behind a proxy can have its API and its console on
	// two host names; the session issued on one is then sent to the other, and
	// the CLI says so when it does (announceSessionSentElsewhere). An address
	// the CLI fell back to on its own is never paired with one the operator
	// gave, and an address on this machine is never paired with one that is
	// not.
	vaultPairedHost string
)

func init() {
	// MUNINNDB_ADMIN_URL / MUNINNDB_UI_URL allow persistent TLS configuration
	// without passing -h on every invocation. A -h flag always takes precedence.
	vaultAdminBase, vaultUIBase, vaultPairedHost = cliBases(os.Getenv("MUNINNDB_ADMIN_URL"), os.Getenv("MUNINNDB_UI_URL"))
}

// cliBases returns where the CLI sends admin API requests and where it signs
// in, from the two environment variables.
//
// When only the admin URL is given, the sign-in address is DERIVED from it
// (uiBaseFor), as -h derives it (setHostPorts). It used to stay at this
// machine's own 127.0.0.1:8476 whatever the admin URL was, so a CLI pointed at
// another server signed in to the local one and carried that session to the
// other.
//
// pairedHost is the admin host when BOTH were given and both are on this
// machine or both are not, and "" otherwise: see vaultPairedHost.
//
// Giving both is not enough on its own. It was, and that reproduced the leak
// the derivation above closes: a console address left in the environment and a
// one-off MUNINNDB_ADMIN_URL for another server meant "sign in here, and send
// the session there". A session issued by a daemon on this machine is never
// valid on another machine, and no real deployment has a loopback console in
// front of a remote API (or the reverse), so those two are not one server
// whatever the variables say.
func cliBases(adminEnv, uiEnv string) (admin, ui, pairedHost string) {
	admin, ui = "http://127.0.0.1:8475", "http://127.0.0.1:8476"
	if adminEnv != "" {
		admin = adminEnv
		ui = uiBaseFor(adminEnv)
	}
	if uiEnv != "" {
		ui = uiEnv
	}
	if adminEnv != "" && uiEnv != "" && isLoopbackURL(adminEnv) == isLoopbackURL(uiEnv) {
		pairedHost = hostOf(adminEnv)
	}
	return admin, ui, pairedHost
}

// uiBaseFor is the sign-in address for the server whose admin API is at
// adminURL: the same scheme and host, on the port after the API's, which is
// the server's default layout (8475 and 8476) and what -h assumes. An admin
// URL with no port gets 8476. Set MUNINNDB_UI_URL when the server is laid out
// differently.
func uiBaseFor(adminURL string) string {
	u, err := url.Parse(adminURL)
	if err != nil || u.Hostname() == "" {
		return "http://127.0.0.1:8476"
	}
	port := 8476
	if n, err := strconv.Atoi(u.Port()); err == nil {
		port = n + 1
	}
	return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
}

// hostOf returns the host name of a base URL, lower-cased, or "".
func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// sessionCookieFor returns the admin session cookie to send with a request to
// u: the cookie when u is on one of the hosts it may go to, and "" for any
// other host. A session is a credential for the server that issued it. Sent
// elsewhere it authenticates nothing and is handed to whoever is listening
// there.
//
// hosts are the host that issued the session and, when the operator configured
// one, the API host paired with it. The comparison is by host name, as a
// browser's is: the server issues the session on its UI port and accepts it on
// its API port. An empty host matches nothing.
func sessionCookieFor(u *url.URL, cookie string, hosts ...string) string {
	if cookie == "" || u == nil {
		return ""
	}
	to := strings.ToLower(u.Hostname())
	for _, h := range hosts {
		if h != "" && h == to {
			return cookie
		}
	}
	return ""
}

// sessionSentElsewhere records that the notice below has been printed.
var sessionSentElsewhere atomic.Bool

// announceSessionSentElsewhere says, once per run and on stderr, that an admin
// session is being sent to a host other than the one that issued it. That
// happens only for a host the operator paired with the sign-in address, and it
// is never done silently: a session is a credential, and the person running
// the command should be able to see where it went.
func announceSessionSentElsewhere(u *url.URL, issuerHost string) {
	if u == nil || issuerHost == "" || strings.ToLower(u.Hostname()) == issuerHost {
		return
	}
	if sessionSentElsewhere.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "Note: the admin session issued by %s is being sent to %s, because MUNINNDB_UI_URL and MUNINNDB_ADMIN_URL name them together.\n",
			issuerHost, strings.ToLower(u.Hostname()))
	}
}

// resetSessionNotice lets the next session sent to a host other than its
// issuer be announced again. Tests only.
func resetSessionNotice() { sessionSentElsewhere.Store(false) }

// sessionIssuer is the host vaultCookie came from. loginAdmin records it. A
// cookie with no recorded host can only have come from a login at vaultUIBase,
// which is the one place the CLI signs in.
func sessionIssuer() string {
	if vaultCookieHost != "" {
		return vaultCookieHost
	}
	return hostOf(vaultUIBase)
}

// signedInTo reports whether the CLI holds an admin session that the server at
// base will be sent.
func signedInTo(base string) bool {
	u, err := url.Parse(base)
	return err == nil && sessionCookieFor(u, vaultCookie, sessionIssuer(), vaultPairedHost) != ""
}

// defaultCredentialsAllowed reports whether the CLI may try the default admin
// credentials at a sign-in address without being asked to: only on this
// machine. A fresh local install has them and should work without flags.
// Another machine is sent no credential the user did not supply.
func defaultCredentialsAllowed(uiBase string) bool {
	return isLoopbackURL(uiBase)
}

// parseAdminFlags extracts MySQL-style auth flags (-u, -p, -h) from args and
// returns the remaining (non-auth) args. Sets package-level vaultAdminBase,
// vaultUIBase, and triggers authentication.
//
// Supported flags:
//
//	-u <user>         admin username (default: root)
//	-p                prompt for password
//	-p<password>      inline password (no space, like MySQL)
//	--password=<pw>   inline password
//	-h <host:port>    UI host:port (default: 127.0.0.1:8476)
func parseAdminFlags(args []string) (remaining []string, username, password string, prompted bool) {
	username = "root"

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--user":
			if i+1 < len(args) {
				i++
				username = args[i]
			}
		case strings.HasPrefix(a, "--user="):
			username = strings.TrimPrefix(a, "--user=")

		case a == "-p":
			prompted = true
		case strings.HasPrefix(a, "-p") && len(a) > 2 && a[2] != '-':
			password = a[2:]
		case strings.HasPrefix(a, "--password="):
			password = strings.TrimPrefix(a, "--password=")

		case a == "-h" || a == "--host":
			if i+1 < len(args) {
				i++
				setHostPorts(args[i])
			}
		case strings.HasPrefix(a, "--host="):
			setHostPorts(strings.TrimPrefix(a, "--host="))

		default:
			remaining = append(remaining, a)
		}
	}
	return
}

// setHostPorts updates vaultAdminBase and vaultUIBase from the -h flag value.
//
// Accepted forms (all backwards-compatible):
//
//	host                     → http://host:8475  / http://host:8476
//	host:port                → http://host:port  / http://host:port+1
//	http://host[:port]       → same, explicit http scheme
//	https://host[:port]      → same, explicit https scheme
//
// When a full URL with scheme is given the scheme is preserved.
// Precedence: -h flag > MUNINNDB_ADMIN_URL env var > compiled-in default.
func setHostPorts(hostPort string) {
	vaultPairedHost = "" // -h names one host, and both addresses are on it
	scheme := "http"
	if strings.HasPrefix(hostPort, "https://") {
		scheme = "https"
		hostPort = strings.TrimPrefix(hostPort, "https://")
	} else if strings.HasPrefix(hostPort, "http://") {
		hostPort = strings.TrimPrefix(hostPort, "http://")
	}

	if !strings.Contains(hostPort, ":") {
		vaultAdminBase = scheme + "://" + hostPort + ":8475"
		vaultUIBase = scheme + "://" + hostPort + ":8476"
		return
	}
	parts := strings.SplitN(hostPort, ":", 2)
	vaultAdminBase = scheme + "://" + hostPort
	// Derive UI port: attempt port+1, fallback to same host with :8476.
	var port int
	if _, err := fmt.Sscanf(parts[1], "%d", &port); err == nil {
		vaultUIBase = fmt.Sprintf("%s://%s:%d", scheme, parts[0], port+1)
	} else {
		vaultUIBase = scheme + "://" + parts[0] + ":8476"
	}
}

// authenticateAdmin obtains a session cookie. Priority:
//  1. If password was provided explicitly, use it.
//  2. On this machine only, try auto-auth with default credentials
//     (root/password).
//  3. If -p flag was given (no inline password), prompt interactively.
//  4. If auto-auth failed or was not tried and no -p flag, prompt
//     interactively.
func authenticateAdmin(username, password string, prompted bool) error {
	if password != "" {
		return loginAdmin(username, password)
	}

	// Try default credentials first, on this machine only. Another machine is
	// sent no credential the user did not supply (defaultCredentialsAllowed):
	// the one rule for every command.
	if defaultCredentialsAllowed(vaultUIBase) {
		if err := loginAdmin("root", "password"); err == nil {
			return nil
		}
	}

	// Default creds failed — need explicit password.
	if !prompted {
		fmt.Printf("Authentication required. Use -u <user> -p to provide credentials.\n")
		fmt.Printf("Attempting interactive login as %q...\n", username)
	}

	fmt.Printf("Password for %s: ", username)
	passBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	return loginAdmin(username, string(passBytes))
}

// loginAdmin POSTs to the UI login endpoint and captures the session cookie.
func loginAdmin(username, password string) error {
	body, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	loginURL := vaultUIBase + "/api/auth/login"
	client := httpClientForURL(loginURL, 5*time.Second)
	resp, err := client.Post(loginURL, "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("connect to MuninnDB: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("authentication failed (HTTP %d)", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "muninn_session" || c.Name == "session" {
			vaultCookie = c.Value
			vaultCookieHost = hostOf(vaultUIBase)
			return nil
		}
	}
	return nil
}

// vaultSignIn is how an attempt to sign in for a vault listing ended.
type vaultSignIn int

const (
	signedIn              vaultSignIn = iota // there is an admin session
	signInNotAttempted                       // no credentials were given and the defaults are not sent to another machine
	signInDefaultsRefused                    // no credentials were given and the server does not have the default ones
	signInRefused                            // the credentials the user gave were refused, or the server was not reached
	signInElsewhere                          // there is a session, issued by a host other than the one being listed
)

// signInForVaultList gets an admin session for a vault listing when it can do
// so without interrupting: with a password given on the command line, by
// prompting when -p asked for a prompt, and otherwise, on this machine only,
// with the default credentials.
func signInForVaultList(username, password string, prompted bool) vaultSignIn {
	switch {
	case password != "" || prompted:
		if err := authenticateAdmin(username, password, prompted); err != nil {
			fmt.Fprintf(os.Stderr, "Authentication failed: %v\n", err)
			return signInRefused
		}
	case defaultCredentialsAllowed(vaultUIBase):
		if err := loginAdmin("root", "password"); err != nil {
			return signInDefaultsRefused
		}
	default:
		return signInNotAttempted
	}
	if vaultCookie == "" {
		return signInDefaultsRefused
	}
	return signedIn
}

// getWithAdminSession sends a GET carrying the admin session cookie, if one
// has been obtained.
func getWithAdminSession(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	addSessionCookie(req)
	return httpClientForURL(url, 0).Do(req)
}

// addSessionCookie attaches the admin session cookie to an HTTP request
// if one has been obtained.
func addSessionCookie(req *http.Request) {
	if cookie := sessionCookieFor(req.URL, vaultCookie, sessionIssuer(), vaultPairedHost); cookie != "" {
		announceSessionSentElsewhere(req.URL, sessionIssuer())
		req.AddCookie(&http.Cookie{Name: "muninn_session", Value: cookie})
	}
}
