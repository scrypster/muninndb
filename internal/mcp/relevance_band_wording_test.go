package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestRelevanceBandWording_NoAnswerClaim pins COG-30 (d): a relevance band
// reports how much EVIDENCE a memory has against the query, measured against
// the embedding model's noise floor with fixed edges. It does not report
// whether the memory answers the question — nothing in MuninnDB computes
// that. Blind-graded recalls on production vaults showed `moderate` rows are
// mostly not directly useful, and a fluent question a vault holds nothing
// about still mostly returns `moderate` rows (decision record, "Honest
// relevance labels").
//
// Three checks, over every surface COG-30 (d) names:
//  1. banned phrases — the specific old over-claims;
//  2. banned PATTERNS — "band X is / means / = relevant | an answer | the
//     answer" in any wording, and "the vault's own / measured noise floor"
//     (the floor is the model's; nothing is measured per vault);
//  3. required governing claim — every surface that DEFINES the bands says
//     the band is evidence, not whether the row answers.
//
// Cloned from TestRecentModeWording_NoVaultDefaultEquivalence.
// bandAnswerClaim matches "band X is / means / = relevant | an answer | the
// answer" in any wording: "a `moderate` row is a relevant answer", "strong
// means the answer", "moderate = relevant", "weak results are answers".
var bandAnswerClaim = regexp.MustCompile(`(?:strong|moderate|weak|filter_match|uncalibrated)[` + "`" + `'"]?` +
	`(?:\s+(?:band|row|rows|result|results|match|matches|memory|memories))?` +
	`\s*(?:is|are|means|mean|indicates|=|:)\s+(?:a\s+|an\s+|the\s+)?` +
	`(?:(?:highly|moderately|very|probably|likely)\s+)?(?:relevant|answer|answers|correct)\b`)

func TestRelevanceBandWording_NoAnswerClaim(t *testing.T) {
	banned := []string{
		"returns the most relevant memories",     // old muninn_recall opener
		"activates the most relevant memories",   // old LangChain adapter docstring
		"genuinely relevant",                     // old LangChain adapter docstring
		"`moderate` (real evidence)",             // old guide gloss
		"moderate = clear of the noise band",     // old openapi gloss
		"clear of the noise band",                // old relevance.go gloss: implies "not noise, so relevant"
		"nothing in this vault matched strongly", // old weak_only hint: implies a returned set matched
		"matched nothing strongly",               // old tool/guide abstention sentence, same implication
		"judge relevance by relevance_band",      // old openapi: the band as proof of relevance
		"(near-verbatim evidence)",               // old strong gloss: overstates what strong needs
		"sharing vocabulary",                     // false: cross-vault moderate rows score almost all semantic
	}
	patterns := []*regexp.Regexp{
		bandAnswerClaim,
		// "moderately relevant" in any context is the misreading itself.
		regexp.MustCompile(`\bmoderately relevant\b`),
		// The floor is the embedding model's (operator-overridable); nothing is
		// measured per vault.
		regexp.MustCompile(`vault'?s\s+(?:own\s+)?(?:measured\s+)?noise\s+floor`),
		regexp.MustCompile(`vault'?s\s+own\s+(?:measured\s+)?(?:noise|calibration)`),
	}
	governing := regexp.MustCompile(`not (?:report )?whether (?:the|a) (?:memory|row) answers|not answerability`)

	// Lower-case, collapse whitespace, and drop Go comment markers so a claim
	// wrapped across comment lines still reads as one sentence.
	norm := func(s string) string {
		var out []string
		for _, f := range strings.Fields(strings.ToLower(s)) {
			if f != "//" {
				out = append(out, f)
			}
		}
		return strings.Join(out, " ")
	}
	texts := map[string]string{}
	definesBands := map[string]bool{}

	for _, tool := range allToolDefinitions() {
		if tool.Name == "muninn_recall" {
			texts["tool muninn_recall"] = norm(mustJSON(tool))
			definesBands["tool muninn_recall"] = true
		}
	}
	if texts["tool muninn_recall"] == "" {
		t.Fatal("muninn_recall tool definition not found")
	}
	on := true
	texts["guide single"] = norm(generateGuide("v", auth.ResolvePlasticity(nil), engineStats{}, ""))
	texts["guide shared"] = norm(generateGuide("v", auth.ResolvePlasticity(&auth.PlasticityConfig{MultiUser: &on}), engineStats{}, ""))
	definesBands["guide single"], definesBands["guide shared"] = true, true
	texts["weak_only hint"] = norm(recallHint(t, &abstainedFakeEngine{resp: &mbp.ActivateResponse{Abstained: true, AbstainedReason: "weak_only", WeakAvailable: 3}},
		`{"vault":"default","context":["anything"]}`))
	if !strings.Contains(texts["weak_only hint"], "include_weak: true") {
		t.Fatalf("weak_only hint not captured (got %q)", texts["weak_only hint"])
	}

	for rel, defines := range map[string]bool{
		"internal/transport/rest/openapi.yaml":    true,
		"docs/internals/invariants.md":            true,
		"internal/engine/activation/relevance.go": true,
		"internal/transport/mbp/types.go":         true,
		"internal/mcp/types.go":                   true,
		"sdk/python/muninn/langchain.py":          false, // describes recall, not the bands
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		texts[rel] = norm(string(b))
		definesBands[rel] = defines
	}

	for name, text := range texts {
		for _, b := range banned {
			if strings.Contains(text, b) {
				t.Errorf("%s claims %q — a band reports evidence against the model's noise floor, not that a row answers the query", name, b)
			}
		}
		for _, re := range patterns {
			if m := re.FindString(text); m != "" {
				t.Errorf("%s matches banned claim pattern: %q", name, m)
			}
		}
		if definesBands[name] && !governing.MatchString(text) {
			t.Errorf("%s defines the bands but never says the band is evidence, not whether the row answers", name)
		}
	}

	// The moderate claim's key phrase wherever a reader is told how to use it.
	const moderateKey = "a candidate to verify, not an established answer"
	for _, name := range []string{"tool muninn_recall", "guide single", "guide shared", "internal/transport/rest/openapi.yaml"} {
		if !strings.Contains(texts[name], moderateKey) {
			t.Errorf("%s must describe moderate as %q", name, moderateKey)
		}
	}
}

// TestRelevanceBandWording_PatternCatchesParaphrase: the pattern check must
// catch an over-claim the exact-phrase list has never seen, and must not fire
// on the honest negations the shipped text uses.
func TestRelevanceBandWording_PatternCatchesParaphrase(t *testing.T) {
	re := bandAnswerClaim
	for _, bad := range []string{
		"a `moderate` row is a relevant answer",
		"strong means the answer",
		"moderate = relevant",
		"weak results are answers",
		"`moderate` is highly relevant",
	} {
		if !re.MatchString(bad) {
			t.Errorf("pattern misses over-claim %q", bad)
		}
	}
	for _, ok := range []string{
		"`moderate` = a candidate to verify, not an established answer",
		"`moderate` is a candidate to verify, not an answer",
		"`strong` = a close match, most likely to answer",
		"`weak` = near the noise floor",
	} {
		if re.MatchString(ok) {
			t.Errorf("pattern fires on honest text %q", ok)
		}
	}
}
