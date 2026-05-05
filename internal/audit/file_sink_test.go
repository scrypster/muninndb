package audit_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/audit"
)

func TestFileSink_WritesNDJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	sink, err := audit.NewFileSink(path)
	if err != nil {
		t.Fatal(err)
	}

	events := []audit.AuditEvent{
		{Timestamp: time.Now().UTC(), Action: "vault.delete", Result: "ok", TargetType: "vault", TargetID: "prod"},
		{Timestamp: time.Now().UTC(), Action: "api_key.create", Result: "ok", TargetType: "api_key", TargetID: "key-1"},
	}
	for _, e := range events {
		if err := sink.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var decoded []audit.AuditEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e audit.AuditEvent
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatalf("invalid JSON line: %v", err)
		}
		decoded = append(decoded, e)
	}

	if len(decoded) != 2 {
		t.Fatalf("want 2 lines, got %d", len(decoded))
	}
	if decoded[0].Action != "vault.delete" {
		t.Errorf("want vault.delete, got %s", decoded[0].Action)
	}
	if decoded[1].Action != "api_key.create" {
		t.Errorf("want api_key.create, got %s", decoded[1].Action)
	}
}

func TestFileSink_AppendsOnReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	s1, err := audit.NewFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Write(audit.AuditEvent{Action: "first"})
	_ = s1.Close()

	s2, err := audit.NewFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Write(audit.AuditEvent{Action: "second"})
	_ = s2.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		if len(scanner.Bytes()) > 0 {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("want 2 lines after reopen, got %d", lines)
	}
}

func TestConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv("MUNINN_AUDIT_FILE", "")
	t.Setenv("MUNINN_AUDIT_STDOUT", "")
	t.Setenv("MUNINN_AUDIT_SYSLOG", "")
	t.Setenv("MUNINN_AUDIT_WEBHOOK_URL", "")
	t.Setenv("MUNINN_AUDIT_BUFFER", "")

	cfg := audit.ConfigFromEnv("/data")
	if cfg.FilePath != "/data/audit.log" {
		t.Errorf("want /data/audit.log, got %s", cfg.FilePath)
	}
	if cfg.Stdout {
		t.Error("stdout should be off by default")
	}
	if cfg.BufferSize != 4096 {
		t.Errorf("want 4096, got %d", cfg.BufferSize)
	}
}

func TestConfigFromEnv_DisableFile(t *testing.T) {
	t.Setenv("MUNINN_AUDIT_FILE", "-")
	cfg := audit.ConfigFromEnv("/data")
	if cfg.FilePath != "" {
		t.Errorf("want empty FilePath when set to -, got %s", cfg.FilePath)
	}
}

func TestConfigFromEnv_CustomPath(t *testing.T) {
	t.Setenv("MUNINN_AUDIT_FILE", "/var/log/muninn-audit.log")
	cfg := audit.ConfigFromEnv("/data")
	if cfg.FilePath != "/var/log/muninn-audit.log" {
		t.Errorf("want custom path, got %s", cfg.FilePath)
	}
}
