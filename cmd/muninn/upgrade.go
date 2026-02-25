package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const githubReleaseAPI = "https://api.github.com/repos/scrypster/muninndb/releases/latest"

// latestVersionFn is the function that fetches the latest version. Tests override it.
var latestVersionFn = latestVersionDefault

// latestVersion delegates to latestVersionFn for testability.
func latestVersion() (string, error) { return latestVersionFn() }

// latestVersionDefault hits the GitHub releases API and returns the latest tag (e.g. "v1.2.3").
// Returns ("", nil) if the current version is "dev" (dev build — skip check).
// Returns ("", err) on network failure — callers should treat this as non-fatal.
func latestVersionDefault() (string, error) {
	if muninnVersion() == "dev" {
		return "", nil
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(githubReleaseAPI)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}
	return release.TagName, nil
}

// parseSemver parses "vX.Y.Z" or "X.Y.Z" into (major, minor, patch) ints.
// Handles pre-release and build metadata (e.g., "v1.2.3-alpha" or "v1.2.3+build").
// Returns false as the second value if parsing fails.
func parseSemver(v string) (major, minor, patch int, ok bool) {
	v = strings.TrimPrefix(v, "v")
	// Strip pre-release suffix (e.g., "1.2.3-alpha" → "1.2.3")
	// and build metadata (e.g., "1.2.3+build" → "1.2.3")
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err error
	if major, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, 0, false
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, 0, false
	}
	if patch, err = strconv.Atoi(parts[2]); err != nil {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}

// newerVersionAvailable returns true if latest > current (both are "vX.Y.Z").
// Returns false on any parse error to avoid false positives.
func newerVersionAvailable(current, latest string) bool {
	if current == "" || latest == "" || current == "dev" {
		return false
	}
	curMaj, curMin, curPat, ok1 := parseSemver(current)
	latMaj, latMin, latPat, ok2 := parseSemver(latest)
	if !ok1 || !ok2 {
		return false // graceful fallback on parse error
	}
	if latMaj != curMaj {
		return latMaj > curMaj
	}
	if latMin != curMin {
		return latMin > curMin
	}
	return latPat > curPat
}

// runUpgrade checks for a newer version and prints instructions to upgrade.
// If --check is passed as arg, it just checks and exits (for scripting).
func runUpgrade(args []string) {
	checkOnly := len(args) > 0 && args[0] == "--check"

	current := muninnVersion()
	fmt.Printf("  Current version: %s\n", current)
	fmt.Print("  Checking for updates... ")

	latest, err := latestVersion()
	if err != nil {
		fmt.Println("failed (no network?)")
		fmt.Fprintf(os.Stderr, "  Could not reach GitHub: %v\n", err)
		osExit(1)
		return
	}
	if latest == "" {
		fmt.Println("skipped (dev build)")
		return
	}

	if !newerVersionAvailable(current, latest) {
		fmt.Printf("up to date (%s)\n", current)
		return
	}

	fmt.Printf("update available: %s\n\n", latest)

	if checkOnly {
		osExit(1) // non-zero so scripts can detect "update available"
		return
	}

	// Print platform-appropriate upgrade instructions
	goos := runtime.GOOS
	switch goos {
	case "darwin":
		// Prefer Homebrew if it looks like they used it
		if _, err := os.Stat("/usr/local/bin/muninn"); err == nil {
			fmt.Println("  To upgrade:")
			fmt.Println()
			fmt.Println("    brew upgrade scrypster/tap/muninn")
			fmt.Println()
			fmt.Println("  or re-run the installer:")
			fmt.Println()
			fmt.Println("    curl -sSL https://muninndb.com/install.sh | sh")
		} else {
			fmt.Println("  To upgrade:")
			fmt.Println()
			fmt.Println("    curl -sSL https://muninndb.com/install.sh | sh")
		}
	case "linux":
		fmt.Println("  To upgrade:")
		fmt.Println()
		fmt.Println("    curl -sSL https://muninndb.com/install.sh | sh")
	default:
		fmt.Printf("  Download %s from:\n", latest)
		fmt.Println("    https://github.com/scrypster/muninndb/releases/latest")
	}

	fmt.Println()
	fmt.Println("  After upgrading: muninn restart")
	fmt.Println()
}
