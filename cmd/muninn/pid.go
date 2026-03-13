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
	RestAddr string `json:"rest_addr"`
	MCPAddr  string `json:"mcp_addr"`
	UIAddr   string `json:"ui_addr"`
}

const addrsFileName = "muninn.addrs"

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
		return 0, fmt.Errorf("no PID file at %s (is muninn running?): %w", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("invalid PID file: %w", err)
	}
	return pid, nil
}
