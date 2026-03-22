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

// TestLocalAvailable asserts that the ONNX model and tokenizer were actually
// embedded at build time. This test is only compiled with -tags localassets,
// so a failure here means the assets are missing despite the build tag being
// set — which would indicate a fetch-assets / go:embed problem.
//
// Crucially, if any go build command for the muninn binary is missing
// -tags localassets, this test will not be compiled at all, meaning the
// regression silently escapes. The scripts/check-build-tags.sh static check
// closes that gap.
func TestLocalAvailable(t *testing.T) {
	if !LocalAvailable() {
		t.Fatal("LocalAvailable() returned false: ONNX model or tokenizer was not embedded at build time. " +
			"Run 'make fetch-assets' and rebuild with -tags localassets.")
	}
}
