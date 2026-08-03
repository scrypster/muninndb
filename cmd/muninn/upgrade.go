package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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

	// Refuse before touching anything if a service manager owns the running daemon.
	// This sits after --check on purpose: reporting that an update exists is safe and
	// useful on a service-managed host; performing one behind the manager's back is not.
	if unit, managed := serviceManagerOwnsDaemon(); managed {
		fmt.Fprint(os.Stderr, serviceManagerRefusal(unit))
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
		fmt.Println("  The daemon will be stopped before upgrading and restarted after.")
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

	// Homebrew: stop daemon → brew upgrade → restart daemon
	if usingBrew {
		fmt.Println()

		daemonWasRunning := isDaemonRunning()

		if daemonWasRunning {
			fmt.Printf("  %-28s", "Stopping daemon...")
			pidPath := filepath.Join(defaultDataDir(), "muninn.pid")
			if !stopDaemonForUpgrade(pidPath, 15*time.Second) {
				fmt.Println(" ✗")
				fmt.Fprintln(os.Stderr, "the daemon is still running and could not be stopped.")
				fmt.Fprintln(os.Stderr, "If it was started with elevated privileges, stop it the same way")
				fmt.Fprintln(os.Stderr, "(for example 'sudo muninn stop' or systemctl), then upgrade again.")
				osExit(1)
				return
			}
			fmt.Println(" ✓")
		}

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

		if daemonWasRunning {
			fmt.Println()
			fmt.Printf("  %-28s", "Restarting daemon...")
			if err := runStart(true); err != nil {
				fmt.Println(" ✗")
				fmt.Fprintf(os.Stderr, "  Failed to restart daemon: %v\n", err)
				osExit(1)
			}
			fmt.Println(" ✓")
			fmt.Println()
			addrs, _ := readAddrsFile(defaultDataDir())
			uiLines := webUIDisplay(addrs)
			fmt.Printf("  Web UI → %s\n", uiLines[0])
			for _, l := range uiLines[1:] {
				fmt.Printf("           %s\n", l)
			}
			fmt.Println()
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

// releaseAssetName returns the bare filename of the release archive for the given
// version, OS, and arch — e.g. "muninn_v1.2.3_linux_amd64.tar.gz". This must match
// the names release.yml feeds to sha256sum, because it is the lookup key into
// checksums.txt. Archive format is tar.gz for Linux/macOS and zip for Windows.
func releaseAssetName(version, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("muninn_%s_%s_%s.%s", version, goos, goarch, ext)
}

// releaseAssetURL returns the GitHub release asset URL for the given version, OS, and arch.
// Derived from releaseAssetName so the URL and the checksum lookup key cannot drift apart.
func releaseAssetURL(version, goos, goarch string) string {
	return fmt.Sprintf(
		"https://github.com/scrypster/muninndb/releases/download/%s/%s",
		version, releaseAssetName(version, goos, goarch),
	)
}

// checksumsURL returns the URL of the checksums.txt asset published alongside a release.
func checksumsURL(version string) string {
	return fmt.Sprintf(
		"https://github.com/scrypster/muninndb/releases/download/%s/checksums.txt",
		version,
	)
}

// parseChecksums reads sha256sum output format ("<64-hex-hash>  <filename>") into a
// filename → hash map. Lines that aren't a valid SHA-256 hash plus a filename are
// skipped. An input yielding no valid entries is an error, not an empty map: an empty
// map would make every lookup miss, which verifyChecksum would then have to treat as
// either "fail everything" or "skip verification" — better to reject it here.
func parseChecksums(r io.Reader) (map[string]string, error) {
	sums := make(map[string]string)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		hash, name := fields[0], fields[1]
		// SHA-256 is exactly 64 hex characters. Anything else is a different
		// algorithm or a malformed line; either way we can't verify with it.
		if len(hash) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(hash); err != nil {
			continue
		}
		// sha256sum binary mode writes "*filename"; normalize it away.
		sums[strings.TrimPrefix(name, "*")] = hash
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read checksums: %w", err)
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("checksums file contained no valid SHA-256 entries")
	}
	return sums, nil
}

// fetchChecksums downloads and parses a release's checksums.txt.
func fetchChecksums(url string) (map[string]string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch checksums: HTTP %d", resp.StatusCode)
	}
	// Cap the read: checksums.txt is a few hundred bytes, and we don't want a
	// hostile or misrouted response to exhaust memory.
	return parseChecksums(io.LimitReader(resp.Body, 1<<20))
}

// verifyChecksum compares the SHA-256 we computed over the downloaded archive against
// the published one. Fails closed in both directions: a mismatch is rejected, and so is
// an asset that isn't listed at all — otherwise anyone able to serve a checksums.txt
// could disable verification just by omitting the entry.
func verifyChecksum(assetName, gotSum string, sums map[string]string) error {
	want, ok := sums[assetName]
	if !ok {
		return fmt.Errorf("%s is not listed in checksums.txt — refusing to install an unverifiable binary", assetName)
	}
	if !strings.EqualFold(want, gotSum) {
		return fmt.Errorf("checksum mismatch for %s:\n    expected %s\n    got      %s", assetName, want, gotSum)
	}
	return nil
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
//
// It also returns the hex SHA-256 of the *entire* archive as served, for checking
// against the release's checksums.txt. The hash deliberately covers every byte the
// server sent, not just the prefix the tar reader consumed to reach the binary —
// otherwise an archive with a payload appended after the binary entry would hash
// clean. That is why the tail is drained before the sum is taken.
func downloadAndExtractBinaryProgress(url, binaryName string, progressFn func(downloaded, total int64)) (string, string, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	hasher := sha256.New()
	var body io.Reader = io.TeeReader(resp.Body, hasher)
	if progressFn != nil {
		body = &progressReader{r: body, total: resp.ContentLength, fn: progressFn}
	}

	gr, err := gzip.NewReader(body)
	if err != nil {
		return "", "", fmt.Errorf("gzip open: %w", err)
	}
	defer gr.Close()

	tmpPath := ""
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", fmt.Errorf("tar read: %w", err)
		}
		if filepath.Base(hdr.Name) != binaryName {
			continue
		}
		exe, err := os.Executable()
		if err != nil {
			return "", "", fmt.Errorf("cannot determine executable path: %w", err)
		}
		tmp, err := os.CreateTemp(filepath.Dir(exe), ".muninn-upgrade-*")
		if err != nil {
			return "", "", fmt.Errorf("temp file: %w", err)
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", "", fmt.Errorf("write temp: %w", err)
		}
		tmp.Close()
		tmpPath = tmp.Name()
		break
	}
	if tmpPath == "" {
		return "", "", fmt.Errorf("binary %q not found in archive", binaryName)
	}

	// Pull the rest of the stream through the hasher so the sum covers the whole file.
	if _, err := io.Copy(io.Discard, body); err != nil {
		os.Remove(tmpPath)
		return "", "", fmt.Errorf("read archive tail: %w", err)
	}

	return tmpPath, hex.EncodeToString(hasher.Sum(nil)), nil
}

