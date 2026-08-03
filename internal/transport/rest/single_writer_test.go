package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func followerServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer("localhost:8080", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	s.SetWriteGate(func() error {
		return &mbp.NotLeaderError{Role: "lobe", LeaderID: "cortex-1", LeaderAddr: "10.0.0.1:8474"}
	})
	return s
}

// TestREST_WriteRoutesRefusedOnFollower is the REST half of #596. Every
// decorated write route must answer 421 with the leader hint instead of 201.
//
// It is also the anti-drift pin for the route list in server.go: a write route
// added without s.withCortexWrite is not covered here, and the omission is what
// this table is for.
func TestREST_WriteRoutesRefusedOnFollower(t *testing.T) {
	s := followerServer(t)

	cases := []struct{ method, path, body string }{
		{"POST", "/api/engrams", `{"content":"written on a lobe"}`},
		{"POST", "/api/engrams/batch", `{"engrams":[{"content":"c"}]}`},
		{"DELETE", "/api/engrams/01ARZ3NDEKTSV4RRFFQ69G5FAV", ``},
		{"POST", "/api/link", `{"source_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","target_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}`},
		{"POST", "/api/engrams/01ARZ3NDEKTSV4RRFFQ69G5FAV/evolve", `{"new_content":"x"}`},
		{"POST", "/api/consolidate", `{"ids":["01ARZ3NDEKTSV4RRFFQ69G5FAV"],"merged_content":"m"}`},
		{"POST", "/api/decide", `{"decision":"d","rationale":"r"}`},
		{"POST", "/api/engrams/01ARZ3NDEKTSV4RRFFQ69G5FAV/restore", ``},
		{"PUT", "/api/engrams/01ARZ3NDEKTSV4RRFFQ69G5FAV/state", `{"state":"archived"}`},
		{"PUT", "/api/engrams/01ARZ3NDEKTSV4RRFFQ69G5FAV/tags", `{"tags":["t"]}`},
		{"POST", "/api/engrams/01ARZ3NDEKTSV4RRFFQ69G5FAV/retry-enrich", ``},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)

			if w.Code != StatusNotCortex {
				t.Fatalf("status = %d, want %d (421 Misdirected Request) — a write "+
					"accepted on a Lobe is never replicated (#596). body: %s",
					w.Code, StatusNotCortex, w.Body.String())
			}
			if got := w.Header().Get(HeaderCortexID); got != "cortex-1" {
				t.Errorf("%s = %q, want cortex-1", HeaderCortexID, got)
			}
			if got := w.Header().Get(HeaderCortexAddr); got != "10.0.0.1:8474" {
				t.Errorf("%s = %q, want 10.0.0.1:8474", HeaderCortexAddr, got)
			}
			var resp ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if resp.Error.Code != mbp.ErrNotCortex {
				t.Errorf("error code = %d, want %d", resp.Error.Code, mbp.ErrNotCortex)
			}
			// 421 is below 500, so sendError must NOT scrub the message — the
			// leader hint has to survive into the body.
			if !strings.Contains(resp.Error.Message, "cortex-1") {
				t.Errorf("message must name the Cortex, got %q", resp.Error.Message)
			}
		})
	}
}

// TestREST_ReadRoutesUnaffectedOnFollower: serving reads is what a Lobe is FOR.
// The gate must not touch them.
func TestREST_ReadRoutesUnaffectedOnFollower(t *testing.T) {
	s := followerServer(t)
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/activate", `{"context":"anything"}`},
		{"GET", "/api/engrams/01ARZ3NDEKTSV4RRFFQ69G5FAV", ``},
		{"POST", "/api/traverse", `{"start_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			if w.Code == StatusNotCortex {
				t.Errorf("read route refused on a Lobe with 421 — reads are exactly "+
					"what a Lobe exists to serve. body: %s", w.Body.String())
			}
		})
	}
}

// TestREST_StandaloneUnaffected: with no gate installed (every non-cluster
// server) writes behave exactly as before.
func TestREST_StandaloneUnaffected(t *testing.T) {
	s := NewServer("localhost:8080", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	req := httptest.NewRequest("POST", "/api/engrams", strings.NewReader(`{"content":"standalone"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code == StatusNotCortex {
		t.Fatalf("standalone server refused a write with 421: %s", w.Body.String())
	}
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("standalone write status = %d, want 200/201: %s", w.Code, w.Body.String())
	}
}
