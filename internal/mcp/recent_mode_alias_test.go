package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// mode="recent" is an alias of balanced — engine defaults, ignoring the
// vault's default recall mode like every wire mode (recent-mode orientation
// design v2, D1). These tests pin that no MCP surface tells an agent it is
// anything else — and that the guide's shared-vault session-start call keeps
// the exact mode="recent" string, which is the COG-29 contradiction-debt
// trigger (handlers.go: `if mode == "recent"`).

// abstainedMultiUserEngine abstains AND reports a shared vault.
type abstainedMultiUserEngine struct{ abstainedFakeEngine }

func (e *abstainedMultiUserEngine) GetVaultPlasticity(_ context.Context, _ string) (*auth.ResolvedPlasticity, error) {
	r := auth.ResolvePlasticity(nil)
	r.MultiUser = true
	return &r, nil
}

func recallHint(t *testing.T, eng EngineInterface, args string) string {
	t.Helper()
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":` + args + `}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	hint, _ := extractInnerJSON(t, resp)["hint"].(string)
	return hint
}

// TestHandleRecall_RecentAbstainHintNotCircular: the empty-result hint used to
// tell a caller who had ALREADY sent mode='recent' to "try mode='recent'".
// Since recent scores exactly like balanced, it cannot rescue an empty
// result — the hint must say so and route to the tools that can.
func TestHandleRecall_RecentAbstainHintNotCircular(t *testing.T) {
	abstained := &mbp.ActivateResponse{Abstained: true, AbstainedReason: "below_threshold"}
	single := &abstainedFakeEngine{resp: abstained}
	shared := &abstainedMultiUserEngine{abstainedFakeEngine{resp: abstained}}

	for _, tc := range []struct {
		name  string
		eng   EngineInterface
		args  string
		want  []string
		avoid []string
	}{
		{"single_recent", single, `{"vault":"default","context":["anything"],"mode":"recent"}`,
			[]string{"threshold", "scores exactly like mode='balanced'", "muninn_where_left_off"},
			[]string{"try mode='recent'"}},
		{"shared_recent", shared, `{"vault":"default","context":["anything"],"mode":"recent"}`,
			[]string{"threshold", "scores exactly like mode='balanced'", "tags_any"},
			[]string{"try mode='recent'", "or use muninn_where_left_off"}},
		// No mode: the hint must not advertise recent as a continuity fix either.
		{"single_default", single, `{"vault":"default","context":["anything"]}`,
			[]string{"threshold", "muninn_where_left_off"},
			[]string{"try mode='recent'"}},
		{"shared_default", shared, `{"vault":"default","context":["anything"]}`,
			[]string{"threshold", "tags_any"},
			[]string{"try mode='recent'", "or use muninn_where_left_off"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hint := recallHint(t, tc.eng, tc.args)
			for _, w := range tc.want {
				if !strings.Contains(hint, w) {
					t.Errorf("hint must contain %q; got %q", w, hint)
				}
			}
			for _, a := range tc.avoid {
				if strings.Contains(hint, a) {
					t.Errorf("hint must not contain %q; got %q", a, hint)
				}
			}
		})
	}
}

// sessionStartStep returns the guide line that begins "1. " inside the
// session-start pattern.
func sessionStartStep(t *testing.T, guide string) string {
	t.Helper()
	i := strings.Index(guide, "**Session start pattern:**")
	if i < 0 {
		t.Fatal("guide has no session start pattern")
	}
	for _, line := range strings.Split(guide[i:], "\n") {
		if strings.HasPrefix(line, "1. ") {
			return line
		}
	}
	t.Fatal("session start pattern has no step 1")
	return ""
}

// TestGuide_SharedSessionStartUsesTagFilter: the shared-vault session-start
// call used to put the per-user tag in `context` with no `tags_any`. A tag in
// context does not FILTER — it only adds lexical evidence — so other users'
// rows came back. The call must scope with tags_any, keep mode="recent" (the
// COG-29 trigger), say that a tag in context does not filter, and point the
// agent at relevance_band. It must make NO ordering claim: newest-first among
// tied rows is Increment 2 and is false on develop.
func TestGuide_SharedSessionStartUsesTagFilter(t *testing.T) {
	on := true
	guide := generateGuide("team", auth.ResolvePlasticity(&auth.PlasticityConfig{MultiUser: &on}), engineStats{}, "")
	step := sessionStartStep(t, guide)

	if !strings.Contains(step, `tags_any=["<your per-user tag>"]`) {
		t.Errorf("shared session-start call must scope with tags_any; got %q", step)
	}
	if !strings.Contains(step, `mode="recent"`) {
		t.Errorf("shared session-start call must keep mode=\"recent\" — it is the COG-29 contradiction-debt trigger; got %q", step)
	}
	if strings.Contains(step, `context=["<your per-user tag>", "session start"]`) {
		t.Errorf("shared session-start call must not scope by putting the tag in context; got %q", step)
	}
	if !strings.Contains(step, "does not filter") {
		t.Errorf("step must say a tag in context does not filter; got %q", step)
	}
	if !strings.Contains(step, "relevance_band") {
		t.Errorf("step must point the agent at relevance_band; got %q", step)
	}
}

