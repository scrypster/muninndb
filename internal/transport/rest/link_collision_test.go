package rest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestHandleLink_ACollisionIs409NamingBothRelations: a link that would replace
// an edge of another relation in the same direction at the same weight (STO-15)
// is the caller's to change, so REST answers 409 with the refusal that names
// both relations and the way out. It used to be a 500 with "an internal error
// occurred", an ERROR in the server log, and a reason clients retry on. The
// same link in the other direction is another key and is written. Real engine,
// invented fixtures (a harbour pilot station).
func TestHandleLink_ACollisionIs409NamingBothRelations(t *testing.T) {
	env := newScopeEnv(t)
	full := context.WithValue(context.Background(), auth.ContextMode, auth.ModeFull)
	write := func(content string) string {
		r, err := env.eng.Write(full, &mbp.WriteRequest{Vault: env.public, Concept: "pilot boarding", Content: content})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	target := write("Pilots board at the outer buoy.")
	succ := write("Pilots board at the inner buoy from now on.")
	post := func(src, dst string, rel uint16) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"vault": env.public, "source_id": src, "target_id": dst, "rel_type": rel, "weight": 0.8})
		resp, err := http.Post(env.url+"/api/link?vault="+env.public, "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post(succ, target, 4); code != http.StatusOK {
		t.Fatalf("supersedes link: %d %s", code, body)
	}
	code, body := post(succ, target, 5)
	if code != http.StatusConflict {
		t.Fatalf("relates_to at the same weight in the same direction: %d %s, want 409", code, body)
	}
	t.Logf("409 body: %s", body)
	for _, want := range []string{"supersedes", "relates_to", "nothing was written", "at a different weight"} {
		if !strings.Contains(body, want) {
			t.Errorf("409 body %s does not say %q", body, want)
		}
	}
	if strings.Contains(body, "an internal error occurred") {
		t.Errorf("409 body %s hides the reason", body)
	}
	if code, body := post(target, succ, 5); code != http.StatusOK {
		t.Errorf("relates_to in the other direction: %d %s, want 200 (another key)", code, body)
	}
}
