package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// twoHosts stands in for two machines: the one this CLI would sign in to by
// default (its own, "127.0.0.1") and another one whose vault list it has been
// pointed at ("localhost": a different host name for the same loopback
// interface, which is as different as two hosts can be without leaving the
// machine). Both are listeners this test owns.
type twoHosts struct {
	mu sync.Mutex

	here, there string // base URLs

	loginsHere    int      // logins the local stand-in was sent
	cookiesThere  []string // session cookies the other host was sent
	requestsThere int
}

func newTwoHosts(t *testing.T) *twoHosts {
	t.Helper()
	h := &twoHosts{}
	here := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			h.mu.Lock()
			h.loginsHere++
			h.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "session-issued-here"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	there := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requestsThere++
		if c, err := r.Cookie("muninn_session"); err == nil {
			h.cookiesThere = append(h.cookiesThere, c.Value)
		}
		h.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/api/auth/login"):
			w.WriteHeader(http.StatusUnauthorized) // its admin password is not the default
		case r.URL.Path == "/api/vaults":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]string{"default"}) // what it tells a caller with no session
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(here.Close)
	t.Cleanup(there.Close)
	h.here = here.URL                                                 // http://127.0.0.1:<port>
	h.there = strings.Replace(there.URL, "127.0.0.1", "localhost", 1) // http://localhost:<port>
	return h
}

func (h *twoHosts) sentThere() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.cookiesThere...)
}

// TestVaultList_ASessionGoesOnlyToTheHostThatIssuedIt: the vault list comes
// from one address and the sign-in goes to another. With only the list's
// address pointed at another machine, `muninn vault list` signed in to the
// LOCAL daemon with the default credentials and sent the admin session it got
// there to the other machine, then printed that machine's short list with
// nothing to say it was short.
func TestVaultList_ASessionGoesOnlyToTheHostThatIssuedIt(t *testing.T) {
	h := newTwoHosts(t)
	oldAdmin, oldUI, oldCookie := vaultAdminBase, vaultUIBase, vaultCookie
	vaultAdminBase, vaultUIBase, vaultCookie = h.there, h.here, ""
	t.Cleanup(func() { vaultAdminBase, vaultUIBase, vaultCookie = oldAdmin, oldUI, oldCookie })

	_, out := exitStatus(t, func() { runVault([]string{"list"}) })

	if sent := h.sentThere(); len(sent) != 0 {
		t.Errorf("the other host was sent a session cookie issued by this one: %q", sent)
	}
	if !strings.Contains(out, "Not signed in as admin") || !strings.Contains(out, "sent only to the host that issued it") {
		t.Errorf("the list is the other host's short one and the output does not say so, or why:\n%s", out)
	}
}

// TestShowVaults_ASessionGoesOnlyToTheHostThatIssuedIt is the same for
// `muninn show vaults`, whose list address is MUNINNDB_ADMIN_URL.
func TestShowVaults_ASessionGoesOnlyToTheHostThatIssuedIt(t *testing.T) {
	h := newTwoHosts(t)
	t.Setenv("MUNINNDB_ADMIN_URL", h.there)
	t.Setenv("MUNINNDB_DATA", t.TempDir()) // not this machine's own data directory
	oldUI, oldCookie := vaultUIBase, vaultCookie
	vaultUIBase, vaultCookie = h.here, ""
	t.Cleanup(func() { vaultUIBase, vaultCookie = oldUI, oldCookie })

	_, out := exitStatus(t, runShowVaults)

	if sent := h.sentThere(); len(sent) != 0 {
		t.Errorf("the other host was sent a session cookie issued by this one: %q", sent)
	}
	if !strings.Contains(out, "Not signed in as admin") || !strings.Contains(out, "sent only to the host that issued it") {
		t.Errorf("the list is the other host's short one and the output does not say so, or why:\n%s", out)
	}
}

// TestShell_ASessionGoesOnlyToTheHostThatIssuedIt: the shell signs in at one
// address and lists vaults at another, and holds the session of the first.
func TestShell_ASessionGoesOnlyToTheHostThatIssuedIt(t *testing.T) {
	h := newTwoHosts(t)
	t.Setenv("MUNINNDB_ADMIN_URL", h.there)
	t.Setenv("MUNINNDB_DATA", t.TempDir())

	r := &replState{sessionCookie: "session-issued-here", sessionHosts: []string{hostOf(h.here)}}
	captureStdout(func() { r.cmdShowVaults() })
	if sent := h.sentThere(); len(sent) != 0 {
		t.Errorf("the other host was sent a session cookie issued by this one: %q", sent)
	}

	// The host that issued it does get it, on whatever port its API is.
	t.Setenv("MUNINNDB_ADMIN_URL", h.here)
	if u := mustParseURL(t, h.here+"/api/vaults"); sessionCookieFor(u, r.sessionCookie, r.sessionHosts...) == "" {
		t.Errorf("the session is not sent back to the host that issued it")
	}
}

