package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// muninn remember — thin client for POST /api/engrams on the RUNNING daemon.
//
// This is the online counterpart of `muninn exec remember` (which opens the
// store directly and therefore only works while the daemon is stopped). It
// rides the same trust boundary as every other key-authed REST client: a
// vault API key (mk_*, created with `muninn api-key create`) read from a
// 0600 key file — never the admin session, never a bare env var. See #610.

// Exit codes match muninn exec: 0 success, 1 usage, 2 error.

// rememberMaxBody mirrors the server's bodySizeMiddleware cap
// (internal/transport/rest/server.go). Checked client-side before the POST so
// an oversized --content-file fails with an explanation instead of an opaque
// "invalid request body" from the truncated read on the server.
const rememberMaxBody = 4 << 20 // 4 MB

// defaultRememberKeyFile is the standard location for the vault API key used
// by `muninn remember`. Deliberately NOT ~/.muninn/mcp.token: that file holds
// the MCP static bearer token (mdb_*), which the REST vault-auth layer does
// not accept — REST validates vault API keys (mk_*) only. Same file
// convention (0600, under ~/.muninn), different credential.
func defaultRememberKeyFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".muninn", "api.key")
}

// rememberRequest is the JSON body for POST /api/engrams. Field names match
// transport WriteRequest; kept as a local struct so the CLI stays a thin
// client of the wire format rather than importing engine types.
type rememberRequest struct {
	Concept      string           `json:"concept"`
	Content      string           `json:"content"`
	Tags         []string         `json:"tags,omitempty"`
	Vault        string           `json:"vault,omitempty"`
	IdempotentID string           `json:"idempotent_id,omitempty"`
	UpsertMode   bool             `json:"upsert_mode,omitempty"`
	Summary      string           `json:"summary,omitempty"`
	Entities     []rememberEntity `json:"entities,omitempty"`
}

type rememberEntity struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type rememberResponse struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	Hint      string `json:"hint,omitempty"`
}

func runRemember(args []string) {
	if code := rememberMain(args, os.Stdin, os.Stdout, os.Stderr); code != 0 {
		osExit(code)
	}
}

// rememberMain is the testable implementation. Returns an exit code.
func rememberMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("remember", flag.ContinueOnError)
	fs.SetOutput(stderr)

	concept := fs.String("concept", "", "short label for the memory (required)")
	content := fs.String("content", "", "full text of the memory (inline)")
	contentFile := fs.String("content-file", "", "read the full text from a file; '-' reads stdin")
	summary := fs.String("summary", "", "one-line summary (inline enrichment)")
	tags := fs.String("tags", "", "comma-separated tags")
	entities := fs.String("entities", "", "comma-separated inline entities, each Name or Name:type")
	vault := fs.String("vault", "", "vault name (default: derived from the API key, else \"default\")")
	opID := fs.String("op-id", "", "idempotency key — safe retries return the original engram id")
	upsertMode := fs.Bool("upsert-mode", false, "keep one stable memory per --op-id: create on first use, evolve it when the content changes, no-op when it is identical (requires --op-id)")
	keyFile := fs.String("key-file", "", "vault API key file (default: ~/.muninn/api.key)")

	if err := fs.Parse(args); err != nil {
		return execExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "muninn remember: unexpected argument %q (content goes in --content or --content-file)\n", fs.Arg(0))
		return execExitUsage
	}
	if strings.TrimSpace(*concept) == "" {
		fmt.Fprintln(stderr, "muninn remember: --concept is required")
		return execExitUsage
	}
	if *content != "" && *contentFile != "" {
		fmt.Fprintln(stderr, "muninn remember: --content and --content-file are mutually exclusive")
		return execExitUsage
	}
	if *content == "" && *contentFile == "" {
		fmt.Fprintln(stderr, "muninn remember: one of --content or --content-file is required")
		return execExitUsage
	}
	if *upsertMode && strings.TrimSpace(*opID) == "" {
		fmt.Fprintln(stderr, "muninn remember: --upsert-mode requires --op-id (the key the engram is pinned to)")
		return execExitUsage
	}

	body := *content
	if *contentFile != "" {
		var data []byte
		var err error
		if *contentFile == "-" {
			data, err = io.ReadAll(stdin)
		} else {
			data, err = os.ReadFile(*contentFile)
		}
		if err != nil {
			fmt.Fprintf(stderr, "muninn remember: read content: %v\n", err)
			return execExitError
		}
		body = string(data)
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintln(stderr, "muninn remember: content is empty")
		return execExitUsage
	}

	ents, err := parseInlineEntities(*entities)
	if err != nil {
		fmt.Fprintf(stderr, "muninn remember: %v\n", err)
		return execExitUsage
	}

	req := rememberRequest{
		Concept:      strings.TrimSpace(*concept),
		Content:      body,
		Tags:         splitCommaList(*tags),
		Vault:        strings.TrimSpace(*vault),
		IdempotentID: strings.TrimSpace(*opID),
		UpsertMode:   *upsertMode,
		Summary:      *summary,
		Entities:     ents,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(stderr, "muninn remember: encode request: %v\n", err)
		return execExitError
	}
	if len(payload) > rememberMaxBody {
		fmt.Fprintf(stderr,
			"muninn remember: request body is %d bytes; the REST API caps request bodies at 4 MB\n"+
				"  Store a summary plus a pointer to the original file, or split the content.\n",
			len(payload))
		return execExitError
	}

	token, code := rememberToken(*keyFile, stderr)
	if code != 0 {
		return code
	}

	url := rememberBaseURL() + "/api/engrams"
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(stderr, "muninn remember: build request: %v\n", err)
		return execExitError
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClientForURL(url, 60*time.Second).Do(httpReq)
	if err != nil {
		if isTLSCertError(err) {
			fmt.Fprintf(stderr, "muninn remember: TLS certificate verification failed for %s: %v\n", url, err)
			fmt.Fprintln(stderr, "Install the server's CA into the system trust store, or point MUNINNDB_ADMIN_URL at a loopback address.")
			return execExitError
		}
		fmt.Fprintf(stderr, "muninn remember: error connecting to server: %v\n", err)
		fmt.Fprintln(stderr, "Is muninn running? Try: muninn start")
		return execExitError
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		fmt.Fprintf(stderr, "muninn remember: read response: %v\n", err)
		return execExitError
	}

	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var wr rememberResponse
		if err := json.Unmarshal(respBody, &wr); err != nil || wr.ID == "" {
			fmt.Fprintf(stderr, "muninn remember: unexpected response: %s\n", strings.TrimSpace(string(respBody)))
			return execExitError
		}
		fmt.Fprintln(stdout, wr.ID)
		if wr.Hint != "" {
			fmt.Fprintf(stderr, "hint: %s\n", wr.Hint)
		}
		return execExitSuccess
	}

	printRememberHTTPError(stderr, resp.StatusCode, respBody, req.Vault, *keyFile)
	return execExitError
}

