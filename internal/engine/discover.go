package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// discoverReader is the narrow, READ-ONLY view of storage that muninn_discover
// is implemented against. It is the structural enforcement of COG-22: the type
// simply has no write methods on it, so runDiscover cannot mutate the store no
// matter what the implementation does — the bad state (discovery writing
// association weights, access metadata, or Hebbian/PAS events) is
// unrepresentable, not merely policy-checked (principle #3).
//
// *storage.PebbleStore satisfies this interface structurally (Go interfaces
// are implicit) — nothing needs to change in storage to keep discovery narrow.
type discoverReader interface {
	ResolveVaultPrefix(vault string) [8]byte
	ScanVaultEntityNames(ctx context.Context, ws [8]byte, fn func(name string) error) error
	GetEntityRecord(ctx context.Context, name string) (*storage.EntityRecord, error)
	ScanEntityEngrams(ctx context.Context, entityName string, fn func(ws [8]byte, engramID storage.ULID) error) error
	ScanEngramEntities(ctx context.Context, ws [8]byte, engramID storage.ULID, fn func(entityName string) error) error
	ListByTagInRange(ctx context.Context, wsPrefix [8]byte, tag string, since, until time.Time, limit int) ([]storage.ULID, error)
	GetMetadata(ctx context.Context, wsPrefix [8]byte, ids []storage.ULID) ([]*storage.EngramMeta, error)
}

// discoverDomainScanLimit bounds the tag-index scan used to resolve a
// tag-selected domain's engram membership. Large enough for any realistic
// single-domain history; the entity-cap below is the real multiple-comparisons
// bound, not this scan limit.
const discoverDomainScanLimit = 200_000

// DiscoverDomain selects one side of a cross-domain discovery request: either
// every entity of a given registry Type (0x1F), or every entity mentioned by
// an engram carrying the given tag. Exactly one must be set.
type DiscoverDomain struct {
	EntityType string
	Tag        string
}

func (d DiscoverDomain) String() string {
	if d.EntityType != "" {
		return "entity_type:" + d.EntityType
	}
	return "tag:" + d.Tag
}

// DiscoverRequest is the input to Engine.Discover. Defaults documented per
// field are applied by NormalizeDiscoverRequest; MCP callers get these
// defaults for free via handleDiscover.
type DiscoverRequest struct {
	Vault      string
	DomainA    DiscoverDomain
	DomainB    DiscoverDomain
	MaxLagDays int       // default 7
	MinSupport int       // default 3, floor — never lowered below 3
	EntityCap  int       // default 200 entities per domain, by distinct-day frequency
	NullIters  int       // default 500 circular-shift permutation draws
	QThreshold float64   // default 0.05 — BH-FDR gate
	From, To   time.Time // optional explicit window; default = observed span of both domains
}

// NormalizeDiscoverRequest fills in defaults and clamps floors. Returns an
// error for a malformed domain selector (both or neither of EntityType/Tag).
func NormalizeDiscoverRequest(req DiscoverRequest) (DiscoverRequest, error) {
	if (req.DomainA.EntityType == "") == (req.DomainA.Tag == "") {
		return req, fmt.Errorf("domain_a: exactly one of entity_type or tag is required")
	}
	if (req.DomainB.EntityType == "") == (req.DomainB.Tag == "") {
		return req, fmt.Errorf("domain_b: exactly one of entity_type or tag is required")
	}
	if req.MaxLagDays <= 0 {
		req.MaxLagDays = 7
	}
	if req.MinSupport < 3 {
		req.MinSupport = 3
	}
	if req.EntityCap <= 0 {
		req.EntityCap = 200
	}
	if req.NullIters <= 0 {
		req.NullIters = 500
	}
	if req.QThreshold <= 0 {
		req.QThreshold = 0.05
	}
	return req, nil
}

