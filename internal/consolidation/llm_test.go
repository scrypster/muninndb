package consolidation

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

type mockLLMProvider struct {
	name     string
	response string
	err      error
}

func (m *mockLLMProvider) Name() string { return m.name }
func (m *mockLLMProvider) Complete(ctx context.Context, system, user string) (string, error) {
	return m.response, m.err
}

func TestVaultTrustTier_LegalSkipped(t *testing.T) {
	cases := []string{"legal", "Legal:Contracts", "legal/nda"}
	for _, vault := range cases {
		if got := vaultTrustTier(vault); got != trustSkip {
			t.Errorf("vaultTrustTier(%q) = %d, want trustSkip (%d)", vault, got, trustSkip)
		}
	}
}

func TestVaultTrustTier_RestrictedVaults(t *testing.T) {
	cases := []string{"work", "personal", "Work", "Personal"}
	for _, vault := range cases {
		if got := vaultTrustTier(vault); got != trustRestricted {
			t.Errorf("vaultTrustTier(%q) = %d, want trustRestricted (%d)", vault, got, trustRestricted)
		}
	}
}

func TestVaultTrustTier_OpenVaults(t *testing.T) {
	cases := []string{"default", "projects", "global", "hobby"}
	for _, vault := range cases {
		if got := vaultTrustTier(vault); got != trustOpen {
			t.Errorf("vaultTrustTier(%q) = %d, want trustOpen (%d)", vault, got, trustOpen)
		}
	}
}

func TestResolveProvider_SkipVault(t *testing.T) {
	ollama := &mockLLMProvider{name: "ollama"}
	if got := resolveProvider("legal", ollama, nil, nil); got != nil {
		t.Errorf("resolveProvider(legal) = %v, want nil", got)
	}
}

func TestResolveProvider_RestrictedAllowsOllama(t *testing.T) {
	ollama := &mockLLMProvider{name: "ollama"}
	openai := &mockLLMProvider{name: "openai"}
	got := resolveProvider("work", ollama, nil, openai)
	if got != ollama {
		t.Errorf("resolveProvider(work, ollama, nil, openai) = %v, want ollama", got)
	}
}

func TestResolveProvider_RestrictedAllowsAnthropic(t *testing.T) {
	anthropic := &mockLLMProvider{name: "anthropic"}
	openai := &mockLLMProvider{name: "openai"}
	got := resolveProvider("personal", nil, anthropic, openai)
	if got != anthropic {
		t.Errorf("resolveProvider(personal, nil, anthropic, openai) = %v, want anthropic", got)
	}
}

func TestResolveProvider_RestrictedBlocksOpenAI(t *testing.T) {
	openai := &mockLLMProvider{name: "openai"}
	got := resolveProvider("work", nil, nil, openai)
	if got != nil {
		t.Errorf("resolveProvider(work, nil, nil, openai) = %v, want nil", got)
	}
}

func TestResolveProvider_OpenAllowsAny(t *testing.T) {
	openai := &mockLLMProvider{name: "openai"}
	got := resolveProvider("projects", nil, nil, openai)
	if got != openai {
		t.Errorf("resolveProvider(projects, nil, nil, openai) = %v, want openai", got)
	}
}

func TestResolveProvider_Priority(t *testing.T) {
	ollama := &mockLLMProvider{name: "ollama"}
	anthropic := &mockLLMProvider{name: "anthropic"}
	openai := &mockLLMProvider{name: "openai"}
	got := resolveProvider("default", ollama, anthropic, openai)
	if got != ollama {
		t.Errorf("resolveProvider(default, ollama, anthropic, openai) = %v, want ollama", got)
	}
}

func TestParseDreamResponse_ValidJSON(t *testing.T) {
	input := `{
		"merges": [{"source_ids": ["a", "b"], "reason": "duplicate"}],
		"contradictions": [{"kept_id": "a", "superseded_id": "c", "reason": "outdated"}],
		"cross_vault_suggestions": [{"source_id": "a", "target_vault": "work", "reason": "related"}],
		"stability_recommendations": [{"id": "a", "direction": "strengthen", "reason": "confirmed"}],
		"journal": "Merged 1 pair, resolved 1 contradiction."
	}`
	resp, err := parseDreamResponse(input)
	if err != nil {
		t.Fatalf("parseDreamResponse: %v", err)
	}
	if len(resp.Merges) != 1 {
		t.Errorf("Merges: got %d, want 1", len(resp.Merges))
	}
	if len(resp.Contradictions) != 1 {
		t.Errorf("Contradictions: got %d, want 1", len(resp.Contradictions))
	}
	if len(resp.CrossVaultSuggs) != 1 {
		t.Errorf("CrossVaultSuggs: got %d, want 1", len(resp.CrossVaultSuggs))
	}
	if len(resp.StabilityRecs) != 1 {
		t.Errorf("StabilityRecs: got %d, want 1", len(resp.StabilityRecs))
	}
	if resp.Journal != "Merged 1 pair, resolved 1 contradiction." {
		t.Errorf("Journal: got %q", resp.Journal)
	}
	// Verify round-trip fields
	if resp.Merges[0].SourceIDs[0] != "a" || resp.Merges[0].SourceIDs[1] != "b" {
		t.Errorf("Merges[0].SourceIDs: got %v", resp.Merges[0].SourceIDs)
	}
	if resp.Contradictions[0].SupersededID != "c" {
		t.Errorf("Contradictions[0].SupersededID: got %q", resp.Contradictions[0].SupersededID)
	}
}

func TestParseDreamResponse_MarkdownFenced(t *testing.T) {
	input := "```json\n" + `{
		"merges": [],
		"contradictions": [],
		"cross_vault_suggestions": [],
		"stability_recommendations": [],
		"journal": "Nothing to consolidate."
	}` + "\n```"
	resp, err := parseDreamResponse(input)
	if err != nil {
		t.Fatalf("parseDreamResponse (markdown): %v", err)
	}
	if resp.Journal != "Nothing to consolidate." {
		t.Errorf("Journal: got %q", resp.Journal)
	}
}

func TestParseDreamResponse_InvalidJSON(t *testing.T) {
	_, err := parseDreamResponse("this is not json at all")
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestRunPhase2b_NoProvider_Skipped(t *testing.T) {
	w := &Worker{
		Engine: &noopEngineInterface{},
	}
	report := &ConsolidationReport{}
	clusters := []DedupCluster{
		{Members: []ClusterMember{{ID: "1", Concept: "c", Content: "x"}}},
	}
	err := w.runPhase2bLLMConsolidation(context.Background(), nil, [8]byte{}, report, "default", clusters)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if report.LLMMerged != 0 {
		t.Errorf("LLMMerged: got %d, want 0", report.LLMMerged)
	}
	if report.LLMContradictions != 0 {
		t.Errorf("LLMContradictions: got %d, want 0", report.LLMContradictions)
	}
}

// noopEngineInterface is a minimal stub for tests that don't exercise engine calls.
type noopEngineInterface struct{}

func (n *noopEngineInterface) Store() *storage.PebbleStore                                     { return nil }
func (n *noopEngineInterface) ListVaults(ctx context.Context) ([]string, error)                 { return nil, nil }
func (n *noopEngineInterface) UpdateLifecycleState(ctx context.Context, vault, id, state string) error {
	return nil
}
