package migrate

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

func openTestDB(t *testing.T) *pebble.DB {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRunner_FreshDB(t *testing.T) {
	db := openTestDB(t)
	r := NewRunner(db)

	var order []int
	for _, v := range []int{1, 2, 3} {
		v := v
		r.Register(Migration{
			Version:     v,
			Description: fmt.Sprintf("migration %d", v),
			Up: func(_ *pebble.DB) error {
				order = append(order, v)
				return nil
			},
		})
	}

	applied, err := r.Run()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if applied != 3 {
		t.Fatalf("applied = %d, want 3", applied)
	}
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("execution order = %v, want [1 2 3]", order)
	}
}

func TestRunner_PartiallyMigrated(t *testing.T) {
	db := openTestDB(t)

	// Simulate a DB already at migration version 2.
	if err := writeMigrationVersion(db, 2); err != nil {
		t.Fatal(err)
	}

	r := NewRunner(db)
	var ran []int
	for _, v := range []int{1, 2, 3} {
		v := v
		r.Register(Migration{
			Version:     v,
			Description: fmt.Sprintf("migration %d", v),
			Up: func(_ *pebble.DB) error {
				ran = append(ran, v)
				return nil
			},
		})
	}

	applied, err := r.Run()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want 1", applied)
	}
	if len(ran) != 1 || ran[0] != 3 {
		t.Fatalf("ran = %v, want [3]", ran)
	}
}

func TestRunner_FailedMigration(t *testing.T) {
	db := openTestDB(t)
	r := NewRunner(db)

	r.Register(Migration{Version: 1, Description: "ok", Up: func(_ *pebble.DB) error { return nil }})
	r.Register(Migration{Version: 2, Description: "boom", Up: func(_ *pebble.DB) error { return fmt.Errorf("disk full") }})
	r.Register(Migration{Version: 3, Description: "never", Up: func(_ *pebble.DB) error {
		t.Fatal("migration 3 should not run")
		return nil
	}})

	applied, err := r.Run()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want 1 (only migration 1 should succeed)", applied)
	}

	// Version should be stuck at 1 (last successful).
	ver, err := readMigrationVersion(db)
	if err != nil {
		t.Fatal(err)
	}
	if ver != 1 {
		t.Fatalf("stored version = %d, want 1", ver)
	}
}

func TestRunner_NoMigrations(t *testing.T) {
	db := openTestDB(t)
	r := NewRunner(db)

	applied, err := r.Run()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if applied != 0 {
		t.Fatalf("applied = %d, want 0", applied)
	}
}

func TestRunner_Idempotent(t *testing.T) {
	db := openTestDB(t)

	runCount := 0
	makeMigration := func(v int) Migration {
		return Migration{
			Version:     v,
			Description: fmt.Sprintf("migration %d", v),
			Up: func(_ *pebble.DB) error {
				runCount++
				return nil
			},
		}
	}

	// First run: apply all.
	r1 := NewRunner(db)
	r1.Register(makeMigration(1))
	r1.Register(makeMigration(2))
	applied, err := r1.Run()
	if err != nil {
		t.Fatal(err)
	}
	if applied != 2 {
		t.Fatalf("first run: applied = %d, want 2", applied)
	}

	// Second run: same migrations, nothing to do.
	runCount = 0
	r2 := NewRunner(db)
	r2.Register(makeMigration(1))
	r2.Register(makeMigration(2))
	applied, err = r2.Run()
	if err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("second run: applied = %d, want 0", applied)
	}
	if runCount != 0 {
		t.Fatalf("second run: runCount = %d, want 0", runCount)
	}
}

func TestRunner_RefusesNewerThanKnown(t *testing.T) {
	db := openTestDB(t)
	// Simulate a DB written by a newer binary that registered migration v3.
	if err := writeMigrationVersion(db, 3); err != nil {
		t.Fatal(err)
	}
	// This (older) binary only knows about migrations v1 and v2.
	r := NewRunner(db)
	r.Register(Migration{Version: 1, Description: "v1", Up: func(*pebble.DB) error { return nil }})
	r.Register(Migration{Version: 2, Description: "v2", Up: func(*pebble.DB) error { return nil }})

	if _, err := r.Run(); err == nil {
		t.Fatal("expected error when stored migration version (3) > max registered (2), got nil")
	}
}

func TestRunner_OutOfOrder(t *testing.T) {
	db := openTestDB(t)
	r := NewRunner(db)

	var order []int
	for _, v := range []int{3, 1, 2} {
		v := v
		r.Register(Migration{
			Version:     v,
			Description: fmt.Sprintf("migration %d", v),
			Up: func(_ *pebble.DB) error {
				order = append(order, v)
				return nil
			},
		})
	}

	applied, err := r.Run()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if applied != 3 {
		t.Fatalf("applied = %d, want 3", applied)
	}
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("execution order = %v, want [1 2 3]", order)
	}
}
