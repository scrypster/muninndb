package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

// runUpgrade is the entry point for `muninn upgrade`.
// Flags:
//
//	--check   Check only; exit 1 if update available (for scripting).
//	--yes/-y  Skip confirmation prompt (non-interactive upgrade).
func runUpgrade(args []string) {
	checkOnly := false
	skipConfirm := false
	for _, a := range args {
		if a == "--check" {
			checkOnly = true
		}
		if a == "--yes" || a == "-y" {
			skipConfirm = true
		}
	}

	current := muninnVersion()

	// Banner
	fmt.Println()
	fmt.Println("  ┌────────────────────────────────────────────────────┐")
	fmt.Println("  │                                                    │")
	fmt.Printf("  │   muninn  ·  cognitive memory database  %-9s│\n", current)
	fmt.Println("  │                                                    │")
	fmt.Println("  └────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Printf("  Current version: %s\n", current)

	fmt.Print("  Checking for updates...")

	latest, err := latestVersion()
	if err != nil {
		fmt.Println(" failed")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintf(os.Stderr, "  Could not reach GitHub: %v\n", err)
		fmt.Fprintln(os.Stderr, "  Check your connection and try again.")
		fmt.Fprintln(os.Stderr, "")
		return
	}
	if latest == "" {
		fmt.Println(" skipped")
		fmt.Println()
		fmt.Println("  Dev build — version checks are disabled.")
		fmt.Println()
		return
	}

	if !newerVersionAvailable(current, latest) {
		fmt.Printf(" done\n")
		fmt.Println()
		fmt.Printf("  You're up to date (%s).\n", current)
		fmt.Println()
		return
	}

	// Update available
	fmt.Println("  done")
	fmt.Println()
	fmt.Println("  ✦  Update available")
	fmt.Println()
	fmt.Printf("     %s  →  %s\n", current, latest)
	fmt.Println()
	fmt.Printf("  Release notes → github.com/scrypster/muninndb/releases/tag/%s\n", latest)
	fmt.Println()
	fmt.Println("  ────────────────────────────────────────────────────")
	fmt.Println()

	if checkOnly {
		osExit(1)
		return
	}

	// Windows: no self-replace (OS locks running executables)
	if runtime.GOOS == "windows" {
		fmt.Printf("  Download %s from:\n", latest)
		fmt.Printf("    https://github.com/scrypster/muninndb/releases/tag/%s\n", latest)
		fmt.Println()
		if err := exec.Command("cmd", "/c", "start",
			fmt.Sprintf("https://github.com/scrypster/muninndb/releases/tag/%s", latest)).Start(); err != nil {
			fmt.Println("  (Could not open browser automatically — visit the link above.)")
		}
		return
	}

	// Detect install type before showing pre-confirm copy
	usingBrew := isHomebrewInstall()

	if usingBrew {
		fmt.Println("  Detected Homebrew install.")
		fmt.Println("  This will run: brew upgrade scrypster/tap/muninn")
	} else {
		fmt.Println("  Your data is safe. Only the binary will be replaced.")
		fmt.Println("  The daemon will restart automatically.")
	}
	fmt.Println()

	if !skipConfirm {
		opts := []selectOption{
			{label: fmt.Sprintf("Yes, upgrade to %s", latest), hint: ""},
			{label: fmt.Sprintf("No, keep %s", current), hint: ""},
		}
		fmt.Println("  Upgrade now?")
		fmt.Println()
		choice := runSingleSelect(opts, 0)
		fmt.Println()
		fmt.Println("  ────────────────────────────────────────────────────")
		if choice != 0 {
			fmt.Println()
			fmt.Println("  Upgrade cancelled.")
			fmt.Println()
			return
		}
	}

	// Homebrew: delegate to brew
	if usingBrew {
		fmt.Println()
		fmt.Println("  Running brew upgrade...")
		fmt.Println()
		cmd := exec.Command("brew", "upgrade", "scrypster/tap/muninn")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintf(os.Stderr, "  brew upgrade failed: %v\n", err)
			osExit(1)
		}
		return
	}

	// Self-update (curl/manual installs)
	if err := selfUpdate(latest); err != nil {
		fmt.Println()
		fmt.Fprintf(os.Stderr, "  Upgrade failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "")
		if strings.Contains(err.Error(), "permission denied") {
			fmt.Fprintln(os.Stderr, "  Try: sudo muninn upgrade")
		}
		fmt.Fprintln(os.Stderr, "")
		osExit(1)
		return
	}

	fmt.Println()
	fmt.Println("  ────────────────────────────────────────────────────")
	fmt.Println()
	fmt.Printf("  You're running %s. Enjoy the upgrade.\n", latest)
	fmt.Println()
}

// isHomebrewInstallPath returns true if exePath is under a Homebrew prefix.
func isHomebrewInstallPath(exePath string) bool {
	homebrewMarkers := []string{"/Cellar/", "/opt/homebrew/", "/usr/local/opt/"}
	for _, marker := range homebrewMarkers {
		if strings.Contains(exePath, marker) {
			return true
		}
	}
	return false
}

// isHomebrewInstall returns true if the running binary lives under a Homebrew prefix.
func isHomebrewInstall() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return isHomebrewInstallPath(exe)
}

