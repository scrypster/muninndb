package migrate

import (
	"testing"

	"github.com/cockroachdb/pebble"
)

// TestForceRerunMigrations_ResetsToZero stamps version 3, calls
// ForceRerunMigrations, and asserts the stored version is now 0 (the
// "fresh DB" state readMigrationVersion returns for a missing key).
func TestForceRerunMigrations_ResetsToZero(t *testing.T) {
	db := openTestDB(t)

	if err := writeMigrationVersion(db, 3); err != nil {
		t.Fatal(err)
	}
	if got, err := readMigrationVersion(db); err != nil || got != 3 {
		t.Fatalf("precondition: version=%d err=%v (want 3)", got, err)
	}

	if err := ForceRerunMigrations(db); err != nil {
		t.Fatalf("ForceRerunMigrations: %v", err)
	}

	got, err := readMigrationVersion(db)
	if err != nil {
		t.Fatalf("readMigrationVersion after reset: %v", err)
	}
	if got != 0 {
		t.Fatalf("stored version after reset = %d, want 0", got)
	}
}

// TestForceRerunMigrations_FreshDBIsNoOp asserts the helper is safe to run
// against a DB that has never had a migration version stamped (the common
// case where recovery is unnecessary but invoked defensively).
func TestForceRerunMigrations_FreshDBIsNoOp(t *testing.T) {
	db := openTestDB(t)

	if err := ForceRerunMigrations(db); err != nil {
		t.Fatalf("ForceRerunMigrations on fresh DB: %v", err)
	}
	got, err := readMigrationVersion(db)
	if err != nil {
		t.Fatalf("readMigrationVersion: %v", err)
	}
	if got != 0 {
		t.Fatalf("fresh DB version = %d, want 0", got)
	}
}

// TestForceRerunMigrations_RefusesNewerBinaryDB proves the downgrade-bypass
// guard: if the DB was last written by a NEWER binary (stored version > this
// binary's MaxRegisteredVersion), ForceRerunMigrations REFUSES instead of
// resetting to 0. Without this, an operator facing the refuse-newer error on
// startup could --force-migration-rerun to reset to 0, then an older binary
// would happily re-apply v1/v2/v3 against a newer-schema DB.
//
// The version key MUST remain at the newer-binary value (not deleted) so the
// next start with a properly-upgraded binary still sees the correct version.
func TestForceRerunMigrations_RefusesNewerBinaryDB(t *testing.T) {
	db := openTestDB(t)

	newerVersion := MaxRegisteredVersion() + 10 // simulate a future vN+10 binary
	if err := writeMigrationVersion(db, newerVersion); err != nil {
		t.Fatal(err)
	}

	if err := ForceRerunMigrations(db); err == nil {
		t.Fatal("ForceRerunMigrations on a newer-binary DB: expected error (refuse-newer), got nil (downgrade-bypass surface)")
	}

	// Version MUST be unchanged — refuse-newer must NOT delete the version key.
	got, err := readMigrationVersion(db)
	if err != nil {
		t.Fatalf("readMigrationVersion after refused reset: %v", err)
	}
	if got != newerVersion {
		t.Fatalf("stored version after refused reset = %d, want %d (refuse-newer must not delete)", got, newerVersion)
	}
}

// TestForceRerunMigrations_NextRunReappliesAll stamps version 3, resets,
// then runs a fresh Runner with migrations v1/v2/v3 registered and asserts
// all three re-apply — proving the reset actually triggers a re-run on the
// next Open.
func TestForceRerunMigrations_NextRunReappliesAll(t *testing.T) {
	db := openTestDB(t)
	if err := writeMigrationVersion(db, 3); err != nil {
		t.Fatal(err)
	}

	if err := ForceRerunMigrations(db); err != nil {
		t.Fatalf("ForceRerunMigrations: %v", err)
	}

	r := NewRunner(db)
	var applied []int
	for _, v := range []int{1, 2, 3} {
		v := v
		r.Register(Migration{
			Version:     v,
			Description: "idempotent stub",
			Up: func(_ *pebble.DB) error {
				applied = append(applied, v)
				return nil
			},
		})
	}

	n, err := r.Run()
	if err != nil {
		t.Fatalf("Runner.Run after reset: %v", err)
	}
	if n != 3 {
		t.Fatalf("applied count = %d, want 3 (all migrations should re-run after reset)", n)
	}
	if len(applied) != 3 || applied[0] != 1 || applied[1] != 2 || applied[2] != 3 {
		t.Fatalf("applied order = %v, want [1 2 3]", applied)
	}

	// And the stored version should now be stamped back at the max registered.
	got, err := readMigrationVersion(db)
	if err != nil {
		t.Fatalf("readMigrationVersion after re-run: %v", err)
	}
	if got != 3 {
		t.Fatalf("stored version after re-run = %d, want 3", got)
	}
}
