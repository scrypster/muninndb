package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// THE PUSH increment 1 — prospective memory (armed intentions + notices).
//
// McDaniel & Einstein: intention retrieval is either costly MONITORING
// (polling) or cheap SPONTANEOUS retrieval, and spontaneous retrieval works
// only when the cue is FOCAL to what is being processed. MuninnDB therefore
// NEVER monitors: an intention is armed on entity cues (0x2D index) and
// checked ONLY inside a tool call the agent itself made, against the entities
// of that call's own results. Time (valid_until) SILENCES an intention; it
// never fires one — there is no scheduler, no sweep, no background delivery
// (the #609 lesson: task-blind push gets zero uptake).
//
// Deferred beyond this increment (by design, see the design doc): SSE
// notifications/muninn/* wiring; recurring cooldowns; derived intentions from
// muninn_decide; working-buffer focal cues; semantic (non-entity) cues;
// notices on remember_batch/where_left_off/find_by_entity; contradiction
// auto-clear; gRPC/MBP intention parity.

const (
	// noticeCapPerResponse bounds notices per tool response. Two is the
	// attention budget: one flagship + one runner-up; more becomes noise the
	// client learns to ignore (#609's failure mode).
	noticeCapPerResponse = 2

	// intendMaxCues bounds the cue fan-out of a single intention.
	intendMaxCues = 8

	// cueUbiquityMinVault is the vault size below which the ubiquity floor is
	// not applied — in a tiny vault every entity is "ubiquitous" by share and
	// document frequency carries no signal yet.
	cueUbiquityMinVault = 20

	// cueUbiquityDenominator: a cue mentioned by >= n/cueUbiquityDenominator
	// engrams (>=10% of the vault) is refused as an arming cue. Firing on a
	// hub entity would make the intention a nag, not a prompt; this is the
	// same rarity intuition as entityIDF (which zeroes hub-entity evidence),
	// applied as a hard arming gate instead of a soft score.
	cueUbiquityDenominator = 10
)

// ErrInvalidIntention marks a muninn_intend request rejected at validation
// (missing content/cues, too many cues, ubiquitous cue, past valid_until).
// Transports map it to their invalid-params error.
var ErrInvalidIntention = errors.New("invalid intention")

// Notice is one prospective-memory delivery, attached (capped, deduped) to a
// recall/remember response. Why always states the focal trigger so every
// delivery is auditable.
type Notice struct {
	Kind          string `json:"kind"` // "intention" | "contradiction"
	MemoryID      string `json:"memory_id"`
	ConflictsWith string `json:"conflicts_with,omitempty"` // contradiction only
	Note          string `json:"note"`                     // intention content, verbatim
	Cue           string `json:"cue,omitempty"`            // the focal entity that fired it
	Why           string `json:"why"`
	ActionHint    string `json:"action_hint,omitempty"`
	// DedupKey identifies this notice for per-session delivery dedup. Not
	// serialized — the MCP layer records it in session state after attach.
	DedupKey string `json:"-"`
}