// rememberToken resolves the vault API key. An explicitly passed --key-file
// that cannot be read is an error; a missing DEFAULT key file means "no auth"
// (public-vault deployments — same semantics as the shipped curl guidance).
// Returns (token, exitCode); exitCode is non-zero only on error.
func rememberToken(keyFile string, stderr io.Writer) (string, int) {
	explicit := keyFile != ""
	if !explicit {
		keyFile = defaultRememberKeyFile()
	}
	data, err := os.ReadFile(keyFile)
	if err != nil {
		if explicit {
			fmt.Fprintf(stderr, "muninn remember: read key file: %v\n", err)
			return "", execExitError
		}
		return "", 0 // no default key file → unauthenticated (public vault)
	}
	if info, statErr := os.Stat(keyFile); statErr == nil && info.Mode().Perm()&0o044 != 0 {
		fmt.Fprintf(stderr, "warning: %s is world-readable — consider: chmod 600 %s\n", keyFile, keyFile)
	}
	return strings.TrimSpace(string(data)), 0
}

// rememberBaseURL resolves the REST base URL the same way the other
// daemon-client commands do: MUNINNDB_ADMIN_URL verbatim if set, else the
// scheme and REST port advertised by the running daemon's addrs file.
func rememberBaseURL() string {
	addrs, _ := readAddrsFile(defaultDataDir())
	restPort := defaultRESTPort
	if addrs.RestAddr != "" {
		if _, p, err := net.SplitHostPort(addrs.RestAddr); err == nil && p != "" {
			restPort = p
		}
	}
	return healthURL("MUNINNDB_ADMIN_URL", addrs.Scheme, restPort)
}

// printRememberHTTPError renders a non-2xx response with actionable hints.
// The REST API uses two error shapes — auth middleware returns a flat
// {"error":"...","code":"..."} while handlers return a nested
// {"error":{"code":4003,"message":"..."}} — so try both before falling back
// to the raw body.
func printRememberHTTPError(stderr io.Writer, status int, respBody []byte, vault, keyFile string) {
	var flat struct {
		Error json.RawMessage `json:"error"`
	}
	var msg string
	if json.Unmarshal(respBody, &flat) == nil && len(flat.Error) > 0 {
		var s string
		var nested struct {
			Message string `json:"message"`
		}
		switch {
		case json.Unmarshal(flat.Error, &s) == nil:
			msg = s
		case json.Unmarshal(flat.Error, &nested) == nil && nested.Message != "":
			msg = nested.Message
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(respBody))
	}
	fmt.Fprintf(stderr, "muninn remember: HTTP %d — %s\n", status, msg)

	switch {
	case status == http.StatusUnauthorized:
		if vault == "" {
			vault = "default"
		}
		if keyFile == "" {
			keyFile = defaultRememberKeyFile()
		}
		fmt.Fprintf(stderr, "  The REST API authenticates with a vault API key (mk_*), not the MCP token.\n")
		fmt.Fprintf(stderr, "  Create one:  muninn api-key create --vault %s --label cli\n", vault)
		fmt.Fprintf(stderr, "  Store it:    printf '%%s\\n' 'mk_...' > %s && chmod 600 %s\n", keyFile, keyFile)
	case status == http.StatusRequestEntityTooLarge:
		fmt.Fprintln(stderr, "  The REST API caps request bodies at 4 MB.")
	}
}

// splitCommaList splits a comma-separated flag value, trimming whitespace and
// dropping empty items. Returns nil for an empty input so the JSON field is
// omitted entirely.
func splitCommaList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseInlineEntities parses --entities "Name,Other Name:type" into inline
// entity records. The type after the LAST colon is optional; names may not be
// empty.
func parseInlineEntities(s string) ([]rememberEntity, error) {
	items := splitCommaList(s)
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]rememberEntity, 0, len(items))
	for _, item := range items {
		name, typ := item, ""
		if i := strings.LastIndex(item, ":"); i >= 0 {
			name, typ = strings.TrimSpace(item[:i]), strings.TrimSpace(item[i+1:])
		}
		if name == "" {
			return nil, fmt.Errorf("invalid --entities item %q (want Name or Name:type)", item)
		}
		out = append(out, rememberEntity{Name: name, Type: typ})
	}
	return out, nil
}
