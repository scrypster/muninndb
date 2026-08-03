package main

import (
	"log/slog"
	"os"
	"sync"
	"syscall"
)

// reopenableFile is an io.Writer over a named file that can be atomically
// repointed at a fresh descriptor for the same path — the mechanism #850
// needs so a SIGHUP handler can implement log rotation's "reopen" half.
//
// A process supervisor's rotation contract (logrotate, newsyslog) is:
// rename the file, then signal the writer to reopen it. Without a reopen
// path the writer keeps its descriptor on the renamed (or deleted) inode
// forever — the "rotated" file keeps growing invisibly and the fresh file
// at the original path stays empty. Nothing logs an error, so the failure
// is silent until someone notices weeks of missing data (the exact failure
// #850 was filed against).
//
// The swap takes the write lock so no write can land half-composed of the
// old descriptor and half of the new one: Reopen excludes every in-flight
// Write before it closes the old file, and every Write after the swap sees
// the new one. It is deliberately not a bare pointer swap for that reason.
type reopenableFile struct {
	mu   sync.RWMutex
	path string
	f    *os.File
}

// newReopenableFile opens path (creating it if absent) in append mode and
// wraps it for later reopening.
func newReopenableFile(path string) (*reopenableFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &reopenableFile{path: path, f: f}, nil
}

// Write implements io.Writer. Multiple concurrent writers may hold the read
// lock at once (an *os.File is safe for concurrent Write calls); Reopen is
// the only writer-side operation, and it takes the exclusive lock.
func (r *reopenableFile) Write(p []byte) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.f.Write(p)
}

// Reopen closes the current descriptor and opens a fresh one at the same
// path — the operation a SIGHUP handler runs after a rotator has renamed
// the file out from under the daemon. Safe to call from any goroutine.
func (r *reopenableFile) Reopen() error {
	newF, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	r.mu.Lock()
	old := r.f
	r.f = newF
	r.mu.Unlock()
	return old.Close()
}

// Close closes the current descriptor. Not used on the daemon's own
// shutdown path today (the process exits and the OS reclaims the fd); it
// exists so tests can clean up deterministically.
func (r *reopenableFile) Close() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.f.Close()
}

// logReopener is the interface handleServerSignal needs from a log
// destination. *reopenableFile satisfies it; tests substitute a spy so the
// signal-routing decision can be checked without touching the filesystem.
type logReopener interface {
	Reopen() error
}

// handleServerSignal processes one signal delivered to the running daemon.
//
//   - SIGHUP reopens the log file (see reopenableFile) and does not touch
//     shutdown state, so it is safe to receive at any point in the
//     process's life, including mid-shutdown. When reopener is nil — no
//     --log-file/MUNINN_LOG_FILE was configured, so the daemon's log
//     destination is inherited stderr with no path this process can
//     resolve and reopen — SIGHUP is a documented no-op: it WARNs rather
//     than attempting anything, per principle #2 (degrade loudly, never
//     silently-wrong). A supervisor that renames an inherited-stderr
//     destination out from under the daemon is not fixed by this; the
//     operator must configure an explicit log file to get reopen support.
//   - SIGINT/SIGTERM (any other signal reaching this function) request
//     graceful shutdown on the first occurrence (cancel is called exactly
//     once) and force an immediate exit on a second, matching the
//     pre-existing behavior this replaces. shutdownRequested is owned by
//     the caller's signal-handling goroutine and must not be read/written
//     concurrently from anywhere else.
func handleServerSignal(sig os.Signal, reopener logReopener, cancel func(), shutdownRequested *bool, exitFn func(int)) {
	if sig == syscall.SIGHUP {
		if reopener == nil {
			slog.Warn("SIGHUP received but no reopenable log file is configured — reopen is a no-op; set --log-file or MUNINN_LOG_FILE to enable log rotation")
			return
		}
		if err := reopener.Reopen(); err != nil {
			slog.Error("log reopen failed", "err", err)
			return
		}
		slog.Info("log file reopened")
		return
	}
	if *shutdownRequested {
		slog.Error("second signal received — forcing immediate exit")
		exitFn(1)
		return
	}
	*shutdownRequested = true
	slog.Info("shutdown signal received — starting graceful shutdown")
	cancel()
}