// TestGuide_SingleUserSessionStartIsWhereLeftOff: in a single-user vault the
// topic-free orientation call is muninn_where_left_off, not a content-free
// recall (which abstains: an orientation phrase matches nothing).
func TestGuide_SingleUserSessionStartIsWhereLeftOff(t *testing.T) {
	guide := generateGuide("default", auth.ResolvePlasticity(nil), engineStats{}, "")
	step := sessionStartStep(t, guide)
	if !strings.Contains(step, "muninn_where_left_off") {
		t.Errorf("single-user session-start step 1 must be muninn_where_left_off; got %q", step)
	}
	if strings.Contains(step, `context=["session start"]`) {
		t.Errorf("single-user step 1 must not be a content-free recall; got %q", step)
	}
}

// TestGuide_MakesNoRecencyClaimForRecentMode: no guide line that mentions
// mode recent may call it recency-biased or claim any newest-first ordering
// (that claim waits for Increment 2's tie-break), and neither guide may
// recommend it as the way to get continuity without context.
func TestGuide_MakesNoRecencyClaimForRecentMode(t *testing.T) {
	on := true
	for name, r := range map[string]auth.ResolvedPlasticity{
		"single": auth.ResolvePlasticity(nil),
		"shared": auth.ResolvePlasticity(&auth.PlasticityConfig{MultiUser: &on}),
	} {
		guide := strings.ToLower(generateGuide("v", r, engineStats{}, ""))
		for _, banned := range []string{"use mode='recent' at session start", "mode='recent' when you need continuity"} {
			if strings.Contains(guide, banned) {
				t.Errorf("%s guide must not contain %q", name, banned)
			}
		}
		for _, line := range strings.Split(guide, "\n") {
			if !strings.Contains(line, "mode='recent'") && !strings.Contains(line, `mode="recent"`) {
				continue
			}
			for _, banned := range []string{"newest", "newer", "recency-biased", "most recent first"} {
				if strings.Contains(line, banned) {
					t.Errorf("%s guide line about mode recent must not claim %q: %q", name, banned, line)
				}
			}
		}
	}
}

// TestHandleRecall_TagFilteredAbstainHintNotCircular: a caller who ALREADY
// scoped with a tag filter (a new user's first session on a shared vault:
// context=["user-a"], tags_any=["user-a"], mode="recent") used to be told to
// "recall with tags_any" — the call they had just made. An explicit tag
// filter defines the candidate set (COG-5 S1) and floors its rows at the
// default bar, so the honest reading is that nothing carrying that tag has
// been stored yet.
func TestHandleRecall_TagFilteredAbstainHintNotCircular(t *testing.T) {
	abstained := &mbp.ActivateResponse{Abstained: true, AbstainedReason: "below_threshold"}
	single := &abstainedFakeEngine{resp: abstained}
	shared := &abstainedMultiUserEngine{abstainedFakeEngine{resp: abstained}}
	filters := map[string]string{
		"tags_any":   `"tags_any":["user-a"]`,
		"tags_all":   `"tags_all":["user-a"]`,
		"tag_filter": `"tag_filter":{"prefix":"user:","eq":"a"}`,
	}
	for fname, f := range filters {
		for vname, eng := range map[string]EngineInterface{"single": single, "shared": shared} {
			t.Run(vname+"_"+fname, func(t *testing.T) {
				hint := recallHint(t, eng, `{"vault":"default","context":["user-a"],"mode":"recent",`+f+`}`)
				for _, avoid := range []string{"recall with tags_any", "tags_any=[", "try mode='recent'"} {
					if strings.Contains(hint, avoid) {
						t.Errorf("caller already used %s; hint must not recommend %q; got %q", fname, avoid, hint)
					}
				}
				if !strings.Contains(hint, "nothing carrying that tag") {
					t.Errorf("hint must say plainly that nothing tagged for the caller exists yet; got %q", hint)
				}
			})
		}
	}
}
