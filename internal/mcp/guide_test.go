package mcp

import (
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

func TestGenerateGuide_AutonomousMode(t *testing.T) {
	r := auth.ResolvePlasticity(nil)
	guide := generateGuide("default", r, engineStats{EngramCount: 42, VaultCount: 1})

	if !strings.Contains(guide, "vault: default") {
		t.Error("guide should contain vault name")
	}
	if !strings.Contains(guide, "proactively remember") {
		t.Error("autonomous mode should mention proactive remembering")
	}
	if !strings.Contains(guide, "muninn_remember") {
		t.Error("guide should list muninn_remember tool")
	}
	if !strings.Contains(guide, "muninn_recall") {
		t.Error("guide should list muninn_recall tool")
	}
	if !strings.Contains(guide, "Memories stored: 42") {
		t.Error("guide should include engram count")
	}
	if !strings.Contains(guide, "Behavior mode: autonomous") {
		t.Error("guide should show behavior mode")
	}
}

func TestGenerateGuide_PromptedMode(t *testing.T) {
	mode := "prompted"
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{BehaviorMode: &mode})
	guide := generateGuide("test-vault", r, engineStats{})

	if !strings.Contains(guide, "Only store memories when the user explicitly asks") {
		t.Error("prompted mode should describe user-initiated storage")
	}
	if !strings.Contains(guide, "Behavior mode: prompted") {
		t.Error("guide should show prompted behavior mode")
	}
}

func TestGenerateGuide_SelectiveMode(t *testing.T) {
	mode := "selective"
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{BehaviorMode: &mode})
	guide := generateGuide("work", r, engineStats{})

	if !strings.Contains(guide, "Automatically remember decisions, errors") {
		t.Error("selective mode should mention auto-remembering decisions and errors")
	}
}

func TestGenerateGuide_CustomMode(t *testing.T) {
	mode := "custom"
	instr := "Always remember code snippets and API patterns."
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{
		BehaviorMode:         &mode,
		BehaviorInstructions: &instr,
	})
	guide := generateGuide("dev", r, engineStats{})

	if !strings.Contains(guide, "Always remember code snippets and API patterns.") {
		t.Error("custom mode should include verbatim instructions")
	}
}

func TestGenerateGuide_CustomModeNoInstructions(t *testing.T) {
	mode := "custom"
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{BehaviorMode: &mode})
	guide := generateGuide("dev", r, engineStats{})

	if !strings.Contains(guide, "no instructions were provided") {
		t.Error("custom mode with no instructions should mention fallback")
	}
}

func TestGenerateGuide_VaultConfigSection(t *testing.T) {
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{Preset: "knowledge-graph"})
	guide := generateGuide("kg", r, engineStats{EngramCount: 100})

	if !strings.Contains(guide, "Hebbian learning: enabled") {
		t.Error("knowledge-graph should show Hebbian enabled")
	}
	if !strings.Contains(guide, "Graph hop depth: 4") {
		t.Error("knowledge-graph should show hop depth 4")
	}
	if !strings.Contains(guide, "Predictive activation (PAS): enabled") {
		t.Error("knowledge-graph should show PAS enabled")
	}
}

func TestGenerateGuide_ScratchpadConfig(t *testing.T) {
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{Preset: "scratchpad"})
	guide := generateGuide("scratch", r, engineStats{})

	if !strings.Contains(guide, "Hebbian learning: disabled") {
		t.Error("scratchpad should show Hebbian disabled")
	}
	if !strings.Contains(guide, "Predictive activation (PAS): disabled") {
		t.Error("scratchpad should show PAS disabled")
	}
	if !strings.Contains(guide, "Behavior mode: selective") {
		t.Error("scratchpad preset should default to selective behavior mode")
	}
}

func TestGenerateGuide_WorkingConfig(t *testing.T) {
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{Preset: "working"})
	guide := generateGuide("wf", r, engineStats{})

	if !strings.Contains(guide, "Hebbian learning: enabled") {
		t.Error("working should show Hebbian enabled")
	}
	if !strings.Contains(guide, "Predictive activation (PAS): enabled") {
		t.Error("working should show PAS enabled")
	}
	if !strings.Contains(guide, "Behavior mode: selective") {
		t.Error("working preset should show selective behavior mode")
	}
	if !strings.Contains(guide, "Retention: 7 days") {
		t.Error("working should surface its 7-day retention")
	}
}

func TestGenerateGuide_TipsSection(t *testing.T) {
	r := auth.ResolvePlasticity(nil)
	guide := generateGuide("default", r, engineStats{})

	if !strings.Contains(guide, "mode='deep'") {
		t.Error("tips should mention deep mode")
	}
	if !strings.Contains(guide, "muninn_link") {
		t.Error("tips should mention muninn_link")
	}
}

func TestHandleGuideHappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_guide","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("expected non-nil result")
	}

	wrapper, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result to be an object, got %T", resp.Result)
	}
	contents, ok := wrapper["content"].([]any)
	if !ok || len(contents) == 0 {
		t.Fatal("expected result.content to be a non-empty array")
	}
	item, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatal("expected result.content[0] to be an object")
	}
	text, ok := item["text"].(string)
	if !ok || text == "" {
		t.Fatal("expected result.content[0].text to be a non-empty string")
	}
	if !strings.Contains(text, "MuninnDB Memory Guide") {
		t.Error("guide response should contain the header")
	}
	if !strings.Contains(text, "proactively remember") {
		t.Error("default vault should use autonomous mode")
	}
}

func TestHandleGuideNoVaultDefaultsOk(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_guide","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("guide should work with default vault, got error: %v", resp.Error)
	}
}

func TestGenerateGuide_EnrichmentGuidanceAutonomous(t *testing.T) {
	r := auth.ResolvePlasticity(nil) // default = autonomous + caller_preferred
	guide := generateGuide("default", r, engineStats{})

	if !strings.Contains(guide, "## Enrichment") {
		t.Error("autonomous + caller_preferred should include Enrichment section")
	}
	if !strings.Contains(guide, "include type, summary, and any entities") {
		t.Error("autonomous mode enrichment guidance missing")
	}
}

func TestGenerateGuide_EnrichmentGuidanceSelective(t *testing.T) {
	mode := "selective"
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{BehaviorMode: &mode})
	guide := generateGuide("test", r, engineStats{})

	if !strings.Contains(guide, "## Enrichment") {
		t.Error("selective + caller_preferred should include Enrichment section")
	}
	if !strings.Contains(guide, "Include type and summary when remembering decisions and errors") {
		t.Error("selective enrichment guidance missing")
	}
}

func TestGenerateGuide_EnrichmentGuidancePrompted(t *testing.T) {
	mode := "prompted"
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{BehaviorMode: &mode})
	guide := generateGuide("test", r, engineStats{})

	if strings.Contains(guide, "include type, summary") {
		t.Error("prompted mode should NOT include enrichment writing instructions")
	}
}

func TestGenerateGuide_EnrichmentGuidanceBackgroundOnly(t *testing.T) {
	ie := "background_only"
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{InlineEnrichment: &ie})
	guide := generateGuide("test", r, engineStats{})

	if strings.Contains(guide, "## Enrichment") {
		t.Error("background_only should NOT include Enrichment section")
	}
}

func TestGenerateGuide_EnrichmentGuidanceDisabled(t *testing.T) {
	ie := "disabled"
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{InlineEnrichment: &ie})
	guide := generateGuide("test", r, engineStats{})

	if strings.Contains(guide, "## Enrichment") {
		t.Error("disabled inline enrichment should NOT include Enrichment section")
	}
}

func TestGenerateGuide_InlineEnrichmentInConfig(t *testing.T) {
	r := auth.ResolvePlasticity(nil)
	guide := generateGuide("test", r, engineStats{})

	if !strings.Contains(guide, "Inline enrichment: caller_preferred") {
		t.Error("vault config should show inline enrichment setting")
	}
}

func TestGenerateGuide_MultiUserVault(t *testing.T) {
	on := true
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{MultiUser: &on})
	guide := generateGuide("team", r, engineStats{})

	if strings.Contains(guide, "purpose-built") {
		t.Error("multi-user guide must not recommend where_left_off for session start")
	}
	if !strings.Contains(guide, "per-user tag") {
		t.Error("multi-user guide should recommend per-user scoped recall")
	}
	if !strings.Contains(guide, "Shared vault (multi-user): yes") {
		t.Error("multi-user guide should surface the setting in Vault Configuration")
	}
	if !strings.Contains(guide, "Recent activity across ALL users") {
		t.Error("multi-user tool list should flag where_left_off as vault-global")
	}
	if strings.Contains(guide, "Resume a previous session") {
		t.Error("multi-user tool list must not present where_left_off as session resumption")
	}
	if !strings.Contains(guide, "Recent memory activity across ALL users") {
		t.Error("multi-user tool list should flag muninn_session as vault-global")
	}
}

func TestGenerateGuide_SingleUserKeepsWhereLeftOff(t *testing.T) {
	r := auth.ResolvePlasticity(nil)
	guide := generateGuide("default", r, engineStats{})
	if !strings.Contains(guide, "purpose-built for session resumption") {
		t.Error("single-user guide should keep the where_left_off session-start tip")
	}
	if !strings.Contains(guide, "Resume a previous session") {
		t.Error("single-user tool list should keep the plain where_left_off entry")
	}
}
