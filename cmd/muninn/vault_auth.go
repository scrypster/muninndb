package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// Package-level session state set by runVault before dispatching subcommands.
// Tests don't touch these, so doVaultRequestForce and friends work unchanged.
var (
	vaultAdminBase = "http://127.0.0.1:8475" // REST API
	vaultUIBase    = "http://127.0.0.1:8476" // login endpoint lives here
	vaultCookie    string                   // muninn_session value
)

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

// setHostPorts updates vaultAdminBase and vaultUIBase from a host or host:port.
// If only a host is given (no port), the defaults (:8475/:8476) are used.
// If host:port is given, the UI port is assumed to be port+1.
func setHostPorts(hostPort string) {
	if !strings.Contains(hostPort, ":") {
		vaultAdminBase = "http://" + hostPort + ":8475"
		vaultUIBase = "http://" + hostPort + ":8476"
		return
	}
	parts := strings.SplitN(hostPort, ":", 2)
	vaultAdminBase = "http://" + hostPort
	// Derive UI port: attempt port+1, fallback to same host with :8476.
	var port int
	if _, err := fmt.Sscanf(parts[1], "%d", &port); err == nil {
		vaultUIBase = fmt.Sprintf("http://%s:%d", parts[0], port+1)
	} else {
		vaultUIBase = "http://" + parts[0] + ":8476"
	}
}

// authenticateAdmin obtains a session cookie. Priority:
//  1. If password was provided explicitly, use it.
//  2. Try auto-auth with default credentials (root/password).
//  3. If -p flag was given (no inline password), prompt interactively.
//  4. If auto-auth failed and no -p flag, prompt interactively.
func authenticateAdmin(username, password string, prompted bool) error {
	if password != "" {
		return loginAdmin(username, password)
	}

	// Try default credentials first.
	if err := loginAdmin("root", "password"); err == nil {
		return nil
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
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(vaultUIBase+"/api/auth/login", "application/json",
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
			return nil
		}
	}
	return nil
}

// addSessionCookie attaches the admin session cookie to an HTTP request
// if one has been obtained.
func addSessionCookie(req *http.Request) {
	if vaultCookie != "" {
		req.AddCookie(&http.Cookie{Name: "muninn_session", Value: vaultCookie})
	}
}