// DiscoverCandidate is one surfaced cross-domain connection. Every field is
// required — no optional evidence fields — so a candidate without its
// denominator (support, lift, p, q, both marginals) cannot be constructed.
type DiscoverCandidate struct {
	EntityA         string   `json:"entity_a"`
	EntityB         string   `json:"entity_b"`
	LagDays         int      `json:"lag_days"`
	Support         int      `json:"support"`
	Lift            float64  `json:"lift"`
	PValue          float64  `json:"p_value"`
	QValue          float64  `json:"q_value"`
	MarginalA       int      `json:"marginal_a"`
	MarginalB       int      `json:"marginal_b"`
	WindowDays      int      `json:"window_days"`
	ExampleEngramsA []string `json:"example_engrams_a"`
	ExampleEngramsB []string `json:"example_engrams_b"`
	Statement       string   `json:"statement"`
}

// DiscoverResult is the full response of a discovery run.
type DiscoverResult struct {
	Candidates          []DiscoverCandidate `json:"candidates"`
	TestedPairs         int                 `json:"tested_pairs"`
	WindowFrom          time.Time           `json:"window_from"`
	WindowTo            time.Time           `json:"window_to"`
	WindowDays          int                 `json:"window_days"`
	DroppedBelowSupport int                 `json:"dropped_below_support"`
	DroppedFDR          int                 `json:"dropped_fdr"`
	// Reason is set (and Candidates empty) when the window/support floors
	// could not be cleared — degrade loudly, never silently relax the gate.
	Reason string `json:"reason,omitempty"`
}

// Discover runs the read-only cross-domain co-occurrence analytic. It never
// writes: no association weights, no AccessCount/LastAccess, no Hebbian/PAS
// events (COG-22). Every surfaced candidate carries its full evidence
// (support, lift, permutation p, BH-FDR q, both marginals, window) — language
// is always "co-occurs at lag N", never "causes".
func (e *Engine) Discover(ctx context.Context, req DiscoverRequest) (*DiscoverResult, error) {
	req, err := NormalizeDiscoverRequest(req)
	if err != nil {
		return nil, err
	}
	var reader discoverReader = e.store
	return runDiscover(ctx, reader, req)
}

// entityPresence is the per-entity, day-bucketed presence set built from the
// 0x20 forward index + EngramMeta event time. presence[dayOffset] == true
// means the entity had at least one active (non-soft-deleted, non-archived)
// engram whose EffectiveValidFrom() day fell on window.From + dayOffset days.
// exampleEngrams holds up to 3 engram IDs per entity for the evidence contract.
type entityPresence struct {
	name           string
	presence       []bool
	count          int
	exampleEngrams []string
}