// releaseAssetURL returns the GitHub release asset URL for the given version, OS, and arch.
// Archive format is tar.gz for Linux/macOS and zip for Windows.
func releaseAssetURL(version, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf(
		"https://github.com/scrypster/muninndb/releases/download/%s/muninn_%s_%s_%s.%s",
		version, version, goos, goarch, ext,
	)
}

// progressReader wraps an io.Reader and calls fn(bytesRead, total) after each read.
type progressReader struct {
	r     io.Reader
	total int64
	read  int64
	fn    func(downloaded, total int64)
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.read += int64(n)
	if pr.fn != nil {
		pr.fn(pr.read, pr.total)
	}
	return n, err
}

// downloadAndExtractBinaryProgress is like downloadAndExtractBinary but calls
// progressFn(bytesDownloaded, totalBytes) during the download. progressFn may be nil.
func downloadAndExtractBinaryProgress(url, binaryName string, progressFn func(downloaded, total int64)) (string, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	var body io.Reader = resp.Body
	if progressFn != nil {
		body = &progressReader{r: resp.Body, total: resp.ContentLength, fn: progressFn}
	}

	gr, err := gzip.NewReader(body)
	if err != nil {
		return "", fmt.Errorf("gzip open: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar read: %w", err)
		}
		if filepath.Base(hdr.Name) != binaryName {
			continue
		}
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("cannot determine executable path: %w", err)
		}
		tmp, err := os.CreateTemp(filepath.Dir(exe), ".muninn-upgrade-*")
		if err != nil {
			return "", fmt.Errorf("temp file: %w", err)
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", fmt.Errorf("write temp: %w", err)
		}
		tmp.Close()
		return tmp.Name(), nil
	}
	return "", fmt.Errorf("binary %q not found in archive", binaryName)
}

// downloadAndExtractBinary downloads a tar.gz from url, extracts the file named
// binaryName, writes it to a temp file next to the current executable, and
// returns the temp file path. Caller is responsible for removing on error or after use.
func downloadAndExtractBinary(url, binaryName string) (string, error) {
	return downloadAndExtractBinaryProgress(url, binaryName, nil)
}

// upgradeStep prints a left-aligned step label, executes fn, then prints ✓ or ✗.
func upgradeStep(label string, fn func() error) error {
	fmt.Printf("  %-28s", label)
	if err := fn(); err != nil {
		fmt.Println("✗")
		return err
	}
	fmt.Println("✓")
	return nil
}

// verifyBinary checks that path is an executable file.
// If expectedVersion is non-empty, it also runs "<path> version" and checks
// that the output contains expectedVersion.
func verifyBinary(path, expectedVersion string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	// Windows does not use Unix execute bits — skip permission check.
	if runtime.GOOS != "windows" && fi.Mode()&0111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	if expectedVersion == "" {
		return nil
	}
	out, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("version check failed: %w", err)
	}
	if !strings.Contains(string(out), strings.TrimPrefix(expectedVersion, "v")) {
		return fmt.Errorf("version mismatch: expected %s in %q", expectedVersion, out)
	}
	return nil
}

// isDaemonRunning returns true if a muninn daemon process is currently running.
func isDaemonRunning() bool {
	pidPath := filepath.Join(defaultDataDir(), "muninn.pid")
	pid, err := readPID(pidPath)
	if err != nil {
		return false
	}
	return isProcessRunning(pid)
}

// selfUpdate performs the atomic binary self-replacement for curl/manual installs.
// Sequence: stop daemon → download → verify → chmod → rename → restart.
func selfUpdate(latest string) error {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	binaryName := "muninn"
	if goos == "windows" {
		binaryName = "muninn.exe"
	}

	assetURL := releaseAssetURL(latest, goos, goarch)

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate current binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("cannot resolve symlink: %w", err)
	}

	daemonWasRunning := isDaemonRunning()

	fmt.Println()

	var tmpPath string

	if err := upgradeStep("Stopping daemon...", func() error {
		if !daemonWasRunning {
			return nil
		}
		pidPath := filepath.Join(defaultDataDir(), "muninn.pid")
		pid, err := readPID(pidPath)
		if err != nil {
			// PID file gone — daemon already stopped
			return nil
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			return nil
		}
		if err := stopProcess(proc); err != nil {
			return fmt.Errorf("stop daemon: %w", err)
		}
		// Wait up to 3s for process to exit
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if !isProcessRunning(pid) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		os.Remove(pidPath)
		return nil
	}); err != nil {
		return err
	}

	// Download with inline progress
	label := fmt.Sprintf("Downloading %s...", latest)
	fmt.Printf("  %-28s", label)
	var dlErr error
	tmpPath, dlErr = downloadAndExtractBinaryProgress(assetURL, binaryName, func(dl, total int64) {
		if total > 0 {
			mb := float64(dl) / 1024 / 1024
			fmt.Printf("\r  %-28s%.1f MB", label, mb)
		}
	})
	if dlErr != nil {
		fmt.Println(" ✗")
		return dlErr
	}
	fmt.Println(" ✓")

	if err := upgradeStep("Verifying binary...", func() error {
		if err := os.Chmod(tmpPath, 0755); err != nil {
			return err
		}
		return verifyBinary(tmpPath, latest)
	}); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err := upgradeStep("Installing...", func() error {
		return os.Rename(tmpPath, exe)
	}); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Restart daemon if it was running before
	if daemonWasRunning {
		fmt.Printf("  %-28s", "Restarting daemon...")
		runStart(true) // manages its own output and error handling
		fmt.Println()
	}

	return nil
}
