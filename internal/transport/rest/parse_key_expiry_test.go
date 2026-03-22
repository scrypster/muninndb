package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── parseKeyExpiry unit tests ─────────────────────────────────────────────────

func TestParseKeyExpiry_Days(t *testing.T) {
	before := time.Now()
	got, err := parseKeyExpiry("90d")
	after := time.Now()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should be roughly 90 days from now (within a second of tolerance).
	wantLow := before.AddDate(0, 0, 90)
	wantHigh := after.AddDate(0, 0, 90)
	if got.Before(wantLow) || got.After(wantHigh) {
		t.Errorf("got %v, want between %v and %v", got, wantLow, wantHigh)
	}
}

func TestParseKeyExpiry_Years(t *testing.T) {
	before := time.Now()
	got, err := parseKeyExpiry("1y")
	after := time.Now()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantLow := before.AddDate(1, 0, 0)
	wantHigh := after.AddDate(1, 0, 0)
	if got.Before(wantLow) || got.After(wantHigh) {
		t.Errorf("got %v, want between %v and %v", got, wantLow, wantHigh)
	}
}

func TestParseKeyExpiry_RFC3339(t *testing.T) {
	const input = "2027-01-01T00:00:00Z"
	want, _ := time.Parse(time.RFC3339, input)
	got, err := parseKeyExpiry(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseKeyExpiry_DateOnly(t *testing.T) {
	const input = "2027-01-01"
	want, _ := time.Parse("2006-01-02", input)
	got, err := parseKeyExpiry(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseKeyExpiry_PastDate(t *testing.T) {
	_, err := parseKeyExpiry("2020-01-01")
	if err == nil {
		t.Fatal("expected error for past date, got nil")
	}
	if !strings.Contains(err.Error(), "past") {
		t.Errorf("error should mention 'past', got: %v", err)
	}
}

func TestParseKeyExpiry_Invalid(t *testing.T) {
	_, err := parseKeyExpiry("garbage")
	if err == nil {
		t.Fatal("expected error for invalid input, got nil")
	}
}

func TestParseKeyExpiry_ZeroDays(t *testing.T) {
	// n must be > 0; "0d" should fall through to the format-error path.
	_, err := parseKeyExpiry("0d")
	if err == nil {
		t.Fatal("expected error for '0d' (n must be > 0), got nil")
	}
}

// ── REST handler error-path tests ─────────────────────────────────────────────
//
// Each engine type embeds MockEngine (defined in server_test.go) so all other
// EngineAPI methods are satisfied, and overrides exactly one method to inject a fault.

// updateStateErrEngine returns an error from UpdateState.
type updateStateErrEngine struct{ MockEngine }

func (e *updateStateErrEngine) UpdateState(_ context.Context, _, _, _, _ string) error {
	return fmt.Errorf("state transition error")
}

// listDeletedErrRESTEngine returns an error from ListDeleted.
type listDeletedErrRESTEngine struct{ MockEngine }

func (e *listDeletedErrRESTEngine) ListDeleted(_ context.Context, _ string, _ int) (*ListDeletedResponse, error) {
	return nil, fmt.Errorf("storage read error")
}

// retryEnrichErrRESTEngine returns an error from RetryEnrich.
type retryEnrichErrRESTEngine struct{ MockEngine }

func (e *retryEnrichErrRESTEngine) RetryEnrich(_ context.Context, _, _ string) (*RetryEnrichResponse, error) {
	return nil, fmt.Errorf("enrichment engine unavailable")
}

// evolveErrEngine returns an error from Evolve.
type evolveErrEngine struct{ MockEngine }

func (e *evolveErrEngine) Evolve(_ context.Context, _, _, _, _ string) (*EvolveResponse, error) {
	return nil, fmt.Errorf("evolve engine error")
}

// consolidateErrEngine returns an error from Consolidate.
type consolidateErrEngine struct{ MockEngine }

func (e *consolidateErrEngine) Consolidate(_ context.Context, _ string, _ []string, _ string) (*ConsolidateResponse, error) {
	return nil, fmt.Errorf("consolidate engine error")
}

// ── TestHandleSetState_EngineError ───────────────────────────────────────────

func TestHandleSetState_EngineError(t *testing.T) {
	server := NewServer("localhost:8080", &updateStateErrEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"state":"active"}`
	req := httptest.NewRequest(http.MethodPut, "/api/engrams/" + testEngramID + "/state", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on engine error, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// ── TestHandleListDeleted_EngineError ─────────────────────────────────────────

func TestHandleListDeleted_EngineError(t *testing.T) {
	server := NewServer("localhost:8080", &listDeletedErrRESTEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/deleted?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on engine error, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// ── TestHandleRetryEnrich_EngineError ─────────────────────────────────────────

func TestHandleRetryEnrich_EngineError(t *testing.T) {
	server := NewServer("localhost:8080", &retryEnrichErrRESTEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest(http.MethodPost, "/api/engrams/" + testEngramID + "/retry-enrich", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on engine error, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// ── TestHandleEvolve_EngineError ──────────────────────────────────────────────

func TestHandleEvolve_EngineError(t *testing.T) {
	server := NewServer("localhost:8080", &evolveErrEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"new_content":"updated content","reason":"fixing a bug"}`
	req := httptest.NewRequest(http.MethodPost, "/api/engrams/" + testEngramID + "/evolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on engine error, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// ── TestHandleConsolidate_EngineError ─────────────────────────────────────────

func TestHandleConsolidate_EngineError(t *testing.T) {
	server := NewServer("localhost:8080", &consolidateErrEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	payload := map[string]interface{}{
		"ids":            []string{"id-1", "id-2"},
		"merged_content": "consolidated content",
	}
	b, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/consolidate", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on engine error, got %d (body: %s)", w.Code, w.Body.String())
	}
}
