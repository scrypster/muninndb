package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

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
	name string
	port int
	up   bool
	note string // optional: "not responding"
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

// probeServicesDefault hits all health endpoints and returns statuses.
func probeServicesDefault() []serviceStatus {
	client := &http.Client{Timeout: 2 * time.Second}
	probe := func(url string) bool {
		resp, err := client.Get(url)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}

	return []serviceStatus{
		{name: "database", port: 8475, up: probe("http://127.0.0.1:8475/api/health")},
		{name: "mcp", port: 8750, up: probe("http://127.0.0.1:8750/mcp/health")},
		{name: "web ui", port: 8476, up: probe("http://127.0.0.1:8476/")},
	}
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
		for _, s := range svcs {
			if !s.up {
				fmt.Printf("  %s is not responding", s.name)
				if s.name == "mcp" {
					fmt.Print(" — your AI tools won't have memory access")
				}
				fmt.Println(".")
			}
		}
		fmt.Println("  Run: muninn restart")
	}

	if !compact {
		if state == stateStopped {
			fmt.Println()
			fmt.Println("  muninn start  →  start all services")
			fmt.Println("  muninn help   →  see all commands")
		}
		if state == stateRunning {
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
