package main

import (
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
