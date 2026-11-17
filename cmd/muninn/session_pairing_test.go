package main

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// wire stands in for the network. It is installed as the transport every CLI
// request goes through, and records, per host, what would have been sent
// there: nothing is dialled, so a host can be a machine that does not exist.
// That matters here, because the tests below are about what the CLI sends to
// ANOTHER machine, and this package's hermetic guard rightly refuses any real
// dial to one.
type wire struct {
	mu       sync.Mutex
	logins   map[string][]string // host -> usernames posted to /api/auth/login
	sessions map[string][]string // host -> muninn_session cookies received
	requests map[string]int      // host -> requests received

	// accepts is the host whose login accepts the default credentials, and
	// issues the session named session.
	accepts string
	session string
}

func newWire(t *testing.T, accepts string) *wire {
	t.Helper()
	w := &wire{
		logins: map[string][]string{}, sessions: map[string][]string{}, requests: map[string]int{},
		accepts: accepts, session: "session-issued-by-" + accepts,
	}
	old := http.DefaultTransport
	http.DefaultTransport = w
	t.Cleanup(func() { http.DefaultTransport = old })
	return w
}

func (w *wire) RoundTrip(r *http.Request) (*http.Response, error) {
	host := r.URL.Hostname()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.requests[host]++
	if c, err := r.Cookie("muninn_session"); err == nil {
		w.sessions[host] = append(w.sessions[host], c.Value)
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: r, ProtoMajor: 1, ProtoMinor: 1}
	body := ""
	switch {
	case strings.Contains(r.URL.Path, "/api/auth/login"):
		posted, _ := io.ReadAll(r.Body)
		user := "?"
		if i := strings.Index(string(posted), `"username":"`); i >= 0 {
			rest := string(posted)[i+len(`"username":"`):]
			user = rest[:strings.Index(rest, `"`)]
		}
		w.logins[host] = append(w.logins[host], user)
		if host == w.accepts && strings.Contains(string(posted), `"password":"password"`) {
			resp.Header.Add("Set-Cookie", "muninn_session="+w.session)
		} else {
			resp.StatusCode = http.StatusUnauthorized
		}
	case r.URL.Path == "/api/vaults":
		resp.Header.Set("Content-Type", "application/json")
		body = `["default"]`
	default:
		resp.StatusCode = http.StatusNotFound
	}
	resp.Body = io.NopCloser(strings.NewReader(body))
	return resp, nil
}

func (w *wire) sessionsAt(host string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.sessions[host]...)
}

func (w *wire) loginsAt(host string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.logins[host]...)
}

// useBases points the CLI at a sign-in address and an admin address as the two
// environment variables would, for one test.
func useBases(t *testing.T, adminEnv, uiEnv string) {
	t.Helper()
	oldAdmin, oldUI, oldCookie, oldHost, oldPaired := vaultAdminBase, vaultUIBase, vaultCookie, vaultCookieHost, vaultPairedHost
	vaultAdminBase, vaultUIBase, vaultPairedHost = cliBases(adminEnv, uiEnv)
	vaultCookie, vaultCookieHost = "", ""
	t.Setenv("MUNINNDB_ADMIN_URL", adminEnv)
	t.Setenv("MUNINNDB_UI_URL", uiEnv)
	t.Setenv("MUNINNDB_DATA", t.TempDir())
	t.Cleanup(func() {
		vaultAdminBase, vaultUIBase, vaultCookie, vaultCookieHost, vaultPairedHost = oldAdmin, oldUI, oldCookie, oldHost, oldPaired
	})
}

// TestASessionFromThisMachineNeverGoesToAnother: naming both addresses makes
// them one server only when they are on the same side of this machine's edge.
//
// Setting both MUNINNDB_UI_URL and MUNINNDB_ADMIN_URL was taken to mean "these
// are one server", so the session issued at the first was sent to the second.
// But the usual way to end up with both set is a console address in the
// environment and a one-off MUNINNDB_ADMIN_URL=<other server> on the command
// line. With the sign-in on this machine and the admin address on another,
// `muninn vault list` and `muninn show vaults`, given no flags, posted the
// default credentials to the local daemon and sent the admin session it
// returned to the other machine, printed that machine's short list with no
// notice, and exited 0.
//
// A session issued by a daemon on this machine is never valid on another
// machine, and no real deployment has a loopback console in front of a remote
// API, so the two are never paired.
func TestASessionFromThisMachineNeverGoesToAnother(t *testing.T) {
	const here, there = "127.0.0.1", "db.example.test"

	for name, run := range map[string]func(){
		"vault list":  func() { runVault([]string{"list"}) },
		"show vaults": runShowVaults,
	} {
		t.Run(name, func(t *testing.T) {
			w := newWire(t, here)
			useBases(t, "http://"+there+":8475", "http://"+here+":9476")

			code, out := exitStatus(t, run)

			if sent := w.sessionsAt(there); len(sent) != 0 {
				t.Errorf("the other machine was sent this machine's admin session: %q", sent)
			}
			if !strings.Contains(out, "Not signed in as admin") {
				t.Errorf("the list is the other machine's short one and the output does not say so:\n%s", out)
			}
			_ = code
		})
	}

	t.Run("the pairing rule", func(t *testing.T) {
		cases := []struct {
			name, adminEnv, uiEnv, wantPaired string
		}{
			{"console and API on two names of another deployment", "https://api.example.test", "https://console.example.test", "api.example.test"},
			{"console and API on two names of this machine", "http://localhost:8475", "http://127.0.0.1:8476", "localhost"},
			{"console here, API elsewhere", "https://api.example.test", "http://127.0.0.1:8476", ""},
			{"console elsewhere, API here", "http://127.0.0.1:8475", "https://console.example.test", ""},
			{"console here by name, API elsewhere", "http://db.example.test:8475", "http://localhost:8476", ""},
		}
		for _, tc := range cases {
			if _, _, paired := cliBases(tc.adminEnv, tc.uiEnv); paired != tc.wantPaired {
				t.Errorf("%s: paired host %q; want %q", tc.name, paired, tc.wantPaired)
			}
		}
	})
}

// TestASessionSentToAnotherHostIsAnnounced: when the operator has named a
// console and an API on two host names, the session issued by one does go to
// the other, and the CLI says so, once, on stderr. Nothing is sent to a host
// other than the issuer without a line saying it was.
func TestASessionSentToAnotherHostIsAnnounced(t *testing.T) {
	const console, api = "console.example.test", "api.example.test"
	w := newWire(t, console)
	useBases(t, "https://"+api, "https://"+console)
	resetSessionNotice()
	// Credentials are given: the default ones are not tried on another machine.
	var out string
	errOut := captureStderr(func() {
		_, out = exitStatus(t, func() { runVault([]string{"-u", "root", "--password=password", "list"}) })
	})
	if sent := w.sessionsAt(api); len(sent) == 0 {
		t.Fatalf("the API host the operator paired with the console was not sent the session:\n%s", out)
	}
	if !strings.Contains(errOut, console) || !strings.Contains(errOut, api) {
		t.Errorf("the session went to a host other than the one that issued it and stderr does not name both:\n%s", errOut)
	}
	// Once, however many requests carry it.
	again := captureStderr(func() { exitStatus(t, func() { runVaultList(nil) }) })
	if strings.Contains(again, api) {
		t.Errorf("the notice was printed a second time:\n%s", again)
	}
}
