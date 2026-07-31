package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// reportingContradictionEngine implements the optional contradictionReporter
// upgrade so the handler's rich path can be exercised without a live engine.
type reportingContradictionEngine struct {
	fakeEngine
	report *ContradictionsReport
}

func (e *reportingContradictionEngine) GetContradictionReport(_ context.Context, _ string) (*ContradictionsReport, error) {
	return e.report, nil
}

const contradictionsRPC = `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_contradictions","arguments":{"vault":"default"}}}`

// TestHandleContradictions_PendingIsDistinguishableFromNone is the surface-level
// regression test for the defect: for ~30s after muninn_link(contradicts) the
// tool returned {"contradictions":[]}, which is exactly what a vault with no
// contradictions returns. Three evaluators read that as "the feature is dead".
func TestHandleContradictions_PendingIsDistinguishableFromNone(t *testing.T) {
	declared := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	pending := newTestServerWith(&reportingContradictionEngine{report: &ContradictionsReport{
		Contradictions: []ContradictionPair{{
			IDa: "A", ConceptA: "widget-color", IDb: "B", ConceptB: "widget-color-revised",
			Status: "pending_detection", DeclaredAt: &declared,
		}},
		PendingCount: 1,
		ScanComplete: true,
	}})
	none := newTestServerWith(&reportingContradictionEngine{report: &ContradictionsReport{
		ScanComplete: true,
	}})

	pendingBody := postRPC(t, pending, contradictionsRPC).Body.String()
	noneBody := postRPC(t, none, contradictionsRPC).Body.String()
	if pendingBody == noneBody {
		t.Fatalf("pending and empty vault produce identical responses:\n%s", pendingBody)
	}

	got := extractInnerJSON(t, decodeResp(t, pendingBody))
	if got["pending_count"] != float64(1) {
		t.Errorf("pending_count = %v, want 1", got["pending_count"])
	}
	list, _ := got["contradictions"].([]any)
	if len(list) != 1 {
		t.Fatalf("contradictions = %v, want one pending entry", got["contradictions"])
	}
	entry, _ := list[0].(map[string]any)
	if entry["status"] != "pending_detection" {
		t.Errorf("status = %v, want pending_detection", entry["status"])
	}
	if entry["concept_a"] != "widget-color" || entry["concept_b"] != "widget-color-revised" {
		t.Errorf("concepts = (%v,%v), want the two seeded concepts", entry["concept_a"], entry["concept_b"])
	}
	if _, present := entry["detected_at"]; present {
		t.Errorf("detected_at must be ABSENT while pending, got %v", entry["detected_at"])
	}
	if note, _ := got["note"].(string); !strings.Contains(note, "awaiting the detector") {
		t.Errorf("note = %q, want an explanation of the pending state", note)
	}

	// The genuinely-empty vault must say so, not just return an empty list.
	emptyGot := extractInnerJSON(t, decodeResp(t, noneBody))
	if note, _ := emptyGot["note"].(string); !strings.Contains(note, "no contradictions") {
		t.Errorf("empty-vault note = %q, want an explicit 'none' statement", note)
	}
	if emptyGot["pending_count"] != float64(0) {
		t.Errorf("empty-vault pending_count = %v, want 0", emptyGot["pending_count"])
	}
}

// TestHandleContradictions_NoZeroTimestamp pins that an unknown detection time
// is omitted rather than serialised as the Go zero time. detected_at came back
// as "0001-01-01T00:00:00Z" on every pair before this change.
func TestHandleContradictions_NoZeroTimestamp(t *testing.T) {
	detected := time.Date(2026, 7, 30, 9, 30, 0, 0, time.UTC)
	srv := newTestServerWith(&reportingContradictionEngine{report: &ContradictionsReport{
		Contradictions: []ContradictionPair{
			{IDa: "A", ConceptA: "a", IDb: "B", ConceptB: "b", Status: "detected", DetectedAt: &detected},
			{IDa: "C", ConceptA: "c", IDb: "D", ConceptB: "d", Status: "detected"}, // legacy marker, time unknown
		},
		DetectedCount: 2,
		ScanComplete:  true,
	}})
	body := postRPC(t, srv, contradictionsRPC).Body.String()
	if strings.Contains(body, "0001-01-01") {
		t.Fatalf("response serialises the Go zero time as a timestamp:\n%s", body)
	}
	got := extractInnerJSON(t, decodeResp(t, body))
	list, _ := got["contradictions"].([]any)
	if len(list) != 2 {
		t.Fatalf("contradictions = %v", got["contradictions"])
	}
	first, _ := list[0].(map[string]any)
	if first["detected_at"] != "2026-07-30T09:30:00Z" {
		t.Errorf("detected_at = %v, want the real flag time", first["detected_at"])
	}
	second, _ := list[1].(map[string]any)
	if _, present := second["detected_at"]; present {
		t.Errorf("unknown detected_at must be absent, got %v", second["detected_at"])
	}
}

// TestHandleContradictions_TruncatedScanIsAdmitted — a capped scan must never
// let the caller read "pending_count: 0" as "nothing is pending".
func TestHandleContradictions_TruncatedScanIsAdmitted(t *testing.T) {
	srv := newTestServerWith(&reportingContradictionEngine{report: &ContradictionsReport{
		ScanComplete: false,
	}})
	got := extractInnerJSON(t, decodeResp(t, postRPC(t, srv, contradictionsRPC).Body.String()))
	if got["scan_complete"] != false {
		t.Errorf("scan_complete = %v, want false", got["scan_complete"])
	}
	if note, _ := got["note"].(string); !strings.Contains(note, "lower bound") {
		t.Errorf("note = %q, want the truncation admitted", note)
	}
}

// TestHandleContradictions_LegacyEngineStillAnswers — an engine that does not
// implement the optional reporter keeps the original response shape.
func TestHandleContradictions_LegacyEngineStillAnswers(t *testing.T) {
	srv := newTestServerWith(&fakeEngine{})
	got := extractInnerJSON(t, decodeResp(t, postRPC(t, srv, contradictionsRPC).Body.String()))
	if _, ok := got["contradictions"]; !ok {
		t.Errorf("response missing \"contradictions\": %v", got)
	}
}
