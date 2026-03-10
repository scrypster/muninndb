package backup

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Checkpointer is implemented by the storage engine to create a Pebble
// checkpoint at a given destination directory.
type Checkpointer interface {
	Checkpoint(destDir string) error
}

// Config holds the configuration for the backup scheduler.
type Config struct {
	Interval  time.Duration
	BackupDir string
	Retain    int
	DataDir   string // for aux file copying (wal, auth_secret)
}

// Status is a snapshot of the scheduler's current state.
type Status struct {
	Enabled       bool      `json:"enabled"`
	Interval      string    `json:"interval,omitempty"`
	BackupDir     string    `json:"backup_dir,omitempty"`
	Retain        int       `json:"retain"`
	LastRunAt     time.Time `json:"last_run_at,omitempty"`
	LastRunOK     bool      `json:"last_run_ok"`
	LastError     string    `json:"last_error,omitempty"`
	LastSizeBytes int64     `json:"last_size_bytes,omitempty"`
	LastElapsed   string    `json:"last_elapsed,omitempty"`
	NextRunAt     time.Time `json:"next_run_at,omitempty"`
	PrunedCount   int       `json:"pruned_count,omitempty"`
}

// Scheduler runs periodic backups of the MuninnDB engine.
type Scheduler struct {
	cfg Config
	eng Checkpointer

	mu          sync.RWMutex
	lastRunAt   time.Time
	lastRunOK   bool
	lastError   string
	lastSize    int64
	lastElapsed string
	nextRunAt   time.Time
	prunedCount int
}

// New creates a new Scheduler. Returns nil if cfg.Interval is zero (disabled).
func New(cfg Config, eng Checkpointer) *Scheduler {
	if cfg.Interval == 0 {
		return nil
	}
	return &Scheduler{cfg: cfg, eng: eng}
}

// Start launches the backup goroutine. It respects context cancellation.
// The returned channel is closed when the goroutine has fully stopped.
func (s *Scheduler) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.run(ctx)
	}()
	return done
}

// GetStatus returns a thread-safe snapshot of the scheduler's current state.
func (s *Scheduler) GetStatus() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := Status{
		Enabled:       true,
		Interval:      s.cfg.Interval.String(),
		BackupDir:     s.cfg.BackupDir,
		Retain:        s.cfg.Retain,
		LastRunAt:     s.lastRunAt,
		LastRunOK:     s.lastRunOK,
		LastError:     s.lastError,
		LastSizeBytes: s.lastSize,
		LastElapsed:   s.lastElapsed,
		NextRunAt:     s.nextRunAt,
		PrunedCount:   s.prunedCount,
	}
	return st
}

// run is the scheduler's main loop.
func (s *Scheduler) run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	s.mu.Lock()
	s.nextRunAt = time.Now().Add(s.cfg.Interval)
	s.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			_ = t
			s.runOnce()

			s.mu.Lock()
			s.nextRunAt = time.Now().Add(s.cfg.Interval)
			s.mu.Unlock()
		}
	}
}

// runOnce performs a single backup cycle: checkpoint, aux file copy, prune.
func (s *Scheduler) runOnce() {
	start := time.Now()
	ts := start.UTC().Format("20060102-150405")
	destDir := filepath.Join(s.cfg.BackupDir, "backup-"+ts)

	slog.Info("backup: scheduled backup starting", "dest", destDir)

	if err := os.MkdirAll(destDir, 0700); err != nil {
		s.recordError(start, fmt.Errorf("create backup dir: %w", err))
		return
	}

	checkpointDir := filepath.Join(destDir, "pebble")
	if err := s.eng.Checkpoint(checkpointDir); err != nil {
		os.RemoveAll(destDir)
		s.recordError(start, fmt.Errorf("pebble checkpoint: %w", err))
		return
	}
	slog.Info("backup: pebble checkpoint complete", "dir", checkpointDir)

	if s.cfg.DataDir != "" {
		walSrc := filepath.Join(s.cfg.DataDir, "wal")
		walDst := filepath.Join(destDir, "wal")
		if info, err := os.Stat(walSrc); err == nil && info.IsDir() {
			if err := copyDir(walSrc, walDst); err != nil {
				slog.Warn("backup: failed to copy wal directory", "err", err)
			}
		}

		secretSrc := filepath.Join(s.cfg.DataDir, "auth_secret")
		secretDst := filepath.Join(destDir, "auth_secret")
		if _, err := os.Stat(secretSrc); err == nil {
			if err := copyFile(secretSrc, secretDst); err != nil {
				slog.Warn("backup: failed to copy auth_secret", "err", err)
			}
		}
	}

	elapsed := time.Since(start)
	size := dirSize(destDir)

	pruned := s.pruneOldBackups()

	slog.Info("backup: complete",
		"dest", destDir,
		"size_bytes", size,
		"elapsed", elapsed.Round(time.Millisecond),
		"pruned", pruned,
	)

	s.mu.Lock()
	s.lastRunAt = start
	s.lastRunOK = true
	s.lastError = ""
	s.lastSize = size
	s.lastElapsed = elapsed.Round(time.Millisecond).String()
	s.prunedCount = pruned
	s.mu.Unlock()
}

// recordError updates status fields after a failed backup attempt.
func (s *Scheduler) recordError(start time.Time, err error) {
	slog.Error("backup: scheduled backup failed", "err", err)
	s.mu.Lock()
	s.lastRunAt = start
	s.lastRunOK = false
	s.lastError = err.Error()
	s.mu.Unlock()
}

// pruneOldBackups removes the oldest backup directories beyond cfg.Retain.
// Returns the number of directories deleted.
func (s *Scheduler) pruneOldBackups() int {
	if s.cfg.Retain <= 0 {
		return 0
	}

	entries, err := os.ReadDir(s.cfg.BackupDir)
	if err != nil {
		slog.Warn("backup: failed to read backup dir for pruning", "err", err)
		return 0
	}

	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}

	// Sort ascending by name — timestamps in "backup-YYYYMMDD-HHMMSS" format
	// sort correctly lexicographically so oldest comes first.
	sort.Strings(dirs)

	pruned := 0
	excess := len(dirs) - s.cfg.Retain
	for i := 0; i < excess; i++ {
		target := filepath.Join(s.cfg.BackupDir, dirs[i])
		if err := os.RemoveAll(target); err != nil {
			slog.Warn("backup: failed to prune old backup", "dir", target, "err", err)
		} else {
			slog.Info("backup: pruned old backup", "dir", target)
			pruned++
		}
	}
	return pruned
}

// copyFile copies a single file preserving its permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// copyDir recursively copies a directory tree from src to dst.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		return copyFile(path, target)
	})
}

// dirSize returns the total byte size of all files under dir.
func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