// Intend stores a prospective intention: a normal TypeGoal engram (full write
// path — entities, embedding, provenance, valid-time, COG-20 importance) plus
// one 0x2D armed-intention key per cue entity.
//
// Ubiquitous cues are REFUSED loudly (ErrInvalidIntention naming the cue):
// arming a hub entity turns the notice channel into a nag. valid_until is a
// BOUND, not a trigger — an expired intention is silenced, never fired.
func (e *Engine) Intend(ctx context.Context, vault, content string, cues []string, validUntil *time.Time, oneShot bool, importance *float32) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("%w: content is required", ErrInvalidIntention)
	}
	seen := make(map[string]struct{}, len(cues))
	clean := make([]string, 0, len(cues))
	for _, c := range cues {
		trimmed := strings.TrimSpace(c)
		norm := keys.NormalizeEntityName(trimmed)
		if norm == "" {
			continue
		}
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		clean = append(clean, trimmed)
	}
	if len(clean) == 0 {
		return "", fmt.Errorf("%w: at least one non-empty cue entity is required", ErrInvalidIntention)
	}
	if len(clean) > intendMaxCues {
		return "", fmt.Errorf("%w: at most %d cues per intention (got %d)", ErrInvalidIntention, intendMaxCues, len(clean))
	}
	if validUntil != nil && !validUntil.After(time.Now()) {
		return "", fmt.Errorf("%w: valid_until is already in the past — time silences intentions, it never fires them", ErrInvalidIntention)
	}

	// IDF arming floor: refuse cues that are focal in most exchanges already.
	ws := e.store.ResolveVaultPrefix(vault)
	n := e.store.GetVaultCount(ctx, ws)
	if n >= cueUbiquityMinVault {
		for _, cue := range clean {
			rec, err := e.store.GetEntityRecord(ctx, cue)
			if err != nil || rec == nil {
				continue // unknown entity: maximally rare, always armable
			}
			df := int64(rec.MentionCount)
			if df*int64(cueUbiquityDenominator) >= n {
				return "", fmt.Errorf(
					"%w: cue %q is too ubiquitous to arm — it is mentioned by %d of %d memories (idf=%.3f); an intention on a hub entity would fire on most exchanges. Pick a rarer, more specific cue entity",
					ErrInvalidIntention, cue, df, n, entityIDF(df, n))
			}
		}
	}

	ents := make([]mbp.InlineEntity, 0, len(clean))
	for _, cue := range clean {
		ents = append(ents, mbp.InlineEntity{Name: cue, Type: "concept"})
	}
	resp, err := e.Write(ctx, &mbp.WriteRequest{
		Vault:      vault,
		Content:    content,
		MemoryType: uint8(storage.TypeGoal),
		TypeLabel:  "intention",
		Tags:       []string{"prospective"},
		ValidUntil: validUntil,
		Importance: importance,
		Entities:   ents,
	})
	if err != nil {
		return "", fmt.Errorf("intend: write intention engram: %w", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		return "", fmt.Errorf("intend: unparseable engram id %q: %w", resp.ID, err)
	}
	if err := e.store.ArmIntention(ctx, ws, id, clean, oneShot); err != nil {
		// Loud partial-state report: the goal engram exists but is not armed.
		return "", fmt.Errorf("intend: intention stored as %s but arming its cues failed: %w", resp.ID, err)
	}
	return resp.ID, nil
}

// ScoredResult is one returned recall result as NoticesForRecall needs it:
// the engram ID plus the final recall score (used to identify the TOP result
// for the focality rule below).
type ScoredResult struct {
	ID    string
	Score float64
}

