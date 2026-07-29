package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// === THE PUSH acceptance harness (Gates 1-4) ================================
//
// A seeded vault (~200 memories, 12 armed intentions) and a scripted 60-call
// session: 15 should-fire, 10 cue-adjacent traps, 35 unrelated. Each scripted
// call models a separate agent session (fresh session-dedup map); one-shot
// consumption persists across calls via the durable fired marker — exactly
// the production semantics.
//
// Gates (non-gameable pair: silence fails G2, spam fails G1):
//
//	Gate1 precision  = fired∧wanted / fired            >= 0.90
//	Gate2 recall     = should-fire calls that fired    >= 0.80
//	Gate3 negative   = 35 unrelated calls -> EXACTLY ZERO notices
//	Gate4 RED        = mechanism off -> zero notices AND G1/G2 fail
//
// Gate5 (live shadow behind MUNINN_PROSPECTIVE=1 on a real vault) is a
// post-merge activity, deliberately NOT a test.

type harnessScenario struct {
	Intentions []struct {
		Content    string   `json:"content"`
		Cues       []string `json:"cues"`
		OneShot    bool     `json:"one_shot"`
		Importance float32  `json:"importance"`
	} `json:"intentions"`
	Memories []struct {
		Content  string   `json:"content"`
		Entities []string `json:"entities"`
	} `json:"memories"`
	Calls []struct {
		Label   string `json:"label"`
		Context string `json:"context"`
		Want    int    `json:"want"`
	} `json:"calls"`
}

type harnessResult struct {
	firedTotal     int // intention notices delivered across the session
	firedWanted    int // ...on a should_fire call, matching the expected intention
	shouldFireHit  int // should_fire calls that delivered their expected intention
	shouldFireCnt  int
	unrelatedCnt   int
	unrelatedFired int // ANY notice on an unrelated call (Gate3 counts all kinds)
}

func (r harnessResult) precision() float64 {
	if r.firedTotal == 0 {
		return 0
	}
	return float64(r.firedWanted) / float64(r.firedTotal)
}

func (r harnessResult) recall() float64 {
	if r.shouldFireCnt == 0 {
		return 0
	}
	return float64(r.shouldFireHit) / float64(r.shouldFireCnt)
}

// runProspectiveHarness seeds the vault and replays the scripted session.
// enabled=false models the MUNINN_PROSPECTIVE gate being off: recall runs
// identically but the notices path is never consulted (Gate4's RED arm).
func runProspectiveHarness(t *testing.T, enabled bool) harnessResult {
	t.Helper()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "push-harness-vault"

	raw, err := os.ReadFile("testdata/prospective_session.json")
	if err != nil {
		t.Fatalf("read scenario: %v", err)
	}
	var sc harnessScenario
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("parse scenario: %v", err)
	}

	// Seed: labeled memories + code-generated filler to ~200 total, so the
	// vault is big enough for the ubiquity floor to be live and for recall to
	// have real negatives to rank against.
	for _, m := range sc.Memories {
		ents := make([]mbp.InlineEntity, 0, len(m.Entities))
		for _, e := range m.Entities {
			ents = append(ents, mbp.InlineEntity{Name: e, Type: "concept"})
		}
		if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: m.Content, Entities: ents}); err != nil {
			t.Fatalf("seed memory: %v", err)
		}
	}
	for i := 0; i < 140; i++ {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Content: fmt.Sprintf("ledger item %d bookkeeping sequence %d", i, i*7),
		}); err != nil {
			t.Fatalf("seed filler: %v", err)
		}
	}

	// Arm the 12 intentions through the real Intend path (IDF floor live).
	intentionIDs := make([]string, len(sc.Intentions))
	for i, it := range sc.Intentions {
		imp := it.Importance
		id, err := eng.Intend(ctx, vault, it.Content, it.Cues, nil, it.OneShot, &imp)
		if err != nil {
			t.Fatalf("Intend[%d]: %v", i, err)
		}
		intentionIDs[i] = id
	}

	var res harnessResult
	for ci, call := range sc.Calls {
		resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
			Vault:      vault,
			Context:    []string{call.Context},
			MaxResults: 3,
			Threshold:  0.35,
		})
		if err != nil {
			t.Fatalf("call %d Activate: %v", ci, err)
		}

		dumpResults := func() string {
			s := ""
			for _, a := range resp.Activations {
				s += fmt.Sprintf(" [%.3f %q]", a.Score, a.Content)
			}
			return s
		}

		var notices []Notice
		if enabled {
			results := make([]ScoredResult, 0, len(resp.Activations))
			for _, a := range resp.Activations {
				results = append(results, ScoredResult{ID: a.ID, Score: float64(a.Score)})
			}
			// Fresh session per scripted call (separate agent sessions).
			seen, _ := newSession()
			notices, err = eng.NoticesForRecall(ctx, vault, results, seen, false)
			if err != nil {
				t.Fatalf("call %d NoticesForRecall: %v", ci, err)
			}
		}

		switch call.Label {
		case "should_fire":
			res.shouldFireCnt++
			hit := false
			for _, n := range notices {
				if n.Kind != "intention" {
					continue
				}
				res.firedTotal++
				if n.MemoryID == intentionIDs[call.Want] {
					res.firedWanted++
					hit = true
				} else {
					t.Logf("call %d (%q): spurious intention %s fired (cue=%s) results:%s", ci, call.Context, n.MemoryID, n.Cue, dumpResults())
				}
			}
			if hit {
				res.shouldFireHit++
			} else if enabled {
				t.Logf("call %d (%q): expected intention %d did not fire (results=%d)", ci, call.Context, call.Want, len(resp.Activations))
			}
		case "trap":
			for _, n := range notices {
				if n.Kind == "intention" {
					res.firedTotal++
					t.Logf("call %d TRAP (%q): intention %s fired (cue=%s) — precision leak; results:%s", ci, call.Context, n.MemoryID, n.Cue, dumpResults())
				}
			}
		case "unrelated":
			res.unrelatedCnt++
			if len(notices) > 0 {
				res.unrelatedFired += len(notices)
				for _, n := range notices {
					if n.Kind == "intention" {
						res.firedTotal++
					}
				}
				t.Logf("call %d UNRELATED (%q): %d notices — Gate3 violation; results:%s", ci, call.Context, len(notices), dumpResults())
			}
		default:
			t.Fatalf("call %d: unknown label %q", ci, call.Label)
		}
	}
	return res
}

