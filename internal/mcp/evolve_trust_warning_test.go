package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// Issue #872 — a content-replacing verb must DECLARE what standing it carried.
//
// Evolve now inherits trust and confidence instead of resetting them to
// inferred/1.0. Inheriting is the right default (§2 of the design), but it is
// exactly the #769 dilemma: "verified" means a human confirmed THIS content, and
// an evolve replaces the content. #769's residual was never the inheritance, it
// was that "nothing SAID the label had been inherited". The announcement is what
// pays the honesty debt here too.
//
// Deliberately narrow: only `verified` and `untrusted` warn. `inferred` and
// `external` are silent — a warning on every evolve is a warning on none.
//
// Fixture domain: a fictional alpine trail-maintenance cooperative.
// ---------------------------------------------------------------------------

// writeCtx carries a write credential — trust=verified is SEC-14 gated.
func writeCtx() context.Context {
	return context.WithValue(context.Background(), auth.ContextMode, auth.ModeWrite)
}

func TestEvolveAdapter_AnnouncesCarriedVerifiedTrust(t *testing.T) {
	eng, adapter, cleanup := newRealEngineAdapter(t)
	defer cleanup()
	ctx := writeCtx()

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "trailwork",
		Concept: "Bridge decking material standard",
		Content: "The Fernbrook crossing is decked in larch.",
		Trust:   "verified",
	})
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	res, err := adapter.Evolve(ctx, "trailwork", w.ID,
		"The Fernbrook crossing is decked in larch and re-decked every twelve years.",
		"added the re-deck cycle", nil, "Bridge decking material standard", nil, nil, timeZero())
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	joined := strings.Join(res.Warnings, " | ")
	if !strings.Contains(joined, "verified") {
		t.Fatalf("#872: evolve carried a VERIFIED stamp onto content that just changed and said "+
			"nothing — a reader now sees a human-confirmed label on text no human confirmed. warnings=%q",
			joined)
	}
	if !strings.Contains(joined, "muninn_trust") {
		t.Errorf("the carried-trust warning must name the tool that disclaims it (muninn_trust); got %q", joined)
	}
}

func TestEvolveAdapter_AnnouncesCarriedUntrustedTrust(t *testing.T) {
	eng, adapter, cleanup := newRealEngineAdapter(t)
	defer cleanup()
	ctx := context.Background()

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "trailwork",
		Concept: "Switchback drainage bar spacing",
		Content: "Drainage bars on the Larkspur switchbacks sit every 4 metres.",
		Trust:   "untrusted",
	})
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	res, err := adapter.Evolve(ctx, "trailwork", w.ID,
		"Drainage bars on the Larkspur switchbacks sit every 5 metres.",
		"resurveyed", nil, "Switchback drainage bar spacing", nil, nil, timeZero())
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	joined := strings.Join(res.Warnings, " | ")
	if !strings.Contains(joined, "untrusted") {
		t.Fatalf("#872: evolve carried the UNTRUSTED flag forward (correctly) but said nothing, so a "+
			"caller who edited the content to address the concern has no way to know the flag is still "+
			"on the record. warnings=%q", joined)
	}
	if !strings.Contains(joined, "muninn_trust") {
		t.Errorf("the carried-trust warning must name the tool that clears it (muninn_trust); got %q", joined)
	}
}

// The silent half: inferred and external are the ordinary states of an ordinary
// memory. Warning on them would fire on nearly every evolve in every vault and
// destroy the signal the two loud levels carry.
func TestEvolveAdapter_SilentOnOrdinaryTrustLevels(t *testing.T) {
	for _, level := range []string{"", "inferred", "external"} {
		t.Run("trust="+level, func(t *testing.T) {
			eng, adapter, cleanup := newRealEngineAdapter(t)
			defer cleanup()
			ctx := context.Background()

			w, err := eng.Write(ctx, &mbp.WriteRequest{
				Vault:   "trailwork",
				Concept: "Winter closure dates",
				Content: "The upper loop closes at the first hard frost.",
				Trust:   level,
			})
			if err != nil {
				t.Fatalf("seed write: %v", err)
			}
			res, err := adapter.Evolve(ctx, "trailwork", w.ID,
				"The upper loop closes at the first hard frost and reopens at the thaw.",
				"added the reopen", nil, "Winter closure dates", nil, nil, timeZero())
			if err != nil {
				t.Fatalf("Evolve: %v", err)
			}
			for _, warning := range res.Warnings {
				if strings.Contains(warning, "trust") {
					t.Errorf("trust=%q must not warn on evolve; got %q", level, warning)
				}
			}
		})
	}
}

// The wire check: the adapter's warning is worth nothing if the JSON-RPC
// response drops it. handleEvolve marshals the whole WriteResult rather than
// building a map by hand, but that is exactly the property worth pinning — the
// two known places an MCP field silently vanishes on this surface are the
// adapter and a hand-built response map, and a future refactor to the latter
// would take this warning with it and pass every adapter-level test above.
func TestEvolveOverMCP_CarriedTrustWarningReachesTheWire(t *testing.T) {
	eng, srv, _, cleanup := newDebtServer(t)
	defer cleanup()

	w, err := eng.Write(writeCtx(), &mbp.WriteRequest{
		Vault:   "default",
		Concept: "Bridge decking material standard",
		Content: "The Fernbrook crossing is decked in larch.",
		Trust:   "verified",
	})
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	out := callTool(t, srv, "muninn_evolve", fmt.Sprintf(
		`{"vault":"default","id":%q,"new_content":"The Fernbrook crossing is decked in larch and re-decked every twelve years.","reason":"added the re-deck cycle","concept":"Bridge decking material standard"}`,
		w.ID))

	warnings, _ := out["warnings"].([]any)
	var joined []string
	for _, x := range warnings {
		if s, ok := x.(string); ok {
			joined = append(joined, s)
		}
	}
	all := strings.Join(joined, " | ")
	if !strings.Contains(all, "verified") {
		t.Fatalf("the carried-trust warning did not reach the muninn_evolve JSON-RPC response; "+
			"response=%v", out)
	}
}
