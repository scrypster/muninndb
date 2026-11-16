package auth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileAtomic_FailedWriteLeavesTargetUntouched pins the property the
// session secret depends on: a write that fails part-way leaves the previous
// file exactly as it was, or no file at all — never a truncated one. A blank
// auth_secret used to be accepted as a valid secret, and os.WriteFile truncates
// the target before it writes, so a full disk on first run produced one.
func TestWriteFileAtomic_FailedWriteLeavesTargetUntouched(t *testing.T) {
	errDiskFull := errors.New("injected: no space left on device")
	failPartWay := func(f *os.File, b []byte) error {
		if _, err := f.Write(b[:len(b)/2]); err != nil {
			return err
		}
		return errDiskFull
	}
	fresh := []byte("fresh-secret-fresh-secret-fresh!!")

	t.Run("existing file keeps its content", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "auth_secret")
		original := []byte("original-secret-original-secret!")
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatalf("seed: %v", err)
		}

		err := writeFileAtomic(path, fresh, failPartWay)
		if !errors.Is(err, errDiskFull) {
			t.Fatalf("err = %v; want the injected write error", err)
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("the target is gone after a failed write: %v", readErr)
		}
		if !bytes.Equal(got, original) {
			t.Fatalf("the target holds %d bytes %q after a failed write; want the original %q untouched", len(got), got, original)
		}
		assertOnlyFile(t, dir, "auth_secret")
	})

	t.Run("no file is created when there was none", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "auth_secret")

		err := writeFileAtomic(path, fresh, failPartWay)
		if !errors.Is(err, errDiskFull) {
			t.Fatalf("err = %v; want the injected write error", err)
		}
		if info, statErr := os.Stat(path); statErr == nil {
			t.Fatalf("a failed first write left a %d-byte file behind; want no file", info.Size())
		}
		assertOnlyFile(t, dir)
	})
}

// assertOnlyFile fails if dir holds anything other than the named files, so a
// leftover temporary file is caught.
func assertOnlyFile(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != len(want) {
		t.Fatalf("directory holds %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("directory holds %v; want %v", got, want)
		}
	}
}
