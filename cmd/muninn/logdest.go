package main

import (
	"os"
	"path/filepath"
	"strings"
)

// logDestFileName is the sidecar the daemon writes at EVERY startup,
// regardless of how it was started, recording where its logs actually go —
// the fix for #852: `muninn logs` reading <dataDir>/muninn.log
// unconditionally, even when the running daemon's real output goes
// somewhere else entirely (a service manager capturing inherited stderr,
// with no PID file and no /proc-based exe-scan match on a launchd-managed
// darwin host).
//
// Unlike muninn.pid (written only by the CLI's fork-a-daemon path,
// lifecycle.go's runStart) and the exe-scan in service_manager.go (which
// depends on /proc and so never fires under launchd), this sidecar is
// written by the daemon ITSELF at the same place it constructs its log
// handler — so it is authoritative for every start shape: CLI fork,
// `muninn --daemon` under a systemd unit, or under launchd.
const logDestFileName = "muninn.logdest"

// logDestInherited is the sentinel written when the daemon has no explicit
// --log-file/MUNINN_LOG_FILE configured: its output is whatever stderr its
// parent gave it (a supervisor's capture file or journal, a terminal, or
// even the CLI fork's redirected muninn.log if MUNINN_LOG_FILE somehow
// failed to reach it), and this process has no portable way to resolve
// that inherited descriptor back to a path.
const logDestInherited = "stderr:inherited"

func writeLogDestFile(dataDir, dest string) error {
	return os.WriteFile(filepath.Join(dataDir, logDestFileName), []byte(dest), 0600)
}

func readLogDestFile(dataDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, logDestFileName))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// resolveLogPath decides which file `muninn logs` should tail, and whether
// it should refuse to guess at all.
//
//   - sidecar holds a concrete path  → tail it. This is the authoritative
//     answer once any daemon built after #850/#852 has started at least
//     once, and it covers the common `muninn start` case too (lifecycle.go
//     sets MUNINN_LOG_FILE to the same historical default path).
//   - sidecar holds logDestInherited → the daemon's log destination is
//     stderr inherited from its parent; refuse to guess (inherited=true).
//   - sidecar absent/unreadable      → an older daemon that predates this
//     fix, or nothing has ever run here. Fall back to the historical
//     default so a previously CLI-managed daemon's log is still shown —
//     the same behavior `muninn logs` had before #852.
func resolveLogPath(dataDir string) (path string, inherited bool) {
	dest, err := readLogDestFile(dataDir)
	if err != nil {
		return filepath.Join(dataDir, "muninn.log"), false
	}
	if dest == logDestInherited {
		return "", true
	}
	return dest, false
}
