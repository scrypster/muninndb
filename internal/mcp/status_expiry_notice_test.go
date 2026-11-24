package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
)

// statusPayloadWithConfig seeds a vault config into a fresh store, then drives
// muninn_status as a full-mode mk_ key scoped to that vault, and decodes the
// tool's textContent JSON payload into a map.
func statusPayloadWithConfig(t *testing.T, cfg auth.VaultConfig) map[string]any {
	t.Helper()
	store := newWorkflowTestStore(t)
	if err := store.SetVaultConfig(cfg); err != nil {
		t.Fatalf("seed vault config: %v", err)
	}
	mkToken, _, err := store.GenerateAPIKey(cfg.Name, "admin", auth.ModeFull, nil)
	if err != nil {
		t.Fatalf("generate mk_ key: %v", err)
	}
	srv := New(":0", &fakeEngine{}, "", store, store, nil)

	w := doAuthenticatedPost(srv, mkToken, mkToolCallBody("muninn_status", map[string]any{}))
	result := decodeRPCResult(t, w)
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("muninn_status result has no content block: %v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] is not an object: %T", content[0])
	}
	text, _ := first["text"].(string)
	var status map[string]any
	if err := json.Unmarshal([]byte(text), &status); err != nil {
		t.Fatalf("unmarshal muninn_status payload: %v\n%s", err, text)
	}
	return status
}

// TestStatus_WorkflowVaultExpiryNotice pins muninn_status as the ONLY surface
// of the workflow-vault expiry notice (#973): a wf-* vault whose config
// carries an expiry inside the final 24 hours — or already past it, not yet
// reclaimed — reports expires_at and a notice steering the caller to a fresh
// vault. Everything else stays silent: outside the window, nil expiry, and
// above all any vault without the wf- prefix, expiry or not, so an operator
// vault can never see a notice.
func TestStatus_WorkflowVaultExpiryNotice(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		vault      string
		expiresAt  *time.Time
		wantNotice bool
	}{
		{"wf inside final 24h", "wf-inside", timePtr(now.Add(2 * time.Hour)), true},
		{"wf past expiry, not yet reclaimed", "wf-past", timePtr(now.Add(-1 * time.Hour)), true},
		{"wf outside window", "wf-far", timePtr(now.Add(72 * time.Hour)), false},
		{"wf nil expiry", "wf-noexp", nil, false},
		{"operator vault with past expiry", "opsprod", timePtr(now.Add(-1 * time.Hour)), false},
		{"operator vault nil expiry", "default", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := statusPayloadWithConfig(t, auth.VaultConfig{
				Name: tc.vault, Public: true, ExpiresAt: tc.expiresAt,
			})

			_, hasExpiry := status["expires_at"]
			notice, _ := status["notice"].(string)
			if tc.wantNotice {
				if !hasExpiry {
					t.Errorf("muninn_status for %q carries no expires_at, want one inside the notice window", tc.vault)
				}
				if notice == "" {
					t.Errorf("muninn_status for %q carries no notice, want the move-to-a-fresh-vault notice; payload: %v", tc.vault, status)
				} else if !strings.Contains(notice, "muninn_create_workflow_vault") {
					t.Errorf("notice %q does not name muninn_create_workflow_vault", notice)
				}
			} else {
				if hasExpiry {
					t.Errorf("muninn_status for %q carries expires_at, want none; payload: %v", tc.vault, status)
				}
				if notice != "" {
					t.Errorf("muninn_status for %q carries notice %q, want none — operator/no-expiry vaults never see a notice", tc.vault, notice)
				}
			}
		})
	}
}

func timePtr(t time.Time) *time.Time { return &t }
