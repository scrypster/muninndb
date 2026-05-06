package rest

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cockroachdb/pebble"
)

// BackupRequest is the JSON body for POST /api/admin/backup.
type BackupRequest struct {
	OutputDir string `json:"output_dir"`
}

// BackupResponse is the JSON response for a successful backup.
type BackupResponse struct {
	OutputDir string `json:"output_dir"`
	SizeBytes int64  `json:"size_bytes"`
	Elapsed   string `json:"elapsed"`
}

// handleBackup creates a point-in-time backup of the database using Pebble's
// Checkpoint, then copies auxiliary files (WAL segments, auth_secret).
// This endpoint is safe to call on a live, running server.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	var req BackupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	if req.OutputDir == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "output_dir is required")
		return
	}

	if _, err := os.Stat(req.OutputDir); err == nil {
		s.sendError(r, w, http.StatusConflict, ErrVaultForbidden,
			fmt.Sprintf("output directory %q already exists", req.OutputDir))
		return
	}

	start := time.Now()
	slog.Info("backup: starting online backup", "output", req.OutputDir)

	if err := os.MkdirAll(req.OutputDir, 0700); err != nil {
		s.emitAuditErr(r, "backup.trigger", "system", "backup", err, nil)
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError,
			"failed to create output directory: "+err.Error())
		return
	}

	checkpointDir := filepath.Join(req.OutputDir, "pebble")
	if err := s.engine.Checkpoint(checkpointDir); err != nil {
		os.RemoveAll(req.OutputDir)
		s.emitAuditErr(r, "backup.trigger", "system", "backup", err, nil)
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError,
			"pebble checkpoint failed: "+err.Error())
		return
	}
	slog.Info("backup: pebble checkpoint complete", "dir", checkpointDir)

	if err := backupVerifyCheckpoint(checkpointDir); err != nil {
		os.RemoveAll(req.OutputDir)
		s.emitAuditErr(r, "backup.trigger", "system", "backup", err, nil)
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError,
			"backup verification failed: "+err.Error())
		return
	}
	slog.Info("backup verified: checkpoint is readable", "dir", checkpointDir)

	if s.dataDir != "" {
		walSrc := filepath.Join(s.dataDir, "wal")
		walDst := filepath.Join(req.OutputDir, "wal")
		if info, err := os.Stat(walSrc); err == nil && info.IsDir() {
			if err := backupCopyDir(walSrc, walDst); err != nil {
				slog.Warn("backup: failed to copy wal directory", "err", err)
			}
		}

		secretSrc := filepath.Join(s.dataDir, "auth_secret")
		secretDst := filepath.Join(req.OutputDir, "auth_secret")
		if _, err := os.Stat(secretSrc); err == nil {
			if err := backupCopyFile(secretSrc, secretDst); err != nil {
				slog.Warn("backup: failed to copy auth_secret", "err", err)
			}
		}
	}

	elapsed := time.Since(start)
	size := backupDirSize(req.OutputDir)

	slog.Info("backup: complete", "output", req.OutputDir, "size", size, "elapsed", elapsed)
	s.EmitAudit(r, "backup.trigger", "system", "backup", "ok", nil)

	s.sendJSON(w, http.StatusOK, BackupResponse{
		OutputDir: req.OutputDir,
		SizeBytes: size,
		Elapsed:   elapsed.Round(time.Millisecond).String(),
	})
}

func backupCopyFile(src, dst string) error {
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

func backupCopyDir(src, dst string) error {
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
		return backupCopyFile(path, target)
	})
}

// backupVerifyCheckpoint opens a Pebble checkpoint in read-only mode and scans
// a few keys to confirm the data is not corrupted.
func backupVerifyCheckpoint(checkpointDir string) error {
	verifyDB, err := pebble.Open(checkpointDir, &pebble.Options{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("open checkpoint: %w", err)
	}
	defer verifyDB.Close()

	iter, err := verifyDB.NewIter(nil)
	if err != nil {
		return fmt.Errorf("create iterator: %w", err)
	}
	defer iter.Close()

	const maxScan = 10
	n := 0
	for iter.First(); iter.Valid() && n < maxScan; iter.Next() {
		n++
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("scan keys: %w", err)
	}
	return nil
}

func backupDirSize(dir string) int64 {
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