// TestProspectiveAcceptance_Gates1to3 is the non-gameable acceptance gate.
func TestProspectiveAcceptance_Gates1to3(t *testing.T) {
	res := runProspectiveHarness(t, true)
	t.Logf("harness: fired=%d fired∧wanted=%d precision=%.3f recall=%d/%d=%.3f unrelated_notices=%d",
		res.firedTotal, res.firedWanted, res.precision(), res.shouldFireHit, res.shouldFireCnt, res.recall(), res.unrelatedFired)

	if res.firedTotal == 0 || res.precision() < 0.90 {
		t.Errorf("Gate1 FAIL: precision = %.3f (fired=%d, wanted=%d), want >= 0.90", res.precision(), res.firedTotal, res.firedWanted)
	}
	if res.recall() < 0.80 {
		t.Errorf("Gate2 FAIL: recall = %.3f (%d/%d), want >= 0.80", res.recall(), res.shouldFireHit, res.shouldFireCnt)
	}
	if res.unrelatedCnt != 35 {
		t.Fatalf("scenario drift: %d unrelated calls, want 35", res.unrelatedCnt)
	}
	if res.unrelatedFired != 0 {
		t.Errorf("Gate3 FAIL: %d notices on the 35 unrelated calls, want EXACTLY ZERO", res.unrelatedFired)
	}
}

// TestProspectiveAcceptance_Gate4RED proves the harness is not trivially
// green: with the mechanism off the session produces zero notices, and both
// Gate1 and Gate2 fail. A harness that passes with the feature disabled would
// be measuring nothing.
func TestProspectiveAcceptance_Gate4RED(t *testing.T) {
	res := runProspectiveHarness(t, false)
	t.Logf("harness (mechanism OFF): fired=%d precision=%.3f recall=%.3f", res.firedTotal, res.precision(), res.recall())
	if res.firedTotal != 0 || res.unrelatedFired != 0 {
		t.Errorf("Gate4 FAIL: mechanism off still produced notices (fired=%d unrelated=%d)", res.firedTotal, res.unrelatedFired)
	}
	g1ok := res.firedTotal > 0 && res.precision() >= 0.90
	g2ok := res.recall() >= 0.80
	if g1ok || g2ok {
		t.Errorf("Gate4 FAIL: gates still pass with the mechanism off (g1=%v g2=%v) — the harness would be gameable", g1ok, g2ok)
	}
}
