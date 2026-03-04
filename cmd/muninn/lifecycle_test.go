package main

import (
	"os"
	"strings"
	"testing"
)

func TestBuildDaemonArgs(t *testing.T) {
	cases := []struct {
		name           string
		dataDir        string
		dev            bool
		mcpToken       string
		osArgs         []string
		listenHostEnv  string
		corsOriginsEnv string
		wantContains   []string
		wantAbsent     []string
	}{
		{
			name:    "default listen-host not forwarded",
			dataDir: "/tmp/data",
			osArgs:  []string{},
			wantAbsent: []string{"--listen-host"},
		},
		{
			name:    "non-default listen-host forwarded",
			dataDir: "/tmp/data",
			osArgs:  []string{"--listen-host", "0.0.0.0"},
			wantContains: []string{"--listen-host", "0.0.0.0"},
		},
		{
			name:    "cors-origins flag in osArgs forwarded",
			dataDir: "/tmp/data",
			osArgs:  []string{"--cors-origins", "http://flag.local"},
			wantContains: []string{"--cors-origins", "http://flag.local"},
		},
		{
			name:           "cors-origins from env when no flag",
			dataDir:        "/tmp/data",
			osArgs:         []string{},
			corsOriginsEnv: "http://env.local",
			wantContains:   []string{"--cors-origins", "http://env.local"},
		},
		{
			name:           "flag wins over env for cors-origins",
			dataDir:        "/tmp/data",
			osArgs:         []string{"--cors-origins", "http://flag.local"},
			corsOriginsEnv: "http://env.local",
			wantContains:   []string{"--cors-origins", "http://flag.local"},
			wantAbsent:     []string{"http://env.local"},
		},
		{
			name:         "neither cors flag nor env not forwarded",
			dataDir:      "/tmp/data",
			osArgs:       []string{},
			wantAbsent:   []string{"--cors-origins"},
		},
		{
			name:         "dev true forwarded",
			dataDir:      "/tmp/data",
			dev:          true,
			osArgs:       []string{},
			wantContains: []string{"--dev"},
		},
		{
			name:         "mcpToken set forwarded",
			dataDir:      "/tmp/data",
			mcpToken:     "tok123",
			osArgs:       []string{},
			wantContains: []string{"--mcp-token", "tok123"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildDaemonArgs(tc.dataDir, tc.dev, tc.mcpToken, tc.osArgs, tc.listenHostEnv, tc.corsOriginsEnv)

			for _, want := range tc.wantContains {
				found := false
				for _, arg := range got {
					if arg == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected %q in args %v", want, got)
				}
			}

			for _, absent := range tc.wantAbsent {
				for _, arg := range got {
					if arg == absent {
						t.Errorf("expected %q to be absent from args %v", absent, got)
						break
					}
				}
			}
		})
	}
}

// TestIsProcessRunningCurrentProcess checks if the current process is identified as running.
func TestIsProcessRunningCurrentProcess(t *testing.T) {
	pid := os.Getpid()
	if !isProcessRunning(pid) {
		t.Errorf("current process (pid %d) should be running", pid)
	}
}

// TestIsProcessRunningDeadProcess checks if a non-existent PID is correctly identified as not running.
func TestIsProcessRunningDeadProcess(t *testing.T) {
	// PID 99999999 almost certainly doesn't exist
	if isProcessRunning(99999999) {
		t.Error("pid 99999999 should not be running")
	}
}

// TestIsProcessRunningNegativePID checks that negative PIDs are handled gracefully.
func TestIsProcessRunningNegativePID(t *testing.T) {
	// Negative PID — should not panic, should return false
	if isProcessRunning(-1) {
		t.Error("negative pid should not be running")
	}
}

// TestIsProcessRunningZeroPID checks that PID 0 is handled correctly.
func TestIsProcessRunningZeroPID(t *testing.T) {
	// PID 0 is special — should return false
	if isProcessRunning(0) {
		t.Error("pid 0 should not be running")
	}
}

// TestDefaultDataDir checks that defaultDataDir returns a valid path under the home directory.
func TestDefaultDataDir(t *testing.T) {
	dir := defaultDataDir()
	if dir == "" {
		t.Error("defaultDataDir returned empty string")
	}
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(dir, home) {
		t.Errorf("defaultDataDir %q should be under home %q", dir, home)
	}
	if !strings.HasSuffix(dir, "data") {
		t.Errorf("defaultDataDir %q should end with 'data'", dir)
	}
}

// TestDefaultDataDirEnvOverride checks that MUNINNDB_DATA environment variable is respected.
func TestDefaultDataDirEnvOverride(t *testing.T) {
	oldVal := os.Getenv("MUNINNDB_DATA")
	defer os.Setenv("MUNINNDB_DATA", oldVal)

	testDir := "/tmp/test-muninn-data"
	os.Setenv("MUNINNDB_DATA", testDir)

	dir := defaultDataDir()
	if dir != testDir {
		t.Errorf("defaultDataDir = %q, want %q (from MUNINNDB_DATA)", dir, testDir)
	}
}
