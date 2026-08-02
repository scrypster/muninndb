package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Detection of a service manager owning the running daemon.
//
// `muninn upgrade` stops the daemon by PID and restarts it itself. When a service
// manager owns the daemon that is not a safe sequence: the service manager owns the
// restart policy, so a systemd unit with Restart=always races the upgrade — it can
// bring the daemon back up while the binary is mid-replacement, and afterwards the
// unit's view of the process no longer matches reality.
//
// Only systemd is detected. Other managers (launchd, OpenRC, s6, Windows services)
// report no ownership, so behaviour there is unchanged from today.

// readProcCgroup returns the contents of /proc/<pid>/cgroup. A variable so tests can
// supply cgroup layouts that cannot be produced on the machine running them.
var readProcCgroup = func(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	return string(b), err
}

// serviceUnitFromCgroup returns the systemd unit named by a /proc/<pid>/cgroup body.
//
// The leaf of the cgroup path is what decides ownership, not any ancestor. A daemon
// started from an interactive shell inside a systemd user session sits under
// ".../user@1000.service/app.slice/session-3.scope" — an ancestor is a .service, but
// the process belongs to a .scope and no unit manages its lifecycle. Matching on any
// ".service" in the path would report every interactively-started daemon as managed.
func serviceUnitFromCgroup(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// cgroup v2: "0::/system.slice/muninndb.service"
		// cgroup v1: "1:name=systemd:/system.slice/muninndb.service"
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		cgPath := parts[2]
		if cgPath == "" || cgPath == "/" {
			continue
		}
		// cgroup paths are always slash-separated, independent of the host's separator.
		leaf := cgPath[strings.LastIndex(cgPath, "/")+1:]
		if strings.HasSuffix(leaf, ".service") {
			return leaf, true
		}
	}
	return "", false
}

// serviceUnitForPID reports the systemd unit that owns pid, if any.
func serviceUnitForPID(pid int) (string, bool) {
	content, err := readProcCgroup(pid)
	if err != nil {
		// No /proc, no cgroup file, or no permission: we cannot show that a service
		// manager is involved, so we do not claim one is.
		return "", false
	}
	return serviceUnitFromCgroup(content)
}

// runningDaemonPIDs returns the PIDs of live processes running this same binary,
// excluding this one.
//
// This exists because the PID file is not a reliable way to find a service-managed
// daemon. `writePID` is reached only through runStart's fork path, so a unit that
// execs the server directly — Type=simple with `ExecStart=... --daemon --data ...`,
// the ordinary way to run a non-forking daemon under systemd — never writes one,
// while systemd is unmistakably managing the process.
//
// So the guard cannot depend on an artifact that a correctly configured deployment
// legitimately does not have; the process table is the fallback.
var runningDaemonPIDs = func() []int {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		target, err := filepath.EvalSymlinks(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			// Not ours to inspect, or gone since ReadDir. Either way, not evidence.
			continue
		}
		if target == exe {
			pids = append(pids, pid)
		}
	}
	return pids
}

// serviceManagerOwnsDaemon reports the unit managing the running daemon.
//
// It deliberately only fires while a daemon is actually running. With the daemon
// stopped there is no lifecycle to fight over — replacing the binary is exactly what
// the refusal message tells the operator to do, and the unit picks the new one up on
// its next start.
func serviceManagerOwnsDaemon() (string, bool) {
	// The PID file is the precise answer when it exists: it names the daemon for
	// *this* data directory.
	if pid, err := readPID(filepath.Join(defaultDataDir(), "muninn.pid")); err == nil && isProcessRunning(pid) {
		return serviceUnitForPID(pid)
	}
	// No usable PID file. Any live process running this binary under a unit is enough
	// to make a self-update unsafe, so report the first one found.
	for _, pid := range runningDaemonPIDs() {
		if unit, ok := serviceUnitForPID(pid); ok {
			return unit, true
		}
	}
	return "", false
}

// serviceManagerRefusal is the message shown when the daemon is service-managed.
// Returned rather than printed so the wording is testable.
func serviceManagerRefusal(unit string) string {
	name := strings.TrimSuffix(unit, ".service")
	var b strings.Builder
	fmt.Fprintf(&b, "  ⚠  The running daemon is managed by systemd (unit: %s).\n", unit)
	b.WriteString("\n")
	b.WriteString("     `muninn upgrade` stops the daemon by PID and restarts it itself.\n")
	b.WriteString("     systemd owns that lifecycle here, so the two would race: the unit's\n")
	b.WriteString("     restart policy can bring the daemon back while the binary is being\n")
	b.WriteString("     replaced, and its view of the process would no longer match reality.\n")
	b.WriteString("\n")
	b.WriteString("     Upgrade through the service manager instead:\n")
	b.WriteString("\n")
	fmt.Fprintf(&b, "         sudo systemctl stop %s\n", name)
	b.WriteString("         sudo muninn upgrade\n")
	fmt.Fprintf(&b, "         sudo systemctl start %s\n", name)
	b.WriteString("\n")
	b.WriteString("     With the daemon stopped, `muninn upgrade` replaces the binary and\n")
	b.WriteString("     leaves the restart to systemd.\n")
	return b.String()
}
