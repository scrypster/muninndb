package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
)

// COG-29 amendment — the vault-wide unresolved-contradiction debt readout.
//
// Every pre-existing contradiction notice on this surface is conditional on
// RETRIEVAL: the per-row annotation rides a returned row, and the response-level
// `conflict` block is pruned to pairs whose endpoints survived into the caller's
// results. A conflict the agent itself declared on a topic it stops querying is
// therefore never spoken about again. This block is the receipt.
//
// It is DELIBERATELY NOT fused with the COG-29 `conflict` block: `conflict`
// describes what you RECEIVED, `unresolved_contradictions` describes what the
// VAULT OWES. Merging them would make a vault-scoped fact look result-scoped and
// would break pruneConflictBlock's contract.
//
// It attaches to the three orientation surfaces the product itself prescribes —
// muninn_guide ("call on first connect"), muninn_where_left_off, and
// muninn_recall with mode="recent", which is the exact call the guide instructs
// shared-vault agents to make at session start. There is no MCP surface a client
// is OBLIGED to call at session start (initialize is the only guaranteed one and
// it is a vault-agnostic const), so this makes the debt visible on every
// orientation call the product recommends. It does not make it unmissable and
// does not claim to.

// contradictionDebtScopeNote mirrors the shared-vault language muninn_where_left_off
// already uses: in a multi-user vault these are conflicts across ALL users, so an
// agent that carefully scoped its recall to its own tag must not read them as its own.
const contradictionDebtScopeNote = "This vault is shared: these conflicts span ALL users of the vault and are not necessarily yours."

// contradictionDebtLowerBoundNote is the words behind `scan_complete: false`. It
// is the SAME sentence the prose render uses, so the two cannot drift into
// disagreeing about how much the count can be trusted.
const contradictionDebtLowerBoundNote = "The contradiction scan hit its cap, so this count is a LOWER BOUND: more unresolved pairs may exist than are reported here."

// contradictionDebtReporter is the optional vault-wide debt read. Probed rather
// than added to EngineInterface for the same reason contradictionReporter is:
// adding a method there forces every implementation, including test doubles in
// other packages, to change in lockstep. An engine that does not provide it
// simply carries no block.
type contradictionDebtReporter interface {
	ContradictionDebt(ctx context.Context, vault string) (*engine.ContradictionDebt, error)
}

// contradictionDebtFor is the single entry point the three orientation handlers
// use. It returns (nil, false) — meaning "attach nothing at all", not an empty
// object — whenever the vault carries no debt, the engine has no debt read, or
// the vault's plasticity has the readout switched off.
//
// The second return is the FAILED flag, and it exists because "we could not tell
// you" and "there is nothing to tell you" are different answers and this surface
// exists precisely to stop reporting one as the other. A derivation error must
// not turn muninn_recall into an error (principle #4, fail open on presentation),
// but silently emitting nothing restores the motivating incident with the
// confidence penalty already charged — a persistent store fault would make the
// vault look debt-free forever. The caller renders a minimal honest marker
// instead: no count, no pairs, no age.
//
// The WARN is deliberately not rate-limited, matching the gate's own probe
// warnings: a repeated failure on a low-frequency orientation call is signal.
func (s *MCPServer) contradictionDebtFor(ctx context.Context, vault string, p auth.ResolvedPlasticity) (*engine.ContradictionDebt, bool) {
	if !p.ContradictionDebt {
		return nil, false
	}
	rep, ok := s.engine.(contradictionDebtReporter)
	if !ok {
		return nil, false
	}
	debt, err := rep.ContradictionDebt(ctx, vault)
	if err != nil {
		slog.Warn("contradiction debt readout failed; reporting it as unavailable rather than as no debt", "vault", vault, "err", err)
		return nil, true
	}
	return debt, false
}

// contradictionDebtAttachment is what the two JSON orientation surfaces attach:
// the block, the unavailable marker, or nothing.
func (s *MCPServer) contradictionDebtAttachment(ctx context.Context, vault string, p auth.ResolvedPlasticity, now time.Time) map[string]any {
	debt, failed := s.contradictionDebtFor(ctx, vault, p)
	if failed {
		return contradictionDebtUnavailableBlock()
	}
	return contradictionDebtBlock(debt, p.MultiUser, now)
}

// contradictionDebtUnavailableBlock is the honest minimum: this vault's
// unresolved-contradiction state could not be read on this call. It carries NO
// count and NO pairs — an invented zero would be the silently-wrong class this
// whole readout exists to close.
func contradictionDebtUnavailableBlock() map[string]any {
	return map[string]any{
		"unavailable": true,
		"note":        "this vault's unresolved-contradiction state could not be read on this call; it is UNKNOWN, not zero. muninn_contradictions reads the same data directly.",
	}
}

