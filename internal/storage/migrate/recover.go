package migrate

import (
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"
)

// ErrDBFromNewerBinary is returned by ForceRerunMigrations when the stored
// migration version is higher than this binary's MaxRegisteredVersion —
// resetting to 0 and re-applying only this binary's migrations would leave
// newer-schema data un-migrated against an older binary (downgrade-bypass).
var ErrDBFromNewerBinary = errors.New("DB was written by a newer binary; upgrade instead of downgrade-recover")

// ForceRerunMigrations resets the stored migration version to 0 (the "fresh
// DB" state), so the next Runner.Run re-applies every registered migration.
//
// This is the operator recovery path for a wedged or partial migration
// (#611, Task 7b / RT5): if a version was stamped but the operator needs to
// force a re-run (e.g. recover from a partial state left by a failed v3),
// they run `muninn start --force-migration-rerun`, which calls this helper
// and exits without starting the server. The next normal start then re-runs
// all migrations from version 1.
//
// Refuse-newer guard: before deleting the version key, the helper reads the
// stored version and compares it to MaxRegisteredVersion (the highest version
// THIS binary knows). If the stored version is higher, the helper REFUSES
// with ErrDBFromNewerBinary — the DB was last written by a newer binary, and
// resetting to 0 would let this older binary re-apply only its own (smaller)
// migration set against a newer schema, a downgrade-bypass surface. The
// operator must upgrade the binary, not recover with an older one.
//
// Re-running only re-applies migrations THIS binary knows about — if the DB
// was last written by a NEWER binary, do NOT use this flag; upgrade instead.
// Every registered migration is idempotent by contract (v1/v2/v3 each guard
// against already-migrated keys), so a re-run against a same-or-older schema
// DB cannot double-apply.
//
// Always back up the DB before a migration-bearing upgrade; use this flag to
// recover from a wedged/partial migration — not to downgrade.
//
// The recovery does NOT run migrations itself — it only resets the version
// marker so the existing Runner re-applies them on the next Open. This keeps
// the recovery path simple and reuses the hardened Runner rather than
// duplicating its fail-loud / per-step-stamp semantics.
func ForceRerunMigrations(db *pebble.DB) error {
	current, err := readMigrationVersion(db)
	if err != nil {
		return fmt.Errorf("read migration version before reset: %w", err)
	}
	if max := MaxRegisteredVersion(); current > max {
		return fmt.Errorf("%w (stored migration version %d > this binary's max %d)", ErrDBFromNewerBinary, current, max)
	}
	return db.Delete(migrationVersionKey, pebble.Sync)
}