func runDiscover(ctx context.Context, store discoverReader, req DiscoverRequest) (*DiscoverResult, error) {
	ws := store.ResolveVaultPrefix(req.Vault)

	namesA, err := resolveDomainEntities(ctx, store, ws, req.DomainA)
	if err != nil {
		return nil, fmt.Errorf("resolve domain_a: %w", err)
	}
	namesB, err := resolveDomainEntities(ctx, store, ws, req.DomainB)
	if err != nil {
		return nil, fmt.Errorf("resolve domain_b: %w", err)
	}
	// Cross-domain only: entities present in both selectors are not a
	// cross-domain pair and are dropped from both sides.
	for name := range namesA {
		if namesB[name] {
			delete(namesA, name)
			delete(namesB, name)
		}
	}

	if len(namesA) == 0 || len(namesB) == 0 {
		return &DiscoverResult{Reason: fmt.Sprintf(
			"domain population empty after cross-domain dedup: domain_a=%d entities, domain_b=%d entities",
			len(namesA), len(namesB))}, nil
	}

	// Gather (entity, engramID) links for every candidate entity in both
	// domains, restricted to this vault.
	entityEngrams := map[string][]storage.ULID{}
	allNames := make([]string, 0, len(namesA)+len(namesB))
	for name := range namesA {
		allNames = append(allNames, name)
	}
	for name := range namesB {
		allNames = append(allNames, name)
	}
	for _, name := range allNames {
		var ids []storage.ULID
		if err := store.ScanEntityEngrams(ctx, name, func(gotWS [8]byte, id storage.ULID) error {
			if gotWS != ws {
				return nil
			}
			ids = append(ids, id)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("scan entity engrams for %q: %w", name, err)
		}
		entityEngrams[name] = ids
	}

	// One metadata read per distinct engram (metaCache) — the design's stated
	// cost bound: a 0x02 meta read, not a full engram load.
	distinctIDs := map[storage.ULID]struct{}{}
	for _, ids := range entityEngrams {
		for _, id := range ids {
			distinctIDs[id] = struct{}{}
		}
	}
	idList := make([]storage.ULID, 0, len(distinctIDs))
	for id := range distinctIDs {
		idList = append(idList, id)
	}
	metas, err := store.GetMetadata(ctx, ws, idList)
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}
	metaByID := map[storage.ULID]*storage.EngramMeta{}
	for _, m := range metas {
		if m == nil {
			continue
		}
		metaByID[m.ID] = m
	}

	// Determine event day per active engram (EffectiveValidFrom, NOT
	// CreatedAt — see discoverReader doc + design). Skip soft-deleted/archived.
	eventDay := map[storage.ULID]time.Time{}
	var minDay, maxDay time.Time
	for id, m := range metaByID {
		if m.State == storage.StateSoftDeleted || m.State == storage.StateArchived {
			continue
		}
		day := dayBucket(m.EffectiveValidFrom())
		eventDay[id] = day
		if minDay.IsZero() || day.Before(minDay) {
			minDay = day
		}
		if maxDay.IsZero() || day.After(maxDay) {
			maxDay = day
		}
	}

	windowFrom, windowTo := req.From, req.To
	if windowFrom.IsZero() {
		windowFrom = minDay
	}
	if windowTo.IsZero() {
		windowTo = maxDay
	}
	if windowFrom.IsZero() || windowTo.IsZero() || windowTo.Before(windowFrom) {
		return &DiscoverResult{Reason: "no active (non-deleted, non-archived) engrams found in either domain"}, nil
	}
	windowFrom = dayBucket(windowFrom)
	windowTo = dayBucket(windowTo)
	T := int(windowTo.Sub(windowFrom).Hours()/24) + 1
	const minWindowDays = 2 * 7 // two weeks: below this, day-bucket lift is not meaningful
	if T < minWindowDays {
		return &DiscoverResult{Reason: fmt.Sprintf(
			"window %d days < %d minimum for day buckets", T, minWindowDays)}, nil
	}

	buildPresence := func(names map[string]bool) []*entityPresence {
		out := make([]*entityPresence, 0, len(names))
		for name := range names {
			ep := &entityPresence{name: name, presence: make([]bool, T)}
			seenDay := map[int]bool{}
			for _, id := range entityEngrams[name] {
				day, ok := eventDay[id]
				if !ok {
					continue // soft-deleted/archived or metadata-missing
				}
				offset := int(day.Sub(windowFrom).Hours() / 24)
				if offset < 0 || offset >= T {
					continue // outside window
				}
				if !ep.presence[offset] {
					ep.presence[offset] = true
					ep.count++
				}
				if !seenDay[offset] {
					seenDay[offset] = true
					if len(ep.exampleEngrams) < 3 {
						ep.exampleEngrams = append(ep.exampleEngrams, id.String())
					}
				}
			}
			out = append(out, ep)
		}
		// Cap at EntityCap by distinct-day count (not MentionCount — bursty
		// spam doesn't buy a seat), highest-frequency first.
		sort.Slice(out, func(i, j int) bool { return out[i].count > out[j].count })
		if len(out) > req.EntityCap {
			out = out[:req.EntityCap]
		}
		return out
	}

	entitiesA := buildPresence(namesA)
	entitiesB := buildPresence(namesB)

	offsets := deterministicShiftOffsets(T, req.NullIters)

	type rawResult struct {
		a, b    *entityPresence
		lag     int
		support int
		lift    float64
		p       float64
	}
	var raw []rawResult
	for _, a := range entitiesA {
		if a.count == 0 {
			continue
		}
		for _, b := range entitiesB {
			if b.count == 0 {
				continue
			}
			for lag := 0; lag <= req.MaxLagDays; lag++ {
				k := dayLagCoOccurrence(a.presence, b.presence, lag)
				if k < req.MinSupport {
					continue
				}
				lift := liftScore(k, T, a.count, b.count)
				p := circularShiftPValue(a.presence, b.presence, lag, T, a.count, offsets, lift)
				raw = append(raw, rawResult{a: a, b: b, lag: lag, support: k, lift: lift, p: p})
			}
		}
	}

	testedPairs := len(raw)
	pvals := make([]float64, len(raw))
	for i, r := range raw {
		pvals[i] = r.p
	}
	qvals := benjaminiHochberg(pvals)

	candidates := make([]DiscoverCandidate, 0, len(raw))
	droppedFDR := 0
	for i, r := range raw {
		if qvals[i] > req.QThreshold {
			droppedFDR++
			continue
		}
		candidates = append(candidates, DiscoverCandidate{
			EntityA:         r.a.name,
			EntityB:         r.b.name,
			LagDays:         r.lag,
			Support:         r.support,
			Lift:            r.lift,
			PValue:          r.p,
			QValue:          qvals[i],
			MarginalA:       r.a.count,
			MarginalB:       r.b.count,
			WindowDays:      T,
			ExampleEngramsA: r.a.exampleEngrams,
			ExampleEngramsB: r.b.exampleEngrams,
			Statement: fmt.Sprintf(
				"%s and %s co-occur at lag %d day(s) (lift %.2f, support %d/%d, p=%.4f, q=%.4f)",
				r.a.name, r.b.name, r.lag, r.lift, r.support, T, r.p, qvals[i]),
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		si := candidates[i].Lift * math.Log(1+float64(candidates[i].Support))
		sj := candidates[j].Lift * math.Log(1+float64(candidates[j].Support))
		return si > sj
	})

	return &DiscoverResult{
		Candidates:          candidates,
		TestedPairs:         testedPairs,
		WindowFrom:          windowFrom,
		WindowTo:            windowTo,
		WindowDays:          T,
		DroppedBelowSupport: len(entitiesA)*len(entitiesB)*(req.MaxLagDays+1) - testedPairs,
		DroppedFDR:          droppedFDR,
	}, nil
}

// resolveDomainEntities returns the set of entity names belonging to a
// domain selector: either every 0x1F entity of the given Type, or every
// entity mentioned by an engram carrying the given tag.
func resolveDomainEntities(ctx context.Context, store discoverReader, ws [8]byte, d DiscoverDomain) (map[string]bool, error) {
	entities := map[string]bool{}
	switch {
	case d.EntityType != "":
		var names []string
		if err := store.ScanVaultEntityNames(ctx, ws, func(name string) error {
			names = append(names, name)
			return nil
		}); err != nil {
			return nil, err
		}
		for _, name := range names {
			rec, err := store.GetEntityRecord(ctx, name)
			if err != nil {
				return nil, err
			}
			if rec != nil && rec.Type == d.EntityType {
				entities[name] = true
			}
		}
	case d.Tag != "":
		ids, err := store.ListByTagInRange(ctx, ws, d.Tag, time.Time{}, time.Time{}, discoverDomainScanLimit)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if err := store.ScanEngramEntities(ctx, ws, id, func(name string) error {
				entities[name] = true
				return nil
			}); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("domain must specify entity_type or tag")
	}
	return entities, nil
}

// dayBucket truncates t to a UTC calendar-day boundary — the "day" bucket
// (increment 1's only bucket granularity).
func dayBucket(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
