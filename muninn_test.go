package muninn_test

import (
	"context"
	"testing"
	"time"

	muninn "github.com/scrypster/muninndb"
)

func openTestDB(t *testing.T) *muninn.DB {
	t.Helper()
	db, err := muninn.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpen_Remember_Recall(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	_, err := db.Remember(ctx, "default", "golang", "Go uses goroutines for concurrency.")
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	_, err = db.Remember(ctx, "default", "python", "Python uses the GIL for thread safety.")
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	_, err = db.Remember(ctx, "default", "rust", "Rust enforces memory safety at compile time.")
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	// FTS indexing is async; retry until the written engrams are visible.
	var results []muninn.Engram
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		results, err = db.Recall(ctx, "default", "goroutines concurrency", 5)
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if len(results) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected at least one result, got none after 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	found := false
	for _, e := range results {
		if e.Concept == "golang" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'golang' engram in results; got: %+v", results)
	}
}

func TestOpen_Read(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	id, err := db.Remember(ctx, "default", "test concept", "test content")
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	e, err := db.Read(ctx, "default", id)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if e.ID != id {
		t.Errorf("got ID %q, want %q", e.ID, id)
	}
	if e.Content != "test content" {
		t.Errorf("got content %q, want %q", e.Content, "test content")
	}
	if e.Concept != "test concept" {
		t.Errorf("got concept %q, want %q", e.Concept, "test concept")
	}
}

func TestOpen_Forget(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	id, err := db.Remember(ctx, "default", "ephemeral", "to be deleted")
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	if err := db.Forget(ctx, "default", id); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	_, err = db.Read(ctx, "default", id)
	if err != muninn.ErrNotFound {
		t.Errorf("expected ErrNotFound after Forget, got: %v", err)
	}
}

func TestOpen_Close_Reopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	db, err := muninn.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, err := db.Remember(ctx, "default", "durable", "should survive close")
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := muninn.Open(dir)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer db2.Close()

	e, err := db2.Read(ctx, "default", id)
	if err != nil {
		t.Fatalf("Read after reopen: %v", err)
	}
	if e.Content != "should survive close" {
		t.Errorf("durability failure: got %q", e.Content)
	}
}

func TestOpen_AlreadyLocked(t *testing.T) {
	dir := t.TempDir()

	db, err := muninn.Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer db.Close()

	_, err = muninn.Open(dir)
	if err == nil {
		t.Fatal("expected error when opening already-locked database, got nil")
	}
}