// TestSessionCookieFor is the rule itself.
func TestSessionCookieFor(t *testing.T) {
	const cookie = "session"
	cases := []struct {
		name, request string
		hosts         []string
		sent          bool
	}{
		{"the issuing host, on its API port", "http://127.0.0.1:8475/api/vaults", []string{"127.0.0.1"}, true},
		{"the issuing host by name, whatever the case", "https://DB.example.test:9475/api/vaults", []string{"db.example.test"}, true},
		{"another host", "http://db.example.test:8475/api/vaults", []string{"127.0.0.1"}, false},
		{"another name for the same machine is another host", "http://localhost:8475/api/vaults", []string{"127.0.0.1"}, false},
		{"a session with no known issuer goes nowhere", "http://127.0.0.1:8475/api/vaults", []string{""}, false},
		{"no hosts at all", "http://127.0.0.1:8475/api/vaults", nil, false},
		{"an API host the operator paired with the console's", "https://api.example.test/api/vaults", []string{"console.example.test", "api.example.test"}, true},
		{"a third host is still a third host", "https://other.example.test/api/vaults", []string{"console.example.test", "api.example.test"}, false},
		{"an empty pairing does not match a request with no host", "/api/vaults", []string{"127.0.0.1", ""}, false},
	}
	for _, tc := range cases {
		got := sessionCookieFor(mustParseURL(t, tc.request), cookie, tc.hosts...) != ""
		if got != tc.sent {
			t.Errorf("%s: sent = %v; want %v", tc.name, got, tc.sent)
		}
	}
	if sessionCookieFor(mustParseURL(t, "http://127.0.0.1:8475/"), "", "127.0.0.1") != "" {
		t.Errorf("no session, yet something was sent")
	}
}

// TestSignInAddressFollowsTheAdminAddress: with only MUNINNDB_ADMIN_URL set,
// the CLI signs in to the server it was pointed at. It used to sign in to this
// machine's own daemon whatever the admin URL was. And two addresses count as
// one server, for the purpose of sending a session from one to the other, only
// when the operator gave both.
func TestSignInAddressFollowsTheAdminAddress(t *testing.T) {
	cases := []struct {
		name, adminEnv, uiEnv         string
		wantAdmin, wantUI, wantPaired string
	}{
		{"neither set: this machine", "", "", "http://127.0.0.1:8475", "http://127.0.0.1:8476", ""},
		{"admin URL with a port: the port after it", "https://db.example.test:9475", "", "https://db.example.test:9475", "https://db.example.test:9476", ""},
		{"admin URL with no port: the default UI port", "https://db.example.test", "", "https://db.example.test", "https://db.example.test:8476", ""},
		{"both set: as given, and paired", "https://api.example.test", "https://console.example.test", "https://api.example.test", "https://console.example.test", "api.example.test"},
		{"only the UI URL set: nothing is paired with the local API", "", "https://console.example.test", "http://127.0.0.1:8475", "https://console.example.test", ""},
		{"an IPv6 host", "http://[::1]:8475", "", "http://[::1]:8475", "http://[::1]:8476", ""},
	}
	for _, tc := range cases {
		admin, ui, paired := cliBases(tc.adminEnv, tc.uiEnv)
		if admin != tc.wantAdmin || ui != tc.wantUI || paired != tc.wantPaired {
			t.Errorf("%s: admin %q, sign-in %q, paired %q; want %q, %q, %q", tc.name, admin, ui, paired, tc.wantAdmin, tc.wantUI, tc.wantPaired)
		}
	}
}

// TestDefaultCredentialsStayOnThisMachine: the default admin credentials are
// tried without being asked only at a loopback address. Another machine is
// sent no credential the user did not supply.
func TestDefaultCredentialsStayOnThisMachine(t *testing.T) {
	for base, want := range map[string]bool{
		"http://127.0.0.1:8476":        true,
		"https://localhost:8476":       true,
		"http://[::1]:8476":            true,
		"https://db.example.test:8476": false,
		"http://192.0.2.10:8476":       false,
		"not a url":                    false,
	} {
		if got := defaultCredentialsAllowed(base); got != want {
			t.Errorf("defaultCredentialsAllowed(%q) = %v; want %v", base, got, want)
		}
	}

	// And the commands obey it. The stand-in below is a listener this test
	// owns, addressed as 0.0.0.0, which is not a loopback address by the rule
	// and yet never leaves this machine: if the rule were broken the login
	// would arrive here and be counted, not go anywhere else.
	logins := 0
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			logins++
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer elsewhere.Close()
	base := strings.Replace(elsewhere.URL, "127.0.0.1", "0.0.0.0", 1)
	if defaultCredentialsAllowed(base) {
		t.Fatalf("fixture: %s counts as this machine", base)
	}

	oldUI, oldCookie := vaultUIBase, vaultCookie
	vaultUIBase, vaultCookie = base, ""
	t.Cleanup(func() { vaultUIBase, vaultCookie = oldUI, oldCookie })
	if got := signInForVaultList("root", "", false); got != signInNotAttempted {
		t.Errorf("signInForVaultList against another machine with no credentials = %v; want signInNotAttempted", got)
	}
	if logins != 0 {
		t.Errorf("the listing command sent %d logins to another machine without being given credentials", logins)
	}

	// The shell asks for credentials there; it does not try the default ones.
	asked, loginsWhenAsked := false, -1
	_, _ = replSignIn(base, func() (string, string, error) {
		asked, loginsWhenAsked = true, logins
		return "", "", errors.New("stop here")
	})
	if !asked || loginsWhenAsked != 0 {
		t.Errorf("the shell asked = %v after sending %d logins; want it to ask before sending anything", asked, loginsWhenAsked)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
