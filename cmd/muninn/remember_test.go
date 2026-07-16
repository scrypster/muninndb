package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rememberTestServer records the last request it saw and replies with the
// configured status and body.
type rememberTestServer struct {
	*httptest.Server
	lastMethod string
	lastPath   string
	lastAuth   string
	lastCT     string
	lastBody   []byte
}

func newRememberTestServer(t *testing.T, status int, respBody string) *rememberTestServer {
	t.Helper()
	rts := &rememberTestServer{}
	rts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rts.lastMethod = r.Method
		rts.lastPath = r.URL.Path
		rts.lastAuth = r.Header.Get("Authorization")
		rts.lastCT = r.Header.Get("Content-Type")
		body := new(bytes.Buffer)
		body.ReadFrom(r.Body) //nolint:errcheck
		rts.lastBody = body.Bytes()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(respBody)) //nolint:errcheck
	}))
	t.Cleanup(rts.Server.Close)
	return rts
}

// isolateRememberEnv points the verb at srv and gives the test an empty HOME
// so the default key file lookup cannot see a developer's real ~/.muninn.
func isolateRememberEnv(t *testing.T, srv *rememberTestServer) {
	t.Helper()
	t.Setenv("MUNINNDB_ADMIN_URL", srv.URL)
	t.Setenv("HOME", t.TempDir())
}

func writeKeyFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.key")
	if err := os.WriteFile(path, []byte("mk_testtoken\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRememberPostsEngram(t *testing.T) {
	srv := newRememberTestServer(t, http.StatusCreated,
		`{"id":"01TESTULID","created_at":123,"hint":"linked to 2 memories"}`)
	isolateRememberEnv(t, srv)

	contentPath := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(contentPath, []byte("full text of the memory\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := rememberMain([]string{
		"--concept", "test concept",
		"--content-file", contentPath,
		"--summary", "a summary",
		"--tags", "one, two",
		"--entities", "Alice:person,Widget",
		"--vault", "work",
		"--op-id", "retry-key-1",
		"--key-file", writeKeyFile(t, 0600),
	}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
	if got := stdout.String(); got != "01TESTULID\n" {
		t.Errorf("stdout = %q, want engram id line", got)
	}
	if !strings.Contains(stderr.String(), "linked to 2 memories") {
		t.Errorf("hint not surfaced on stderr: %q", stderr.String())
	}
	if srv.lastMethod != http.MethodPost || srv.lastPath != "/api/engrams" {
		t.Errorf("request = %s %s, want POST /api/engrams", srv.lastMethod, srv.lastPath)
	}
	if srv.lastAuth != "Bearer mk_testtoken" {
		t.Errorf("Authorization = %q", srv.lastAuth)
	}
	if !strings.HasPrefix(srv.lastCT, "application/json") {
		t.Errorf("Content-Type = %q", srv.lastCT)
	}

	var req rememberRequest
	if err := json.Unmarshal(srv.lastBody, &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if req.Concept != "test concept" || req.Content != "full text of the memory\n" {
		t.Errorf("concept/content = %q / %q", req.Concept, req.Content)
	}
	if req.Summary != "a summary" || req.Vault != "work" || req.IdempotentID != "retry-key-1" {
		t.Errorf("summary/vault/op-id = %q / %q / %q", req.Summary, req.Vault, req.IdempotentID)
	}
	if len(req.Tags) != 2 || req.Tags[0] != "one" || req.Tags[1] != "two" {
		t.Errorf("tags = %v", req.Tags)
	}
	if len(req.Entities) != 2 || req.Entities[0] != (rememberEntity{Name: "Alice", Type: "person"}) ||
		req.Entities[1] != (rememberEntity{Name: "Widget"}) {
		t.Errorf("entities = %v", req.Entities)
	}
}

func TestRememberStdinContent(t *testing.T) {
	srv := newRememberTestServer(t, http.StatusCreated, `{"id":"01FROMSTDIN","created_at":1}`)
	isolateRememberEnv(t, srv)

	var stdout, stderr bytes.Buffer
	code := rememberMain(
		[]string{"--concept", "piped", "--content-file", "-"},
		strings.NewReader("piped content"), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
	var req rememberRequest
	if err := json.Unmarshal(srv.lastBody, &req); err != nil {
		t.Fatal(err)
	}
	if req.Content != "piped content" {
		t.Errorf("content = %q", req.Content)
	}
	if srv.lastAuth != "" {
		t.Errorf("expected unauthenticated request without a key file, got Authorization %q", srv.lastAuth)
	}
}

func TestRememberUsageErrors(t *testing.T) {
	srv := newRememberTestServer(t, http.StatusCreated, `{"id":"01X","created_at":1}`)
	isolateRememberEnv(t, srv)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing concept", []string{"--content", "x"}, "--concept is required"},
		{"no content source", []string{"--concept", "c"}, "one of --content or --content-file"},
		{"both content sources", []string{"--concept", "c", "--content", "x", "--content-file", "y"}, "mutually exclusive"},
		{"positional arg", []string{"--concept", "c", "--content", "x", "stray"}, "unexpected argument"},
		{"empty entity name", []string{"--concept", "c", "--content", "x", "--entities", ":person"}, "invalid --entities"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := rememberMain(tc.args, strings.NewReader(""), &stdout, &stderr)
			if code != execExitUsage {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, execExitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestRememberEmptyContentRejected(t *testing.T) {
	srv := newRememberTestServer(t, http.StatusCreated, `{"id":"01X","created_at":1}`)
	isolateRememberEnv(t, srv)

	path := filepath.Join(t.TempDir(), "empty.md")
	if err := os.WriteFile(path, []byte("  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := rememberMain([]string{"--concept", "c", "--content-file", path},
		strings.NewReader(""), &stdout, &stderr)
	if code != execExitUsage || !strings.Contains(stderr.String(), "content is empty") {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
}

func TestRememberBodyCap(t *testing.T) {
	srv := newRememberTestServer(t, http.StatusCreated, `{"id":"01X","created_at":1}`)
	isolateRememberEnv(t, srv)

	var stdout, stderr bytes.Buffer
	code := rememberMain(
		[]string{"--concept", "big", "--content", strings.Repeat("a", rememberMaxBody+1)},
		strings.NewReader(""), &stdout, &stderr)
	if code != execExitError {
		t.Fatalf("exit code = %d, want %d", code, execExitError)
	}
	if !strings.Contains(stderr.String(), "4 MB") {
		t.Errorf("stderr should name the 4 MB cap: %q", stderr.String())
	}
	if srv.lastMethod != "" {
		t.Error("oversized body must be rejected before any request is sent")
	}
}

func TestRememberUnauthorizedHint(t *testing.T) {
	srv := newRememberTestServer(t, http.StatusUnauthorized, `{"error":"invalid api key"}`)
	isolateRememberEnv(t, srv)

	var stdout, stderr bytes.Buffer
	code := rememberMain(
		[]string{"--concept", "c", "--content", "x", "--key-file", writeKeyFile(t, 0600)},
		strings.NewReader(""), &stdout, &stderr)
	if code != execExitError {
		t.Fatalf("exit code = %d, want %d", code, execExitError)
	}
	out := stderr.String()
	if !strings.Contains(out, "invalid api key") || !strings.Contains(out, "api-key create") {
		t.Errorf("401 output should surface the server error and the key-creation hint: %q", out)
	}
}

func TestRememberNestedErrorShapeSurfaced(t *testing.T) {
	// Handlers (as opposed to auth middleware) return the nested error shape.
	srv := newRememberTestServer(t, http.StatusBadRequest,
		`{"error":{"code":4003,"message":"vault must match the authenticated request vault","request_id":"x"}}`)
	isolateRememberEnv(t, srv)

	var stdout, stderr bytes.Buffer
	code := rememberMain([]string{"--concept", "c", "--content", "x", "--vault", "other"},
		strings.NewReader(""), &stdout, &stderr)
	if code != execExitError {
		t.Fatalf("exit code = %d, want %d", code, execExitError)
	}
	if !strings.Contains(stderr.String(), "vault must match the authenticated request vault") {
		t.Errorf("nested error message not extracted: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "request_id") {
		t.Errorf("raw JSON leaked into the rendered error: %q", stderr.String())
	}
}

func TestRememberExplicitKeyFileMissing(t *testing.T) {
	srv := newRememberTestServer(t, http.StatusCreated, `{"id":"01X","created_at":1}`)
	isolateRememberEnv(t, srv)

	var stdout, stderr bytes.Buffer
	code := rememberMain(
		[]string{"--concept", "c", "--content", "x", "--key-file", filepath.Join(t.TempDir(), "absent.key")},
		strings.NewReader(""), &stdout, &stderr)
	if code != execExitError || !strings.Contains(stderr.String(), "read key file") {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if srv.lastMethod != "" {
		t.Error("missing explicit key file must fail before any request is sent")
	}
}

func TestRememberWorldReadableKeyWarns(t *testing.T) {
	srv := newRememberTestServer(t, http.StatusCreated, `{"id":"01X","created_at":1}`)
	isolateRememberEnv(t, srv)

	var stdout, stderr bytes.Buffer
	code := rememberMain(
		[]string{"--concept", "c", "--content", "x", "--key-file", writeKeyFile(t, 0644)},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "world-readable") {
		t.Errorf("expected world-readable warning, stderr = %q", stderr.String())
	}
}

func TestParseInlineEntities(t *testing.T) {
	got, err := parseInlineEntities("Alice:person, Bob ,  Muninn DB : project ")
	if err != nil {
		t.Fatal(err)
	}
	want := []rememberEntity{
		{Name: "Alice", Type: "person"},
		{Name: "Bob"},
		{Name: "Muninn DB", Type: "project"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entity %d = %v, want %v", i, got[i], want[i])
		}
	}
}