// NoticesForRecall computes pending notices for a recall response.
//
// Focality rule (the precision guard, design risk #1): an entity is FOCAL iff
// it is carried by the TOP-scored result, or corroborated by at least two of
// the returned results. "Focal" means what the exchange is actually about —
// a single weak-tail result dragged in by a stem collision ("sizing"→"size")
// carries its entities into the response but NOT into the focal set. The rule
// is deliberately scoring-mode independent: the acceptance harness showed
// weighted-sum FTS scores saturating at the blend cap (every result 0.400),
// which makes any score-ratio gate blind exactly when it is needed.
//
// The same gate applies to contradiction notices: a pair is reported only for
// results that are the top result or that carry a focal entity — a flag on a
// tail result is the same precision leak in a different coat.
//
// readOnly (COG-11) suppresses the fired-marker write; sessionSeen provides
// per-session delivery dedup.
func (e *Engine) NoticesForRecall(ctx context.Context, vault string, results []ScoredResult, sessionSeen func(string) bool, readOnly bool) ([]Notice, error) {
	ws := e.store.ResolveVaultPrefix(vault)

	type resultEntities struct {
		id    storage.ULID
		names []string // raw entity names on this result
	}
	ordered := make([]resultEntities, 0, len(results))
	var topIdx = -1
	var topScore float64
	for _, r := range results {
		id, err := storage.ParseULID(r.ID)
		if err != nil {
			continue
		}
		re := resultEntities{id: id}
		if err := e.store.ScanEngramEntities(ctx, ws, id, func(name string) error {
			re.names = append(re.names, name)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("notices: scan result entities: %w", err)
		}
		ordered = append(ordered, re)
		if topIdx == -1 || r.Score > topScore {
			topIdx, topScore = len(ordered)-1, r.Score
		}
	}

	// Count carriers per normalized entity; remember a raw name for display
	// and which result indices contributed (bug #693 needs to re-examine
	// only those, not every (result, entity) pair — see below).
	type carrierInfo struct {
		count        int
		contributors []int // indices into ordered
	}
	carriers := make(map[string]*carrierInfo, 8)
	rawName := make(map[string]string, 8)
	for i, re := range ordered {
		seenHere := make(map[string]struct{}, len(re.names))
		for _, name := range re.names {
			norm := keys.NormalizeEntityName(name)
			if _, dup := seenHere[norm]; dup {
				continue
			}
			seenHere[norm] = struct{}{}
			ci, ok := carriers[norm]
			if !ok {
				ci = &carrierInfo{}
				carriers[norm] = ci
			}
			ci.count++
			ci.contributors = append(ci.contributors, i)
			if _, ok := rawName[norm]; !ok {
				rawName[norm] = name
			}
		}
	}

	focalSet := make(map[string]struct{}, len(carriers))

	// #693 self-focality guard: an armed intention's own engram is a TypeGoal
	// engram that carries its own cue as a first-class entity (Intend writes
	// the cue as an inline entity). Left unguarded, that engram coming back
	// in results can self-satisfy its own focality via either path below —
	// so both paths exclude a result's contribution to a cue that THAT VERY
	// result is armed on as an intention. This does NOT exclude TypeGoal
	// engrams generally: a goal engram is still a legitimate corroborator of
	// a DIFFERENT intention's cue, or of its own cue via genuinely separate
	// carriers.
	//
	// Perf: gate the 0x2D point lookups to the entries that could actually
	// flip a focality decision — the top result's own names (bounded by that
	// one engram's entity count) and cues that already reached the raw >=2
	// threshold (below it, self-exclusion changes nothing) — instead of an
	// O(results×names) scan over every result.
	if topIdx >= 0 {
		topID := ordered[topIdx].id
		for _, name := range ordered[topIdx].names {
			selfArmed, err := e.store.IsIntentionArmedOnCue(ctx, ws, name, topID)
			if err != nil {
				return nil, fmt.Errorf("notices: self-focality check (top result): %w", err)
			}
			if selfArmed {
				continue
			}
			focalSet[keys.NormalizeEntityName(name)] = struct{}{}
		}
	}
	for norm, ci := range carriers {
		if ci.count < 2 {
			continue
		}
		real := ci.count
		for _, idx := range ci.contributors {
			re := ordered[idx]
			armed, err := e.store.IsIntentionArmedOnCue(ctx, ws, rawName[norm], re.id)
			if err != nil {
				return nil, fmt.Errorf("notices: self-focality check (corroboration): %w", err)
			}
			if armed {
				real--
			}
		}
		if real >= 2 {
			focalSet[norm] = struct{}{}
		}
	}
	focal := make([]string, 0, len(focalSet))
	for norm := range focalSet {
		focal = append(focal, rawName[norm])
	}
	sort.Strings(focal) // deterministic scan order

	// Contradiction intersection: top result + results carrying a focal entity.
	ids := make([]storage.ULID, 0, len(ordered))
	for i, re := range ordered {
		eligible := i == topIdx
		if !eligible {
			for _, name := range re.names {
				if _, ok := focalSet[keys.NormalizeEntityName(name)]; ok {
					eligible = true
					break
				}
			}
		}
		if eligible {
			ids = append(ids, re.id)
		}
	}
	return e.pendingNotices(ctx, vault, ws, focal, ids, storage.ULID{}, sessionSeen, readOnly)
}

// NoticesForRemember computes pending notices for a remember response. The
// focal set is the caller-supplied inline entities of the write; createdID is
// excluded from firing (self-echo guard: the write that creates an intention
// must not immediately fire it).
func (e *Engine) NoticesForRemember(ctx context.Context, vault string, focal []string, createdID string, sessionSeen func(string) bool) ([]Notice, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	var created storage.ULID
	var results []storage.ULID
	if id, err := storage.ParseULID(createdID); err == nil {
		created = id
		results = []storage.ULID{id}
	}
	return e.pendingNotices(ctx, vault, ws, focal, results, created, sessionSeen, false)
}

// noticeCandidate is an eligible delivery before ranking/capping.
type noticeCandidate struct {
	notice     Notice
	importance float64
	createdAt  time.Time
	// intention-only: what MarkIntentionFired needs after selection.
	isIntention bool
	intentionID storage.ULID
	cues        []string
	oneShot     bool
}

// pendingNotices applies the firing rule. An intention FIRES iff its cue is
// in the focal set AND the intention engram is ValidAt(now) AND not
// soft-deleted/archived AND (one-shot ⇒ never fired) AND not seen this
// session AND not created by this very call. Contradiction notices fire when
// a returned result sits in an unresolved 0x0A pair. At most
// noticeCapPerResponse notices are returned, ranked by EffectiveImportance
// then recency; on delivery each fired intention gets its marker written
// unless readOnly (COG-11: a read must not mutate state).
func (e *Engine) pendingNotices(ctx context.Context, vault string, ws [8]byte, focal []string, resultIDs []storage.ULID, excludeID storage.ULID, sessionSeen func(string) bool, readOnly bool) ([]Notice, error) {
	if sessionSeen == nil {
		sessionSeen = func(string) bool { return false }
	}
	now := time.Now()
	var cands []noticeCandidate

	// Intention notices: only entities focal in THIS call are consulted.
	consulted := make(map[storage.ULID]struct{}, 4)
	for _, cue := range focal {
		armed, err := e.store.ScanArmedForEntity(ctx, ws, cue)
		if err != nil {
			return nil, fmt.Errorf("notices: scan armed intentions for %q: %w", cue, err)
		}
		for _, a := range armed {
			if _, dup := consulted[a.ID]; dup {
				continue // multi-cue intention already considered via an earlier focal entity
			}
			consulted[a.ID] = struct{}{}
			if a.ID == excludeID {
				continue // self-echo: the call that created it must not fire it
			}
			if a.OneShot && a.FiredCount > 0 {
				continue
			}
			if sessionSeen(a.ID.String()) {
				continue
			}
			eng, err := e.store.GetEngram(ctx, ws, a.ID)
			if err != nil || eng == nil {
				continue // intention engram gone; stale 0x2D key never fires
			}
			if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
				continue
			}
			if !eng.ValidAt(now) {
				continue // time silences, never fires
			}
			hint := "Act on this now if it applies; muninn_forget " + a.ID.String() + " disarms it."
			if a.OneShot {
				hint = "Act on this now if it applies — this one-shot intention is consumed by this delivery."
			}
			cands = append(cands, noticeCandidate{
				notice: Notice{
					Kind:       "intention",
					MemoryID:   a.ID.String(),
					Note:       eng.Content,
					Cue:        cue,
					Why:        fmt.Sprintf("armed intention: cue entity %q is focal in this call's results", cue),
					ActionHint: hint,
					DedupKey:   a.ID.String(),
				},
				importance:  float64(eng.EffectiveImportance()),
				createdAt:   eng.CreatedAt,
				isIntention: true,
				intentionID: a.ID,
				cues:        a.Cues,
				oneShot:     a.OneShot,
			})
		}
	}

	// Contradiction notices: intersect returned results with the durable 0x0A
	// pairs — zero new detection, focal by construction (the agent just
	// retrieved one side of the conflict).
	if len(resultIDs) > 0 {
		pairs, err := e.store.GetContradictions(ctx, ws)
		if err != nil {
			return nil, fmt.Errorf("notices: get contradictions: %w", err)
		}
		if len(pairs) > 0 {
			inResults := make(map[storage.ULID]struct{}, len(resultIDs))
			for _, id := range resultIDs {
				inResults[id] = struct{}{}
			}
			for _, p := range pairs {
				hit, other := p[0], p[1]
				if _, ok := inResults[hit]; !ok {
					if _, ok := inResults[other]; !ok {
						continue
					}
					hit, other = p[1], p[0]
				}
				dedup := "contradiction:" + p[0].String() + ":" + p[1].String()
				if sessionSeen(dedup) {
					continue
				}
				eng, err := e.store.GetEngram(ctx, ws, hit)
				if err != nil || eng == nil {
					continue
				}
				cands = append(cands, noticeCandidate{
					notice: Notice{
						Kind:          "contradiction",
						MemoryID:      hit.String(),
						ConflictsWith: other.String(),
						Note:          fmt.Sprintf("memory %s in these results is flagged as contradicting %s (unresolved)", hit.String(), other.String()),
						Why:           "a returned result is one side of an unresolved contradiction pair",
						ActionHint:    "Read both and resolve: muninn_evolve the stale one, or muninn_forget it with not_true_since.",
						DedupKey:      dedup,
					},
					importance: float64(eng.EffectiveImportance()),
					createdAt:  eng.CreatedAt,
				})
			}
		}
	}

	if len(cands) == 0 {
		return nil, nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].importance != cands[j].importance {
			return cands[i].importance > cands[j].importance
		}
		return cands[i].createdAt.After(cands[j].createdAt)
	})
	if len(cands) > noticeCapPerResponse {
		cands = cands[:noticeCapPerResponse]
	}

	notices := make([]Notice, 0, len(cands))
	for _, c := range cands {
		if c.isIntention && !readOnly {
			if err := e.store.MarkIntentionFired(ctx, ws, c.intentionID, c.cues, c.oneShot); err != nil {
				// Degrade loudly-but-gracefully: the notice still delivers;
				// session dedup bounds re-delivery within this session.
				slog.Warn("prospective: failed to mark intention fired", "vault", vault, "intention", c.intentionID.String(), "err", err)
			}
		}
		notices = append(notices, c.notice)
	}
	return notices, nil
}
