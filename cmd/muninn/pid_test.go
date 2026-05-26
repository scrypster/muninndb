package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPIDFileWriteRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muninn.pid")

	if err := writePID(path, 12345); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	pid, err := readPID(path)
	if err != nil {
		t.Fatalf("readPID: %v", err)
	}
	if pid != 12345 {
		t.Errorf("pid = %d, want 12345", pid)
	}
}

func TestReadPIDMissingFile(t *testing.T) {
	_, err := readPID("/nonexistent/path/pid")
	if err == nil {
		t.Error("expected error for missing PID file")
	}
}

func TestWriteReadAddrsFile(t *testing.T) {
	dir := t.TempDir()
	want := daemonAddrs{
		RestAddr: "127.0.0.1:8475",
		MCPAddr:  "0.0.0.0:8760",
		UIAddr:   "127.0.0.1:8476",
	}
	if err := writeAddrsFile(dir, want); err != nil {
		t.Fatalf("writeAddrsFile: %v", err)
	}
	got, err := readAddrsFile(dir)
	if err != nil {
		t.Fatalf("readAddrsFile: %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestReadAddrsMissingFile(t *testing.T) {
	_, err := readAddrsFile("/nonexistent/dir")
	if err == nil {
		t.Error("expected error for missing addrs file")
	}
}

func TestWriteAddrsFileOverwrites(t *testing.T) {
	dir := t.TempDir()
	first := daemonAddrs{RestAddr: "127.0.0.1:8475", MCPAddr: "127.0.0.1:8750", UIAddr: "127.0.0.1:8476"}
	second := daemonAddrs{RestAddr: "127.0.0.1:8475", MCPAddr: "0.0.0.0:8760", UIAddr: "127.0.0.1:9000"}
	if err := writeAddrsFile(dir, first); err != nil {
		t.Fatalf("writeAddrsFile: %v", err)
	}
	if err := writeAddrsFile(dir, second); err != nil {
		t.Fatalf("writeAddrsFile overwrite: %v", err)
	}
	got, err := readAddrsFile(dir)
	if err != nil {
		t.Fatalf("readAddrsFile: %v", err)
	}
	if got != second {
		t.Errorf("got %+v, want %+v", got, second)
	}
}

func TestWriteReadAddrsFile_Scheme(t *testing.T) {
	dir := t.TempDir()
	want := daemonAddrs{
		Scheme:   "https",
		RestAddr: "127.0.0.1:8475",
		MCPAddr:  "127.0.0.1:8750",
		UIAddr:   "127.0.0.1:8476",
	}
	if err := writeAddrsFile(dir, want); err != nil {
		t.Fatalf("writeAddrsFile: %v", err)
	}
	got, err := readAddrsFile(dir)
	if err != nil {
		t.Fatalf("readAddrsFile: %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestReadAddrsFile_OldFormatNoScheme(t *testing.T) {
	// A sidecar written by an older daemon has no "scheme" key. readAddrsFile
	// must tolerate it and leave Scheme empty (callers treat empty as "http").
	dir := t.TempDir()
	blob := `{"rest_addr":"127.0.0.1:8475","mcp_addr":"127.0.0.1:8750","ui_addr":"127.0.0.1:8476"}`
	if err := os.WriteFile(filepath.Join(dir, addrsFileName), []byte(blob), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := readAddrsFile(dir)
	if err != nil {
		t.Fatalf("readAddrsFile: %v", err)
	}
	if got.Scheme != "" {
		t.Errorf("Scheme = %q, want empty for an old-format sidecar", got.Scheme)
	}
	if got.RestAddr != "127.0.0.1:8475" {
		t.Errorf("RestAddr = %q, want 127.0.0.1:8475", got.RestAddr)
	}
}

func TestSchemeFor(t *testing.T) {
	cases := []struct {
		cert, key, want string
	}{
		{"", "", "http"},
		{"cert.pem", "", "http"},
		{"", "key.pem", "http"},
		{"cert.pem", "key.pem", "https"},
	}
	for _, c := range cases {
		if got := schemeFor(c.cert, c.key); got != c.want {
			t.Errorf("schemeFor(%q, %q) = %q, want %q", c.cert, c.key, got, c.want)
		}
	}
}
