package migrate

import "github.com/cockroachdb/pebble"

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
// Resetting to 0 rather than (max-1) is the safer choice: every registered
// migration is idempotent by contract (v1/v2/v3 each guard against
// already-migrated keys), so re-running the full set cannot double-apply.
//
// Always back up the DB before a migration-bearing upgrade; use this flag to
// recover from a wedged/partial migration.
//
// The recovery does NOT run migrations itself — it only resets the version
// marker so the existing Runner re-applies them on the next Open. This keeps
// the recovery path simple and reuses the hardened Runner rather than
// duplicating its fail-loud / per-step-stamp semantics.
func ForceRerunMigrations(db *pebble.DB) error {
	return db.Delete(migrationVersionKey, pebble.Sync)
}
