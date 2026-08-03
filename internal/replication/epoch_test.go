package replication

import (
	"sync"
	"testing"

	"github.com/cockroachdb/pebble"
)

func openTestDB(t *testing.T, dir string) *pebble.DB {
	t.Helper()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	return db
}

func TestEpochStore_FreshStart(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer db.Close()

	s, err := NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore: %v", err)
	}

	if got := s.Load(); got != 0 {
		t.Errorf("Load() = %d, want 0", got)
	}
}

func TestEpochStore_Persistence(t *testing.T) {
	dir := t.TempDir()

	// First open: set epoch to 42
	db := openTestDB(t, dir)
	s, err := NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore: %v", err)
	}
	if _, err := s.Advance(42); err != nil {
		t.Fatalf("Advance(42): %v", err)
	}
	db.Close()

	// Reopen: epoch should still be 42
	db2 := openTestDB(t, dir)
	defer db2.Close()

	s2, err := NewEpochStore(db2)
	if err != nil {
		t.Fatalf("NewEpochStore (reopen): %v", err)
	}
	if got := s2.Load(); got != 42 {
		t.Errorf("Load() after reopen = %d, want 42", got)
	}
}

func TestEpochStore_CompareAndSet_Success(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer db.Close()

	s, err := NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore: %v", err)
	}

	if got := s.Load(); got != 0 {
		t.Fatalf("initial Load() = %d, want 0", got)
	}

	ok, err := s.CompareAndSet(0, 1)
	if err != nil {
		t.Fatalf("CompareAndSet(0,1): %v", err)
	}
	if !ok {
		t.Fatal("CompareAndSet(0,1) returned false, want true")
	}
	if got := s.Load(); got != 1 {
		t.Errorf("Load() = %d, want 1", got)
	}

	ok, err = s.CompareAndSet(1, 2)
	if err != nil {
		t.Fatalf("CompareAndSet(1,2): %v", err)
	}
	if !ok {
		t.Fatal("CompareAndSet(1,2) returned false, want true")
	}
	if got := s.Load(); got != 2 {
		t.Errorf("Load() = %d, want 2", got)
	}
}

func TestEpochStore_CompareAndSet_Fail(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer db.Close()

	s, err := NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore: %v", err)
	}

	ok, err := s.CompareAndSet(5, 6)
	if err != nil {
		t.Fatalf("CompareAndSet(5,6): %v", err)
	}
	if ok {
		t.Fatal("CompareAndSet(5,6) returned true, want false")
	}
	if got := s.Load(); got != 0 {
		t.Errorf("Load() = %d, want 0 (should be unchanged)", got)
	}
}

func TestEpochStore_Advance_OnlyIncreases(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer db.Close()

	s, err := NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore: %v", err)
	}

	if _, err := s.Advance(10); err != nil {
		t.Fatalf("Advance(10): %v", err)
	}
	if got := s.Load(); got != 10 {
		t.Errorf("Load() = %d, want 10", got)
	}

	// Lower value: no-op
	if _, err := s.Advance(5); err != nil {
		t.Fatalf("Advance(5): %v", err)
	}
	if got := s.Load(); got != 10 {
		t.Errorf("Load() after Advance(5) = %d, want 10 (no-op)", got)
	}

	// Higher value: should update
	if _, err := s.Advance(11); err != nil {
		t.Fatalf("Advance(11): %v", err)
	}
	if got := s.Load(); got != 11 {
		t.Errorf("Load() = %d, want 11", got)
	}
}

func TestEpochStore_CompareAndSet_Concurrent(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer db.Close()

	s, err := NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore: %v", err)
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wins := make(chan bool, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.CompareAndSet(0, 1)
			if err != nil {
				t.Errorf("CompareAndSet error: %v", err)
				return
			}
			if ok {
				wins <- true
			}
		}()
	}

	wg.Wait()
	close(wins)

	count := 0
	for range wins {
		count++
	}

	if count != 1 {
		t.Errorf("exactly 1 goroutine should win CAS, got %d", count)
	}
	if got := s.Load(); got != 1 {
		t.Errorf("Load() = %d, want 1", got)
	}
}

func TestEpochStore_PersistSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	// First session: chain CAS 0→1→2→3
	db := openTestDB(t, dir)
	s, err := NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore: %v", err)
	}

	for _, step := range [][2]uint64{{0, 1}, {1, 2}, {2, 3}} {
		ok, err := s.CompareAndSet(step[0], step[1])
		if err != nil {
			t.Fatalf("CompareAndSet(%d,%d): %v", step[0], step[1], err)
		}
		if !ok {
			t.Fatalf("CompareAndSet(%d,%d) failed unexpectedly", step[0], step[1])
		}
	}
	db.Close()

	// Reopen: epoch should be 3
	db2 := openTestDB(t, dir)
	defer db2.Close()

	s2, err := NewEpochStore(db2)
	if err != nil {
		t.Fatalf("NewEpochStore (reopen): %v", err)
	}
	if got := s2.Load(); got != 3 {
		t.Errorf("Load() after reopen = %d, want 3", got)
	}

	// One more CAS after reopen
	ok, err := s2.CompareAndSet(3, 4)
	if err != nil {
		t.Fatalf("CompareAndSet(3,4): %v", err)
	}
	if !ok {
		t.Fatal("CompareAndSet(3,4) returned false after reopen")
	}
	if got := s2.Load(); got != 4 {
		t.Errorf("Load() = %d, want 4", got)
	}
}
