package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/writetwin"
)

// writeTwinCapable is the optional engine surface for the write-time twin
// check (COG-35). The adapter forwards it to *engine.Engine; test fakes need
// not implement it (the block is then simply absent).
type writeTwinCapable interface {
	WriteTwinInline(ctx context.Context, vault, newID string, render func(*engine.WriteTwinResult) int) *engine.WriteTwinResult
}

// RelatedBlock is the mode-1 write-time twin block on a muninn_remember
// response. It is EVIDENCE, not an assertion: which earlier memories are very
// close, how close against the threshold in use, and what each possible
// action would retire. It never decides for the agent.
type RelatedBlock struct {
	// Unavailable/Reason: the check could not run. Loud, never silent — an
	// agent that sees no block must be able to trust that none was found.
	Unavailable bool   `json:"unavailable,omitempty"`
	Reason      string `json:"reason,omitempty"`

	Basis      *RelatedBasis      `json:"basis,omitempty"`
	Candidates []RelatedCandidate `json:"candidates,omitempty"`
	Options    []RelatedOption    `json:"options,omitempty"`
}

// RelatedBasis states the rule that produced the candidates.
type RelatedBasis struct {
	Rule            string  `json:"rule"`
	Threshold       float64 `json:"threshold"`
	ThresholdSource string  `json:"threshold_source"`
	Window          string  `json:"window"`
	Considered      int     `json:"considered"`
	Note            string  `json:"note"`
}

// RelatedCandidate is one live chain head the new memory is very close to.
type RelatedCandidate struct {
	ID         string  `json:"id"`
	Concept    string  `json:"concept"`
	Similarity float64 `json:"similarity"`
	// VsThreshold is similarity − threshold, signed ("+0.021").
	VsThreshold string `json:"vs_threshold"`
	Written     string `json:"written"`
}

// RelatedOption is one possible action and exactly which id it would retire
// (null: nothing is retired).
type RelatedOption struct {
	If      string  `json:"if"`
	Call    string  `json:"call"`
	Retires *string `json:"retires"`
}

const relatedNote = "Not a confirmed duplicate. Similarity covers each memory's opening text. Read both before acting."

// credentialMayReadTwins reports whether the caller may see the block: full
// mode only. A write-mode key cannot call read tools, so a concept in the
// block would be a read oracle; append can neither link nor forget, so the
// block offers it nothing it could do; observe never writes. Static-token
// and open-server callers are full by construction; mk_ and cap_ follow their
// mode (a full-mode cap_ sees it).
func credentialMayReadTwins(a AuthContext) bool {
	if !a.IsAPIKey && !a.IsCapability {
		return true
	}
	return a.Mode == auth.ModeFull
}

func twinCallerFor(a AuthContext, hasOpID bool) engine.WriteTwinCaller {
	c := engine.WriteTwinCaller{Mode: a.Mode, HasOpID: hasOpID}
	switch {
	case a.IsAPIKey:
		c.CredentialKind = "mk"
	case a.IsCapability:
		c.CredentialKind = "cap"
	case a.Token != "":
		c.CredentialKind = "static"
		c.Mode = auth.ModeFull
	default:
		c.CredentialKind = "open"
		c.Mode = auth.ModeFull
	}
	return c
}

// rememberRelated computes the related block for a just-committed single
// remember. nil = nothing to add (the common case, and always when the check
// is off or in shadow). It cannot fail or delay the write beyond the engine's
// bounded wait, and a panic anywhere in it yields the plain write response.
func (s *MCPServer) rememberRelated(ctx context.Context, vault, newID, engineHint string, upsert bool) (block *RelatedBlock) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mcp: write twin block panicked (the committed write is unaffected)", "panic", fmt.Sprint(r))
			block = nil
		}
	}()
	// The returned id IS the existing memory for these: there is no new
	// memory to compare.
	if upsert || engineHint == "duplicate_content" || engineHint == "idempotent" || strings.HasPrefix(engineHint, "upsert") {
		return nil
	}
	if !credentialMayReadTwins(authFromContext(ctx)) {
		return nil
	}
	wt, ok := s.engine.(writeTwinCapable)
	if !ok {
		return nil
	}
	var built *RelatedBlock
	res := wt.WriteTwinInline(ctx, vault, newID, func(r *engine.WriteTwinResult) int {
		built = buildRelatedBlock(newID, r, time.Now())
		return len(built.Candidates)
	})
	if res == nil {
		return nil
	}
	if res.Unavailable != "" {
		return &RelatedBlock{Unavailable: true, Reason: res.Unavailable}
	}
	if len(res.Candidates) == 0 {
		return nil
	}
	if built == nil { // an engine that did not call render
		built = buildRelatedBlock(newID, res, time.Now())
	}
	return built
}

// buildRelatedBlock renders the evidence and options, then enforces the
// MaxBlockBytes cap by shrinking (shorter concepts, then one candidate) —
// never by dropping the options or the basis.
func buildRelatedBlock(newID string, res *engine.WriteTwinResult, now time.Time) *RelatedBlock {
	cands := res.Candidates
	if len(cands) > writetwin.MaxShown {
		cands = cands[:writetwin.MaxShown]
	}
	// Shrink order: keep every candidate and shorten the concept echoes (the
	// id is the evidence; the concept is a convenience), and only then drop
	// the second candidate.
	for n := len(cands); n >= 1; n-- {
		for _, conceptRunes := range []int{writetwin.MaxConceptRunes, 40, 16} {
			b := renderRelated(newID, res, cands[:n], conceptRunes, now)
			if size, err := json.Marshal(b); err == nil && len(size) <= writetwin.MaxBlockBytes {
				return b
			}
		}
	}
	return renderRelated(newID, res, cands[:1], 8, now)
}

func renderRelated(newID string, res *engine.WriteTwinResult, cands []engine.WriteTwinCandidate, conceptRunes int, now time.Time) *RelatedBlock {
	b := &RelatedBlock{Basis: &RelatedBasis{
		Rule: "recent_twin", Threshold: res.Threshold, ThresholdSource: res.ThresholdSource,
		Window: "1h", Considered: res.Considered, Note: relatedNote,
	}}
	for _, c := range cands {
		b.Candidates = append(b.Candidates, RelatedCandidate{
			ID: c.ID, Concept: clipConcept(c.Concept, conceptRunes),
			Similarity:  math.Round(c.Similarity*1000) / 1000,
			VsThreshold: fmt.Sprintf("%+.3f", c.Similarity-res.Threshold),
			Written:     ageString(now.Sub(c.CreatedAt)),
		})
	}
	// The UPDATE direction first: most judged positives were updates. Each
	// option names exactly the id it retires. "old supersedes new" is never
	// offered — it would retire the update — and forgetting the new copy is
	// never the first option.
	// The ids live in call and retires; the condition names the candidate by
	// position so the id is not paid for three times per option.
	for i, c := range cands {
		id := c.ID
		b.Options = append(b.Options, RelatedOption{
			If:      fmt.Sprintf("the new memory updates or corrects candidate %d", i+1),
			Call:    fmt.Sprintf("muninn_link(source_id=%q,target_id=%q,relation=\"supersedes\")", newID, id),
			Retires: &id,
		})
	}
	nid := newID
	which := "candidate 1"
	if len(cands) > 1 {
		which = "a candidate"
	}
	b.Options = append(b.Options,
		RelatedOption{If: which + " already says this and is the better copy", Call: fmt.Sprintf("muninn_forget(id=%q)", newID), Retires: &nid},
		RelatedOption{If: "they are different facts", Call: "none", Retires: nil},
	)
	return b
}

func clipConcept(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func ageString(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}
