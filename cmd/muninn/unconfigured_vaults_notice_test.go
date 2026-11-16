package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// captureLog swaps the default logger for one writing to a buffer at DEBUG and
// returns the buffer's contents after fn. Not safe with t.Parallel.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestUnconfiguredVaultsNotice_OneLineNotOnePerVault(t *testing.T) {
	vaults := []string{"orchard", "ferry", "lantern", "quarry"}
	out := captureLog(t, func() {
		logUnconfiguredVaults(
			func() ([]string, error) { return vaults, nil },
			func([]string) ([]string, error) { return []string{"ferry", "lantern", "quarry"}, nil },
		)
	})
	got := lines(out)
	if len(got) != 1 {
		t.Fatalf("want exactly one line for three unconfigured vaults, got %d:\n%s", len(got), out)
	}
	if !strings.Contains(got[0], "level=INFO") {
		t.Errorf("a vault locked by default is the secure state, not a fault: want INFO, got %q", got[0])
	}
	for _, want := range []string{"unconfigured=3", "of=4", "ferry, lantern, quarry", "names_not_shown=0", "require an API key"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("notice is missing %q: %q", want, got[0])
		}
	}
}

func TestUnconfiguredVaultsNotice_BoundsTheNames(t *testing.T) {
	var locked []string
	for i := 0; i < maxUnconfiguredVaultsShown+7; i++ {
		locked = append(locked, fmt.Sprintf("vault-%02d", i))
	}
	out := captureLog(t, func() {
		logUnconfiguredVaults(
			func() ([]string, error) { return locked, nil },
			func([]string) ([]string, error) { return locked, nil },
		)
	})
	got := lines(out)
	if len(got) != 1 {
		t.Fatalf("want one line, got %d", len(got))
	}
	if !strings.Contains(got[0], fmt.Sprintf("unconfigured=%d", len(locked))) || !strings.Contains(got[0], "names_not_shown=7") {
		t.Errorf("the count must be the true count and the remainder must be stated: %q", got[0])
	}
	if strings.Contains(got[0], fmt.Sprintf("vault-%02d", maxUnconfiguredVaultsShown)) {
		t.Errorf("names past the bound must not be printed: %q", got[0])
	}
}

func TestUnconfiguredVaultsNotice_NoneUnconfigured(t *testing.T) {
	out := captureLog(t, func() {
		logUnconfiguredVaults(
			func() ([]string, error) { return []string{"orchard"}, nil },
			func([]string) ([]string, error) { return nil, nil },
		)
	})
	got := lines(out)
	if len(got) != 1 || !strings.Contains(got[0], "level=INFO") || !strings.Contains(got[0], "every vault on this server has an explicit access policy") {
		t.Errorf("want one INFO line saying nothing is unconfigured, got:\n%s", out)
	}
}

// When the list cannot be built the operator has been told nothing about which
// vaults are locked. That is the one case that is a warning.
func TestUnconfiguredVaultsNotice_FailureIsAWarning(t *testing.T) {
	boom := errors.New("read fault")
	for name, run := range map[string]func(){
		"listing vaults fails": func() {
			logUnconfiguredVaults(func() ([]string, error) { return nil, boom }, func([]string) ([]string, error) { return nil, nil })
		},
		"reading configs fails": func() {
			logUnconfiguredVaults(func() ([]string, error) { return []string{"orchard"}, nil }, func([]string) ([]string, error) { return nil, boom })
		},
	} {
		got := lines(captureLog(t, run))
		if len(got) != 1 || !strings.Contains(got[0], "level=WARN") || !strings.Contains(got[0], "read fault") || !strings.Contains(got[0], "require an API key") {
			t.Errorf("%s: want one WARN line carrying the error and the fail-closed rule, got %v", name, got)
		}
	}
}