// orientationPlasticity resolves the vault's plasticity for an orientation
// surface, falling back to resolved DEFAULTS when the config read fails —
// exactly what handleGuide already does. Principle #4: this is presentation, so
// an unreadable config must not silently disable a readout the operator never
// turned off.
func (s *MCPServer) orientationPlasticity(ctx context.Context, vault string) auth.ResolvedPlasticity {
	if p, err := s.engine.GetVaultPlasticity(ctx, vault); err == nil && p != nil {
		return *p
	}
	return auth.ResolvePlasticity(nil)
}

// contradictionDebtBlock renders the additive wire object. Returns nil when
// there is nothing to say, so the zero-debt case is ABSENT rather than
// {"count":0} — a standing empty object on every orientation call is exactly the
// wallpaper #609 died of.
//
// A zero DeclaredAt means UNKNOWN (a legacy edge written before the association
// carried a timestamp). It is rendered as ABSENT plus an explicit
// declared_at_unknown flag, never as an instant and never as 1970 — the same
// rule the contradictions surface already applies to DetectedAt/DeclaredAt.
func contradictionDebtBlock(debt *engine.ContradictionDebt, multiUser bool, now time.Time) map[string]any {
	if debt == nil || debt.Count == 0 {
		return nil
	}
	pairs := make([]map[string]any, 0, len(debt.Pairs))
	for _, p := range debt.Pairs {
		row := map[string]any{
			"id_a":      p.IDa,
			"concept_a": p.ConceptA,
			"id_b":      p.IDb,
			"concept_b": p.ConceptB,
		}
		if p.DeclaredAt.IsZero() {
			row["declared_at_unknown"] = true
		} else {
			row["declared_at"] = p.DeclaredAt.UTC().Format(time.RFC3339)
			row["age_hours"] = ageHours(p.DeclaredAt, now)
		}
		pairs = append(pairs, row)
	}
	block := map[string]any{
		"count":         debt.Count,
		"showing":       len(debt.Pairs),
		"truncated":     debt.Truncated,
		"scan_complete": debt.ScanComplete,
		"pairs":         pairs,
		"action":        engine.ContradictionDebtAction,
	}
	if !debt.Oldest.IsZero() {
		block["oldest_declared_at"] = debt.Oldest.UTC().Format(time.RFC3339)
		block["oldest_age_hours"] = ageHours(debt.Oldest, now)
	}
	if !debt.ScanComplete {
		// The prose render says "LOWER BOUND" in words; a bare `scan_complete:
		// false` next to a confident `count` does not, and a caller reading the
		// JSON is the one most likely to treat the count as exhaustive. Say the
		// same sentence on both renders.
		block["note"] = contradictionDebtLowerBoundNote
	}
	if multiUser {
		block["scope_note"] = contradictionDebtScopeNote
	}
	return block
}

// ageHours reports how long ago t was, in hours to one decimal. Never negative:
// a declaration timestamped in the future (clock skew across a cluster) reports
// 0 rather than a negative age that reads as a nonsense.
func ageHours(t, now time.Time) float64 {
	h := now.Sub(t).Hours()
	if h < 0 {
		return 0
	}
	return math.Round(h*10) / 10
}

// contradictionDebtGuideSection renders the SAME derivation as a markdown
// paragraph, because muninn_guide's response is prose, not JSON. Both renderers
// read one *engine.ContradictionDebt so the two surfaces cannot report different
// counts.
func contradictionDebtGuideSection(debt *engine.ContradictionDebt, multiUser bool, now time.Time) string {
	if debt == nil || debt.Count == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n**This vault currently has %d unresolved declared contradiction(s)", debt.Count)
	if !debt.Oldest.IsZero() {
		fmt.Fprintf(&b, ", the oldest declared %.1f hours ago", ageHours(debt.Oldest, now))
	}
	b.WriteString(".** ")
	b.WriteString(engine.ContradictionDebtAction)
	if multiUser {
		b.WriteString(" ")
		b.WriteString(contradictionDebtScopeNote)
	}
	if !debt.ScanComplete {
		b.WriteString(" ")
		b.WriteString(contradictionDebtLowerBoundNote)
	}
	b.WriteString("\n")
	for _, p := range debt.Pairs {
		fmt.Fprintf(&b, "  - %q (%s) vs %q (%s)", p.ConceptA, p.IDa, p.ConceptB, p.IDb)
		if p.DeclaredAt.IsZero() {
			b.WriteString(" — declared at an unknown time")
		} else {
			fmt.Fprintf(&b, " — declared %.1f hours ago", ageHours(p.DeclaredAt, now))
		}
		b.WriteString("\n")
	}
	if debt.Truncated {
		fmt.Fprintf(&b, "  (showing the %d oldest of %d; muninn_contradictions lists them all)\n", len(debt.Pairs), debt.Count)
	}
	return b.String()
}
