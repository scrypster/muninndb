//go:build localassets

package embed

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalProvider_Name(t *testing.T) {
	p := &LocalProvider{}
	if p.Name() != "local" {
		t.Errorf("expected name 'local', got %q", p.Name())
	}
}

func TestLocalProvider_MaxBatchSize(t *testing.T) {
	p := &LocalProvider{}
	if p.MaxBatchSize() != localMaxBatch {
		t.Errorf("expected batch size %d, got %d", localMaxBatch, p.MaxBatchSize())
	}
}

func TestLocalProvider_Close_NilSession(t *testing.T) {
	p := &LocalProvider{}
	err := p.Close()
	if err != nil {
		t.Errorf("Close with nil session failed: %v", err)
	}
}

func TestAtomicWrite_Success(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "test.txt")
	data := []byte("hello world")

	err := atomicWrite(dest, data)
	if err != nil {
		t.Fatalf("atomicWrite failed: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("expected %q, got %q", data, got)
	}
}

func TestAtomicWrite_Overwrite(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "test.txt")

	if err := atomicWrite(dest, []byte("first")); err != nil {
		t.Fatalf("first atomicWrite failed: %v", err)
	}
	if err := atomicWrite(dest, []byte("second")); err != nil {
		t.Fatalf("second atomicWrite failed: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("expected 'second', got %q", got)
	}
}

func TestAtomicWrite_InvalidDir(t *testing.T) {
	err := atomicWrite("/nonexistent/path/file.txt", []byte("data"))
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestReadAll(t *testing.T) {
	data := []byte("test data for readAll")
	got, err := readAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("readAll failed: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("expected %q, got %q", data, got)
	}
}

func TestReadAll_Empty(t *testing.T) {
	got, err := readAll(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("readAll failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d bytes", len(got))
	}
}

func TestLocalAvailable(t *testing.T) {
	_ = LocalAvailable()
}
