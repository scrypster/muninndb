package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestServiceUnitFromCgroup_V2SystemService(t *testing.T) {
	unit, ok := serviceUnitFromCgroup("0::/system.slice/muninndb.service\n")
	if !ok {
		t.Fatal("a system.slice unit was not recognised as service-managed")
	}
	if unit != "muninndb.service" {
		t.Errorf("unit = %q, want muninndb.service", unit)
	}
}

func TestServiceUnitFromCgroup_V1SystemService(t *testing.T) {
	// cgroup v1 lists one line per controller; only the name=systemd line carries
	// the unit path, and the others must not confuse the parse.
	content := `12:pids:/system.slice/muninndb.service
5:memory:/system.slice/muninndb.service
1:name=systemd:/system.slice/muninndb.service
`
	unit, ok := serviceUnitFromCgroup(content)
	if !ok || unit != "muninndb.service" {
		t.Errorf("serviceUnitFromCgroup = (%q, %v), want (muninndb.service, true)", unit, ok)
	}
}

// The case that decides whether the leaf or any ancestor is consulted: a daemon
// started by hand inside a systemd user session has ".service" in its cgroup path,
// but belongs to a .scope and no unit manages its lifecycle. Matching an ancestor
// would refuse to upgrade on every interactively-started daemon.
func TestServiceUnitFromCgroup_InteractiveSessionIsNotManaged(t *testing.T) {
	content := "0::/user.slice/user-1000.slice/user@1000.service/app.slice/session-3.scope\n"
	if unit, ok := serviceUnitFromCgroup(content); ok {
		t.Errorf("interactive session reported as managed by %q", unit)
	}
}

func TestServiceUnitFromCgroup_UserManagerUnit(t *testing.T) {
	// A per-user unit is still a unit, and systemd --user still owns its restarts.
	content := "0::/user.slice/user-1000.slice/user@1000.service/app.slice/muninndb.service\n"
	unit, ok := serviceUnitFromCgroup(content)
	if !ok || unit != "muninndb.service" {
		t.Errorf("serviceUnitFromCgroup = (%q, %v), want (muninndb.service, true)", unit, ok)
	}
}

func TestServiceUnitFromCgroup_RootCgroupIsNotManaged(t *testing.T) {
	// Containers and non-systemd inits commonly show a bare root cgroup.
	for _, content := range []string{"0::/\n", "0::\n", "", "\n\n"} {
		if unit, ok := serviceUnitFromCgroup(content); ok {
			t.Errorf("content %q reported as managed by %q", content, unit)
		}
	}
}

func TestServiceUnitFromCgroup_MalformedLinesAreSkipped(t *testing.T) {
	content := `not-a-cgroup-line
0::/system.slice/muninndb.service
`
	unit, ok := serviceUnitFromCgroup(content)
	if !ok || unit != "muninndb.service" {
		t.Errorf("a malformed leading line broke the parse: (%q, %v)", unit, ok)
	}
}

func TestServiceUnitFromCgroup_ScopeUnderSystemSliceIsNotManaged(t *testing.T) {
	// `systemd-run --scope` produces a transient scope, not a service: systemd does
	// not restart it, so the upgrade path is not racing anything.
	content := "0::/system.slice/run-r123.scope\n"
	if unit, ok := serviceUnitFromCgroup(content); ok {
		t.Errorf("a transient scope reported as managed by %q", unit)
	}
}

func TestServiceUnitForPID_UnreadableCgroupIsNotManaged(t *testing.T) {
	old := readProcCgroup
	readProcCgroup = func(int) (string, error) { return "", fmt.Errorf("no such file") }
	defer func() { readProcCgroup = old }()

	if unit, ok := serviceUnitForPID(1234); ok {
		t.Errorf("unreadable cgroup reported as managed by %q — must not claim "+
			"a service manager it cannot see", unit)
	}
}

// A Type=simple unit that execs the server directly never reaches the code that
// writes the PID file, while systemd is unmistakably managing the process. A guard
// consulting only the PID file is inert on exactly that deployment, so the
// process-table fallback has to carry it.
func TestServiceManagerOwnsDaemon_FallsBackWhenNoPIDFile(t *testing.T) {
	t.Setenv("MUNINNDB_DATA", t.TempDir()) // no muninn.pid in here

	oldPIDs := runningDaemonPIDs
	runningDaemonPIDs = func() []int { return []int{4242} }
	defer func() { runningDaemonPIDs = oldPIDs }()

	oldCgroup := readProcCgroup
	readProcCgroup = func(pid int) (string, error) {
		if pid != 4242 {
			return "", fmt.Errorf("unexpected pid %d", pid)
		}
		return "0::/system.slice/muninndb.service\n", nil
	}
	defer func() { readProcCgroup = oldCgroup }()

	unit, ok := serviceManagerOwnsDaemon()
	if !ok {
		t.Fatal("a service-managed daemon with no PID file was not detected")
	}
	if unit != "muninndb.service" {
		t.Errorf("unit = %q, want muninndb.service", unit)
	}
}

func TestServiceManagerOwnsDaemon_UnmanagedDaemonIsAllowed(t *testing.T) {
	t.Setenv("MUNINNDB_DATA", t.TempDir())

	oldPIDs := runningDaemonPIDs
	runningDaemonPIDs = func() []int { return []int{4242} }
	defer func() { runningDaemonPIDs = oldPIDs }()

	oldCgroup := readProcCgroup
	readProcCgroup = func(int) (string, error) {
		return "0::/user.slice/user-1000.slice/session-3.scope\n", nil
	}
	defer func() { readProcCgroup = oldCgroup }()

	if unit, ok := serviceManagerOwnsDaemon(); ok {
		t.Errorf("refused an unmanaged daemon (reported %q) — a hand-started daemon "+
			"must still be upgradable", unit)
	}
}

func TestServiceManagerOwnsDaemon_NoDaemonRunning(t *testing.T) {
	t.Setenv("MUNINNDB_DATA", t.TempDir())

	oldPIDs := runningDaemonPIDs
	runningDaemonPIDs = func() []int { return nil }
	defer func() { runningDaemonPIDs = oldPIDs }()

	if unit, ok := serviceManagerOwnsDaemon(); ok {
		t.Errorf("refused with no daemon running (reported %q) — replacing the binary "+
			"while stopped is the documented path", unit)
	}
}

func TestServiceManagerRefusal_IsActionable(t *testing.T) {
	msg := serviceManagerRefusal("muninndb.service")

	if !strings.Contains(msg, "muninndb.service") {
		t.Error("refusal does not name the unit it detected")
	}
	// The operator needs the exact commands, with the unit name resolved.
	for _, want := range []string{
		"systemctl stop muninndb",
		"systemctl start muninndb",
		"muninn upgrade",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal is missing the step %q:\n%s", want, msg)
		}
	}
	// ".service.service" would mean the suffix was not stripped for the CLI commands.
	if strings.Contains(msg, "stop muninndb.service") {
		t.Errorf("unit suffix leaked into the systemctl command:\n%s", msg)
	}
}
