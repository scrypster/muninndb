package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/audit"
)

func writeTestAuditLog(t *testing.T, events []audit.AuditEvent) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	sink, err := audit.NewFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if err := sink.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	_ = sink.Close()
	return path
}

func TestRunAuditStats_PrintsCounts(t *testing.T) {
	events := []audit.AuditEvent{
		{Timestamp: time.Now().UTC(), Action: "vault.delete", Result: "ok"},
		{Timestamp: time.Now().UTC(), Action: "vault.delete", Result: "ok"},
		{Timestamp: time.Now().UTC(), Action: "api_key.create", Result: "ok"},
		{Timestamp: time.Now().UTC(), Action: "auth.login_failed", Result: "denied"},
	}
	path := writeTestAuditLog(t, events)

	var out strings.Builder
	runAuditStats(path, &out)
	output := out.String()

	if !strings.Contains(output, "vault.delete") {
		t.Errorf("expected vault.delete in output: %s", output)
	}
	if !strings.Contains(output, "2") {
		t.Errorf("expected count 2 in output: %s", output)
	}
}

func TestRunAuditExport_WritesJSON(t *testing.T) {
	now := time.Now().UTC()
	events := []audit.AuditEvent{
		{Timestamp: now, Action: "vault.delete", Result: "ok", TargetID: "myvault"},
	}
	path := writeTestAuditLog(t, events)

	var out strings.Builder
	err := runAuditExport(path, "", "", "json", &out)
	if err != nil {
		t.Fatal(err)
	}

	var decoded []audit.AuditEvent
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if len(decoded) != 1 {
		t.Fatalf("want 1 event, got %d", len(decoded))
	}
	if decoded[0].Action != "vault.delete" {
		t.Errorf("want vault.delete, got %s", decoded[0].Action)
	}
}

func TestRunAuditExport_FiltersSince(t *testing.T) {
	old := time.Now().UTC().Add(-2 * time.Hour)
	recent := time.Now().UTC()
	events := []audit.AuditEvent{
		{Timestamp: old, Action: "api_key.create", Result: "ok"},
		{Timestamp: recent, Action: "vault.delete", Result: "ok"},
	}
	path := writeTestAuditLog(t, events)

	cutoff := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	var out strings.Builder
	err := runAuditExport(path, cutoff, "", "json", &out)
	if err != nil {
		t.Fatal(err)
	}

	var decoded []audit.AuditEvent
	_ = json.Unmarshal([]byte(out.String()), &decoded)
	if len(decoded) != 1 {
		t.Fatalf("want 1 event after cutoff, got %d", len(decoded))
	}
	if decoded[0].Action != "vault.delete" {
		t.Errorf("want vault.delete, got %s", decoded[0].Action)
	}
}