// downloadAndExtractBinary downloads a tar.gz from url, extracts the file named
// binaryName, writes it to a temp file next to the current executable, and
// returns the temp file path. Caller is responsible for removing on error or after use.
func downloadAndExtractBinary(url, binaryName string) (string, error) {
	path, _, err := downloadAndExtractBinaryProgress(url, binaryName, nil)
	return path, err
}

// stopDaemonForUpgrade stops the daemon named by pidPath so the binary
// underneath it can be replaced, waiting up to timeout for it to exit, and
// reports whether it is gone afterwards.
//
// The PID file is cleared only once the daemon is known to be gone. A daemon
// started with elevated privileges cannot be signalled by an unprivileged
// upgrade: both the signal and the kill are refused, and removing its PID
// file would leave it running against the database with nothing on disk to
// find it by.
func stopDaemonForUpgrade(pidPath string, timeout time.Duration) bool {
	pid, err := readPID(pidPath)
	if err != nil {
		// A PID file that vanished between the liveness check and here leaves
		// nothing to stop. Any other failure — unreadable, unparseable — means
		// we cannot identify the daemon the caller just saw running, and must
		// not report it as stopped.
		return errors.Is(err, os.ErrNotExist)
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = stopProcess(proc)
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if !isProcessRunning(pid) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if isProcessRunning(pid) {
			_ = proc.Kill()
			time.Sleep(500 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if probeProcess(pid) != processDead {
		return false
	}
	os.Remove(pidPath)
	return true
}

// stopDaemonInstances stops every live process this machine can find that is
// running this same binary — both the one named by pidPath (if any) and any
// additional live process the exe-scan (runningDaemonPIDs) finds that the PID
// file alone does not name.
//
// This exists because a daemon not started by `muninn start` has no PID file
// at all — launchd, a systemd Type=simple unit that execs the server
// directly, `systemd-run --scope`, and any operator-run `--daemon` process —
// and relying on the PID file alone meant selfUpdate's "Stopping daemon..."
// step printed ✓ having stopped nothing for every one of those (#792).
//
// It reports stoppedAny=true if anything needed stopping, and returns an
// error — rather than silently claiming success — if any known live PID
// survives the stop attempt. The caller must not proceed past that error:
// printing "Enjoy the upgrade" while the OLD binary is still serving traffic
// off a since-renamed inode is exactly the silently-wrong case this closes.
//
// DOES NOT CATCH: a live daemon this machine's exe-scan cannot see at all.
// runningDaemonPIDs depends on /proc, so on darwin (including launchd, this
// project's own recommended setup) it returns nothing — the gap the issue
// measured stays open here. Closing that needs a cross-platform process
// enumeration this increment does not add; the honesty fix (never print ✓
// for a stop that didn't happen) applies as far as detection reaches today.
func stopDaemonInstances(pidPath string, timeout time.Duration) (stoppedAny bool, err error) {
	pidFilePID, pidFileErr := readPID(pidPath)
	havePIDFilePID := pidFileErr == nil

	targets := make(map[int]bool)
	if havePIDFilePID {
		targets[pidFilePID] = true
	}
	for _, pid := range runningDaemonPIDs() {
		targets[pid] = true
	}

	for pid := range targets {
		if !isProcessRunning(pid) {
			continue
		}
		stoppedAny = true
		proc, ferr := os.FindProcess(pid)
		if ferr != nil {
			continue
		}
		_ = stopProcess(proc)
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if !isProcessRunning(pid) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if isProcessRunning(pid) {
			_ = proc.Kill()
			time.Sleep(500 * time.Millisecond)
		}
	}

	if stoppedAny {
		// Brief grace period for the OS to release file locks (e.g. PebbleDB
		// LOCK file) before the new binary attempts to open the same data
		// directory.
		time.Sleep(200 * time.Millisecond)
	}

	var stillAlive []int
	for pid := range targets {
		if isProcessRunning(pid) {
			stillAlive = append(stillAlive, pid)
		}
	}
	if len(stillAlive) > 0 {
		sort.Ints(stillAlive)
		return stoppedAny, fmt.Errorf(
			"the daemon (pid(s) %v) is still running and could not be stopped. "+
				"If it was started with elevated privileges, stop it the same way "+
				"(for example 'sudo muninn stop' or systemctl), then upgrade again",
			stillAlive)
	}

	if havePIDFilePID {
		os.Remove(pidPath)
	}
	return stoppedAny, nil
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
	assetName := releaseAssetName(latest, goos, goarch)

	// Fetch the published checksums before downloading anything. Fail closed: if we
	// can't get them, we can't tell an official build from a tampered one, and this
	// binary is about to replace itself on disk. install.sh warns and continues in
	// this case, but it runs once on a machine with nothing to lose yet; an upgrade
	// overwrites a working install.
	sums, err := fetchChecksums(checksumsURL(latest))
	if err != nil {
		return fmt.Errorf("cannot verify this release: %w\n"+
			"    Download it manually from https://github.com/scrypster/muninndb/releases/tag/%s "+
			"if you want to proceed without verification", err, latest)
	}

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

	pidPath := filepath.Join(defaultDataDir(), "muninn.pid")
	if err := upgradeStep("Stopping daemon...", func() error {
		stopped, stopErr := stopDaemonInstances(pidPath, 15*time.Second)
		if stopErr != nil {
			return stopErr
		}
		if stopped {
			// A live process was found and confirmed stopped even though
			// isDaemonRunning() (PID-file-only) said no daemon was running —
			// e.g. the exe-scan found a daemon with no PID file (#792). Make
			// sure the restart step below still fires for it.
			daemonWasRunning = true
		}
		return nil
	}); err != nil {
		return err
	}

	// Download with inline progress
	label := fmt.Sprintf("Downloading %s...", latest)
	fmt.Printf("  %-28s", label)
	var dlErr error
	var gotSum string
	tmpPath, gotSum, dlErr = downloadAndExtractBinaryProgress(assetURL, binaryName, func(dl, total int64) {
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

	// Checksum first, and on its own step. Everything below this point either executes
	// the downloaded file (verifyBinary runs `<path> version`) or installs it, so this
	// is the last moment where a tampered archive is still inert bytes on disk.
	if err := upgradeStep("Verifying checksum...", func() error {
		return verifyChecksum(assetName, gotSum, sums)
	}); err != nil {
		os.Remove(tmpPath)
		return err
	}

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
		if err := runStart(true); err != nil {
			fmt.Println(" ✗")
			return fmt.Errorf("restart failed: %w", err)
		}
		fmt.Println(" ✓")
	}

	return nil
}
