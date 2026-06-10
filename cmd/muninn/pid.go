package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// daemonAddrs records the actual addresses the daemon bound to.
// Written to muninn.addrs in the data directory so 'muninn status' and
// the startup health poll can probe the correct ports when non-default
// --*-addr flags are used.
type daemonAddrs struct {
	// Scheme is "http" or "https" — the scheme the daemon's client-facing
	// listeners serve. An empty Scheme (e.g. a sidecar written by an older
	// daemon that predates this field) is treated as "http" by readers.
	Scheme   string `json:"scheme"`
	RestAddr string `json:"rest_addr"`
	MCPAddr  string `json:"mcp_addr"`
	UIAddr   string `json:"ui_addr"`
}

const addrsFileName = "muninn.addrs"

// schemeFor reports the scheme the daemon's client-facing listeners serve:
// "https" when both a TLS cert and key are configured, "http" otherwise.
func schemeFor(tlsCert, tlsKey string) string {
	if tlsCert != "" && tlsKey != "" {
		return "https"
	}
	return "http"
}

// localScheme reports the scheme the local daemon is serving ("https"/"http"),
// read from the muninn.addrs sidecar. Defaults to "http" when the sidecar is
// absent (an older or stopped daemon) — matching the readers' empty-Scheme rule.
func localScheme() string {
	if addrs, err := readAddrsFile(defaultDataDir()); err == nil && addrs.Scheme != "" {
		return addrs.Scheme
	}
	return "http"
}

func writeAddrsFile(dataDir string, addrs daemonAddrs) error {
	b, err := json.Marshal(addrs)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, addrsFileName), b, 0600)
}

func readAddrsFile(dataDir string) (daemonAddrs, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, addrsFileName))
	if err != nil {
		return daemonAddrs{}, err
	}
	var a daemonAddrs
	if err := json.Unmarshal(b, &a); err != nil {
		return daemonAddrs{}, err
	}
	return a, nil
}

func writePID(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0600)
}

func readPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf(
			"no PID file at %s — muninn may not be running (try 'muninn start'),\n"+
				"or it may be managed by systemd (try 'systemctl status muninndb'): %w",
			path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("invalid PID file: %w", err)
	}
	return pid, nil
}
