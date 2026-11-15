package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/storage"
)

// Label pipeline (slice 2a, spec 2026-11-15). An agent says which memories a
// recall returned were useful; the label is stored as a child row of the
// recall's 0x29 event with the context a later slice needs to correct for
// position and exposure bias. NOTHING IN RANKING READS LABELS IN THIS SLICE.

// ErrObserveForbidden is returned when an observe-mode (read-only) credential
// calls an operation that records state. Defence in depth for non-MCP callers:
// MCP already refuses observe credentials at dispatch.
var ErrObserveForbidden = errors.New("observe-mode credential cannot record feedback")

// FeedbackGroup is one (query id, memory ids, polarity) triple of a feedback
// call. The MCP layer validates shape first; the engine re-checks it.
type FeedbackGroup struct {
	QueryID string
	IDs     []string
	Useful  bool
}

// Feedback channels (muninn_feedback's `source`).
const (
	FeedbackChannelDirect  = "direct"
	FeedbackChannelHandoff = "handoff"
)

// Feedback rejection reasons — the closed label set of
// muninndb_feedback_rejected_total and the status `labels` block.
const (
	labelRejectNotInRecall  = "not_in_recall"
	labelRejectUnknownEvent = "unknown_event"
	labelRejectBadMAC       = "bad_mac"
	labelRejectNoKey        = "no_key"
)

var labelRejectReasons = []string{labelRejectNotInRecall, labelRejectUnknownEvent, labelRejectBadMAC, labelRejectNoKey}

// Feedback call limits, enforced by the engine as well as the MCP layer.
const (
	maxFeedbackGroups      = 50
	maxFeedbackIDsPerGroup = 20
	maxFeedbackIDsPerCall  = 200
)

// labelStatsWindowDays is the trailing window of the status `labels` block.
const labelStatsWindowDays = 7

// Storm thresholds: muninndb_feedback_storm_total{threshold} is incremented
// when a (credential, memory, UTC day) post count first reaches each.
var labelStormThresholds = []int{10, 100}

// In-memory label state is METRICS ONLY (third security review of 2a).
// Nothing below is consulted to allow or refuse an effect on stored state:
// whether a post earns the day's access bump, and whether it is the day's
// first, is decided durably by the day-marker row (storage.
// UpsertRecallDayMarker), so a restart, an eviction or a full table cannot
// grant or deny anything. The storm metric reads the marker's durable post
// count too. What remains here is the status block's rejection tally, and
// losing an entry loses only a count.
//
// Bound: at most labelMaxTrackedVaults entries, one per EXISTING vault (a
// vault that does not exist allocates nothing), each a fixed-size array —
// labelStatsWindowDays slots × (day + one int64 per rejection reason). At the
// cap the least-recently-updated vault's entry is evicted (O(n) scan, only
// when full). ClearVault/DeleteVault drop the vault's entry. ≈ 300 B per
// entry including map overhead → ≈ 300 KB worst case. The tally is per vault
// and has no per-credential part, so identity-less (open/static-token)
// callers share their vault's tally exactly like everyone else.
var labelMaxTrackedVaults = 1024

type labelState struct {
	mu     sync.Mutex
	vaults map[[8]byte]*labelVaultTally
	tick   uint64 // LRU clock for eviction
}

type labelVaultTally struct {
	used  uint64
	since time.Time // this vault's first counted rejection (per vault: no cross-tenant signal)
	slots [labelStatsWindowDays]labelDayTally
}

type labelDayTally struct {
	day    int64
	counts [len(labelRejectReasonsArr)]int64
}

var labelRejectReasonsArr = [...]string{labelRejectNotInRecall, labelRejectUnknownEvent, labelRejectBadMAC, labelRejectNoKey}

func labelReasonIndex(reason string) int {
	for i, r := range labelRejectReasonsArr {
		if r == reason {
			return i
		}
	}
	return -1
}

func (e *Engine) labelClock() time.Time {
	if e.labelNow != nil {
		return e.labelNow()
	}
	return time.Now()
}

func unixDay(t time.Time) int64 { return t.UTC().Unix() / 86400 }

// dropLabelPartition forgets a vault's rejection tally (ClearVault,
// DeleteVault).
func (e *Engine) dropLabelPartition(ws [8]byte) {
	e.labels.mu.Lock()
	delete(e.labels.vaults, ws)
	e.labels.mu.Unlock()
}

// labelPartitionExists is a test seam.
func (e *Engine) labelPartitionExists(ws [8]byte) bool {
	e.labels.mu.Lock()
	defer e.labels.mu.Unlock()
	_, ok := e.labels.vaults[ws]
	return ok
}

func (e *Engine) noteLabelRejected(vault string, ws [8]byte, exists bool, reason string, n int) {
	if n <= 0 {
		return
	}
	metrics.FeedbackRejectedTotal.WithLabelValues(reason).Add(float64(n))
	ri := labelReasonIndex(reason)
	if !exists || ri < 0 {
		return // a vault that does not exist allocates nothing
	}
	now := e.labelClock()
	day := unixDay(now)
	s := &e.labels
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vaults == nil {
		s.vaults = make(map[[8]byte]*labelVaultTally)
	}
	s.tick++
	vt, ok := s.vaults[ws]
	if !ok {
		if len(s.vaults) >= labelMaxTrackedVaults {
			var oldest [8]byte
			var oldestUse uint64 = ^uint64(0)
			for k, v := range s.vaults {
				if v.used < oldestUse {
					oldest, oldestUse = k, v.used
				}
			}
			delete(s.vaults, oldest)
		}
		vt = &labelVaultTally{since: now}
		s.vaults[ws] = vt
	}
	vt.used = s.tick
	slot := &vt.slots[day%labelStatsWindowDays]
	if slot.day != day {
		*slot = labelDayTally{day: day} // lazy sweep of a past day
	}
	slot.counts[ri] += int64(n)
}

// noteStormFromMarker fires the storm metric when a marker's durable post
// count reaches a threshold exactly.
func noteStormFromMarker(posts uint32) {
	for _, th := range labelStormThresholds {
		if int(posts) == th {
			metrics.FeedbackStormTotal.WithLabelValues(fmt.Sprint(th)).Inc()
		}
	}
}

// callerHashes returns the request's credential and session hashes under
// k_q. ok is false when no transport set a caller identity (non-MCP today).
func (e *Engine) callerHashes(ctx context.Context) (cred, sess [8]byte, ok bool) {
	token, session, ok := auth.CallerIdentityFromContext(ctx)
	if !ok {
		return cred, sess, false
	}
	return e.qidKey.CredHash(token), e.qidKey.SessionHash(session), true
}

// withWriterIdentity stamps ctx so the create/evolve provenance of writes
// under it records the writer's session and credential hashes (self-labelling
// needs them). A no-op for callers with no identity (REST/MBP/gRPC — D4) or
// no key wired.
func (e *Engine) withWriterIdentity(ctx context.Context) context.Context {
	if e.qidKey == nil {
		return ctx
	}
	cred, sess, ok := e.callerHashes(ctx)
	if !ok {
		return ctx
	}
	w := storage.WriterIdentity{CredentialHash: hex.EncodeToString(cred[:])}
	if sess != ([8]byte{}) {
		w.SessionHash = hex.EncodeToString(sess[:])
	}
	return storage.ContextWithWriterIdentity(ctx, w)
}

// recallSourceFlags packs a returned row's source flags (storage.RecallSource*).
func recallSourceFlags(s *activation.ScoredEngram) uint8 {
	f := uint8(s.Admission) & storage.RecallSourceAdmissionMask
	if s.SubstitutedFor != (storage.ULID{}) {
		f |= storage.RecallSourceVersionHead
	}
	if s.Components.EntityBoost > 0 {
		f |= storage.RecallSourceEntityBoost
	}
	if s.Components.TransitionBoost > 0 {
		f |= storage.RecallSourcePAS
	}
	if s.Components.HebbianBoost > 0 {
		f |= storage.RecallSourceHebbian
	}
	return f
}

// mintRecallQueryID returns the query id for a recall whose event id is
// eventID: the ULID itself for a normal recall, or the self-verifying ro1. id
// for a read-only one (ro0. with no key wired).
// rows are the returned memory ids in served order; a read-only id carries a
// keyed tag per row so a later label can be checked for membership.
func (e *Engine) mintRecallQueryID(vault string, eventID storage.ULID, readOnly bool, rc, rs [8]byte, rows [][16]byte) string {
	if !readOnly {
		return eventID.String()
	}
	return e.qidKey.MintReadOnlyQueryID(vault, [16]byte(eventID), eventID.String(), rc, rs, rows)
}

// Feedback outcomes. The engine always computes them; the TRANSPORT decides
// who may see them. A caller that cannot read the vault (a write-mode key)
// gets the uniform response, so it learns nothing about existence; a caller
// that can recall in the vault gains nothing from uniformity and needs the
// outcome to self-correct.
const (
	FeedbackOutcomeRecorded = "recorded" // a new label for (query, memory, credential)
	FeedbackOutcomeRepeat   = "repeat"   // the label existed (polarity updated) or the pair repeated in this call
	FeedbackOutcomeRejected = "rejected"

	FeedbackReasonNotInRecall  = "not_in_recall" // the query did not return this memory
	FeedbackReasonSubstituted  = "substituted"   // it returned the memory's current version (HeadID) instead
	FeedbackReasonUnknownQuery = "unknown_query" // no such recall in this vault (or an id from before a secret rotation)
	FeedbackReasonWrongVault   = "wrong_vault"   // the id belongs to another vault (Vault), named only to callers that may read every vault
	FeedbackReasonUntaggedRow  = "untagged_row"  // a read-only id tags only its first 100 rows
	FeedbackReasonNoKey        = "no_key"        // a read-only id from a server with no auth secret
)

// FeedbackOutcome is the result for one (group, id) of a feedback call, in
// input order.
type FeedbackOutcome struct {
	Group   int
	QueryID string
	ID      string
	Outcome string
	Reason  string
	HeadID  string // FeedbackReasonSubstituted: the id the recall returned
	Vault   string // FeedbackReasonWrongVault: the vault the id belongs to
}

// FeedbackOptions are per-call transport decisions.
type FeedbackOptions struct {
	// ProbeOtherVaults: the caller may read every vault (an unpinned
	// credential), so on an unknown id the engine may look for the vault it
	// belongs to and name it. Never set for a vault-pinned credential: it
	// would reveal another vault's existence.
	ProbeOtherVaults bool
}

// RecordFeedbackLabels is RecordFeedbackLabelsDetailed without outcomes.
func (e *Engine) RecordFeedbackLabels(ctx context.Context, vault string, groups []FeedbackGroup, channel string) error {
	_, err := e.RecordFeedbackLabelsDetailed(ctx, vault, groups, channel, FeedbackOptions{})
	return err
}

// RecordFeedbackLabelsDetailed records labels for the memories each group's
// recall returned, and returns one outcome per (group, id) in input order.
// Synchronous: when it returns, every accepted label is committed (and
// replicated).
//
// Gates: cluster single-writer (SEC-13), then observe refusal. Append-mode
// credentials MAY label (SEC-15 amendment): a label creates a child row and,
// on a first positive label of a LIVE memory, strengthens access metadata —
// it never modifies or deletes the memory.
func (e *Engine) RecordFeedbackLabelsDetailed(ctx context.Context, vault string, groups []FeedbackGroup, channel string, opts FeedbackOptions) ([]FeedbackOutcome, error) {
	if err := e.refuseNonLeaderWrite(); err != nil {
		return nil, err
	}
	if auth.ObserveFromContext(ctx) {
		return nil, ErrObserveForbidden
	}
	var ch uint8
	switch channel {
	case "", FeedbackChannelDirect:
		ch, channel = storage.LabelChannelDirect, FeedbackChannelDirect
	case FeedbackChannelHandoff:
		ch = storage.LabelChannelHandoff
	default:
		return nil, fmt.Errorf("%w: feedback source must be direct or handoff", ErrInvalidRequest)
	}

	// Shape validation (the MCP layer already did this with exact messages;
	// the engine is the authority for every other caller), including the
	// size limits, so no caller can make one call unbounded.
	if len(groups) > maxFeedbackGroups {
		return nil, fmt.Errorf("%w: too many groups in one feedback call (max %d)", ErrInvalidRequest, maxFeedbackGroups)
	}
	totalIDs := 0
	for _, g := range groups {
		totalIDs += len(g.IDs)
		if len(g.IDs) > maxFeedbackIDsPerGroup {
			return nil, fmt.Errorf("%w: too many ids in one group (max %d)", ErrInvalidRequest, maxFeedbackIDsPerGroup)
		}
	}
	if totalIDs > maxFeedbackIDsPerCall {
		return nil, fmt.Errorf("%w: too many ids in one feedback call (max %d)", ErrInvalidRequest, maxFeedbackIDsPerCall)
	}
	type resolvedGroup struct {
		kind auth.QueryIDKind
		ro   auth.ParsedReadOnlyID
		e    storage.ULID
		mems []storage.ULID
	}
	resolved := make([]resolvedGroup, len(groups))
	for gi, g := range groups {
		kind, p := auth.ClassifyQueryID(g.QueryID)
		if kind == auth.QueryIDInvalid {
			return nil, fmt.Errorf("%w: groups[%d]: query_id must be the query_id returned by muninn_recall", ErrInvalidRequest, gi)
		}
		eid, err := storage.ParseULID(p.ULID)
		if err != nil {
			return nil, fmt.Errorf("%w: groups[%d]: query_id must be the query_id returned by muninn_recall", ErrInvalidRequest, gi)
		}
		mems := make([]storage.ULID, 0, len(g.IDs))
		for ii, s := range g.IDs {
			m, err := storage.ParseULID(s)
			if err != nil {
				return nil, fmt.Errorf("%w: groups[%d].ids[%d] is not a memory id", ErrInvalidRequest, gi, ii)
			}
			mems = append(mems, m)
		}
		resolved[gi] = resolvedGroup{kind: kind, ro: p, e: eid, mems: mems}
	}

	ws := e.store.ResolveVaultPrefix(vault)
	vaultExists := e.store.VaultNameExists(vault)
	lc, ls, _ := e.callerHashes(ctx)
	if e.qidKey == nil {
		lc, ls = [8]byte{}, [8]byte{}
	}

	// Batch-read the events of every normal query id in one call.
	var eventIDs []storage.ULID
	for _, r := range resolved {
		if r.kind == auth.QueryIDEvent {
			eventIDs = append(eventIDs, r.e)
		}
	}
	var events map[storage.ULID]*storage.RecallEvent
	if len(eventIDs) > 0 {
		var err error
		events, err = e.store.RecallEventEntries(ctx, ws, eventIDs, storage.RecallPurposeLabel)
		if err != nil {
			return nil, fmt.Errorf("record feedback: %w", err)
		}
	}

	// Engram reads in this path must not freshen the L1 recency stamp: a
	// label is not a read.
	readCtx := storage.ContextWithNoAccessCacheStamp(ctx)
	now := e.labelClock()
	seen := make(map[[2]storage.ULID]struct{})
	var out []FeedbackOutcome
	rejectGroup := func(gi int, r resolvedGroup, metricReason, reason, otherVault string) {
		e.noteLabelRejected(vault, ws, vaultExists, metricReason, len(r.mems))
		for _, m := range r.mems {
			out = append(out, FeedbackOutcome{Group: gi, QueryID: groups[gi].QueryID, ID: m.String(), Outcome: FeedbackOutcomeRejected, Reason: reason, Vault: otherVault})
		}
	}
	for gi, r := range resolved {
		useful := groups[gi].Useful
		switch r.kind {
		case auth.QueryIDNoKey:
			rejectGroup(gi, r, labelRejectNoKey, FeedbackReasonNoKey, "")
			continue
		case auth.QueryIDReadOnly:
			if !e.qidKey.VerifyReadOnly(vault, [16]byte(r.e), r.ro) {
				other := ""
				if opts.ProbeOtherVaults {
					other = e.probeReadOnlyVault(vault, r.e, r.ro)
				}
				if other != "" {
					rejectGroup(gi, r, labelRejectBadMAC, FeedbackReasonWrongVault, other)
				} else {
					rejectGroup(gi, r, labelRejectBadMAC, FeedbackReasonUnknownQuery, "")
				}
				continue
			}
		case auth.QueryIDEvent:
			if events[r.e] == nil {
				other := ""
				if opts.ProbeOtherVaults {
					other = e.probeEventVault(vault, r.e)
				}
				if other != "" {
					rejectGroup(gi, r, labelRejectUnknownEvent, FeedbackReasonWrongVault, other)
				} else {
					rejectGroup(gi, r, labelRejectUnknownEvent, FeedbackReasonUnknownQuery, "")
				}
				continue
			}
		}
		// isMember reports a memory's served rank in this group's recall.
		isMember := func(id storage.ULID) (int, bool) {
			if r.kind == auth.QueryIDEvent {
				is := id.String()
				for i := range events[r.e].Entries {
					if events[r.e].Entries[i].ID == is {
						return i + 1, true
					}
				}
				return 0, false
			}
			return e.qidKey.ReadOnlyRank(vault, [16]byte(r.e), r.ro, [16]byte(id))
		}
		for _, m := range r.mems {
			o := FeedbackOutcome{Group: gi, QueryID: groups[gi].QueryID, ID: m.String()}
			pair := [2]storage.ULID{r.e, m}
			if _, dup := seen[pair]; dup {
				o.Outcome = FeedbackOutcomeRepeat // deduped within the call
				out = append(out, o)
				continue
			}
			seen[pair] = struct{}{}

			rank, member := isMember(m)
			if !member {
				o.Outcome, o.Reason = FeedbackOutcomeRejected, FeedbackReasonNotInRecall
				if head := e.labelHead(ctx, ws, m); head != m {
					if _, ok := isMember(head); ok {
						o.Reason, o.HeadID = FeedbackReasonSubstituted, head.String()
					}
				}
				if o.Reason == FeedbackReasonNotInRecall && r.kind == auth.QueryIDReadOnly && r.ro.Rows() >= auth.ROMaxRows {
					o.Reason = FeedbackReasonUntaggedRow
				}
				e.noteLabelRejected(vault, ws, vaultExists, labelRejectNotInRecall, 1)
				out = append(out, o)
				continue
			}

			fresh := storage.RecallLabel{
				LastAt:    now.UnixMilli(),
				Channel:   ch,
				LabelSess: nonZero(ls),
				Rank:      clampU16(rank),
			}
			if useful {
				fresh.Polarity = 1
			}
			var header *storage.RecallROHeader
			if r.kind == auth.QueryIDEvent {
				ev := events[r.e]
				fresh.Membership = storage.LabelMembershipVerified
				fresh.SetSize = clampU16(len(ev.Entries))
				fresh.Source = ev.EntrySource(rank - 1)
				fresh.RecallCred = ev.RecallCred
				fresh.RecallSess = ev.RecallSess
			} else {
				fresh.Membership = storage.LabelMembershipToken
				fresh.SetSize = clampU16(r.ro.Rows())
				fresh.Source = storage.RecallSourceUnknown
				fresh.RecallCred = nonZero(r.ro.RC)
				fresh.RecallSess = nonZero(r.ro.RS)
				header = &storage.RecallROHeader{
					Unverified: 1,
					RecallCred: fresh.RecallCred,
					RecallSess: fresh.RecallSess,
					At:         int64(ulidTimeMs(r.e)),
				}
			}

			live, err := e.labelLive(readCtx, ws, m, now)
			if err != nil {
				return nil, err
			}
			if !live {
				// Hard- or soft-deleted, superseded or expired when labelled:
				// still a label about what the recall served, never a use.
				fresh.Superseded = 1
			}
			fresh.Self = e.selfLabel(ctx, ws, m, fresh.RecallSess)
			_, first, err := e.store.UpsertRecallLabel(ctx, ws, r.e, m, lc, fresh, header)
			if err != nil {
				return nil, fmt.Errorf("record feedback: storage error")
			}
			// The day marker decides the access bump DURABLY: granted only on
			// a first positive label of this (query, memory, credential), for
			// a LIVE memory, and only if this credential has not already
			// earned this memory's bump today.
			mk, _, granted, err := e.store.UpsertRecallDayMarker(ctx, ws, unixDay(now), m, lc, now.UnixMilli(), first && useful && live,
				storage.DayMarkerPost{Membership: fresh.Membership, Positive: useful})
			if err != nil {
				return nil, fmt.Errorf("record feedback: storage error")
			}
			noteStormFromMarker(mk.Posts)
			membership := "verified"
			if fresh.Membership == storage.LabelMembershipToken {
				membership = "token"
			}
			polarity := "neg"
			if useful {
				polarity = "pos"
			}
			metrics.FeedbackLabelsTotal.WithLabelValues(membership, polarity, channel, fmt.Sprint(first)).Inc()
			if first {
				o.Outcome = FeedbackOutcomeRecorded
			} else {
				o.Outcome = FeedbackOutcomeRepeat
			}
			out = append(out, o)

			if granted {
				if err := e.store.TouchAccess(ctx, ws, m); err != nil && !errors.Is(err, storage.ErrNotFound) {
					slog.Warn("feedback: access bump failed", "err", err)
				}
			}
		}
	}
	return out, nil
}

// labelLive reports whether m is a live memory: it exists, is not
// soft-deleted and has not expired. Only a live memory may receive feedback's
// access bump. "Superseded" needs no separate check: both supersession paths
// close the predecessor — evolve soft-deletes it and stamps ValidUntil
// (SupersedeEngram), and an explicit supersedes link stamps ValidUntil
// (Engine.Link) — and a successor-scan here was measured redundant (removing
// it left TestFeedback_NotLiveMemoriesNeverBumped green for every state).
func (e *Engine) labelLive(ctx context.Context, ws [8]byte, m storage.ULID, now time.Time) (bool, error) {
	// Uncached: a cache-filling read here races feedback's own access bump
	// and can leave a stale entry (see GetEngramUncached).
	eng, err := e.store.GetEngramUncached(ctx, ws, m)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("record feedback: storage error")
	}
	return eng != nil && eng.State != storage.StateSoftDeleted && !eng.IsExpired(now), nil
}

// labelHead walks m's supersession chain to its head (an unambiguous chain
// only; a fork or a cycle stops the walk where it is).
func (e *Engine) labelHead(ctx context.Context, ws [8]byte, m storage.ULID) storage.ULID {
	cur := m
	seen := map[storage.ULID]bool{m: true}
	for depth := 0; depth < supersessionMaxDepth; depth++ {
		succ, err := e.store.SupersedersOf(ctx, ws, cur)
		if err != nil || len(succ) != 1 || seen[succ[0]] {
			return cur
		}
		cur = succ[0]
		seen[cur] = true
	}
	return cur
}

// probeEventVault finds the vault a normal query id's event belongs to, for
// a caller allowed to read every vault. Failure path only; one key Get per
// vault.
func (e *Engine) probeEventVault(vault string, id storage.ULID) string {
	names, err := e.store.ListVaultNames()
	if err != nil {
		return ""
	}
	for _, n := range names {
		if n != vault && e.store.RecallEventExists(e.store.ResolveVaultPrefix(n), id) {
			return n
		}
	}
	return ""
}

// probeReadOnlyVault finds the vault an ro1. id was minted for (its MAC binds
// the vault name), for a caller allowed to read every vault. One HMAC per
// vault, failure path only.
func (e *Engine) probeReadOnlyVault(vault string, id storage.ULID, p auth.ParsedReadOnlyID) string {
	names, err := e.store.ListVaultNames()
	if err != nil {
		return ""
	}
	for _, n := range names {
		if n != vault && e.qidKey.VerifyReadOnly(n, [16]byte(id), p) {
			return n
		}
	}
	return ""
}

// selfLabel decides whether memory m was written in the recall's session,
// from the session hash on its create/evolve provenance entry.
func (e *Engine) selfLabel(ctx context.Context, ws [8]byte, m storage.ULID, recallSess []byte) uint8 {
	if len(recallSess) == 0 || e.prov == nil {
		return storage.LabelSelfUnknown
	}
	entries, err := e.prov.Get(ctx, ws, [16]byte(m))
	if err != nil {
		return storage.LabelSelfUnknown
	}
	for _, pe := range entries {
		if pe.Operation != "create" && pe.Operation != "evolve" {
			continue
		}
		if pe.Details == nil || pe.Details.SessionHash == "" {
			return storage.LabelSelfUnknown
		}
		if pe.Details.SessionHash == hex.EncodeToString(recallSess) {
			return storage.LabelSelfSame
		}
		return storage.LabelSelfOther
	}
	return storage.LabelSelfUnknown
}

// LabelStats is the status `labels` block for one vault, over the trailing
// window: post counts from the window's durable day markers, and the
// process-local rejection tally.
type LabelStats struct {
	WindowDays int
	// Posts in the window, by kind: Verified (against the recall event),
	// Token (against a read-only id's row tags), Legacy (engram_id, no label).
	Verified, Token, Legacy int
	Positive, Negative      int
	// MemoryCredentialDays counts (memory, credential, UTC day) with feedback;
	// Bumps how many of them earned the day's access bump.
	MemoryCredentialDays, Bumps int
	// Rejected is process-local: per-reason counts in the window, since
	// RejectedSince (this vault's first rejection this process saw, or zero).
	Rejected      map[string]int64
	RejectedSince time.Time
}

// LabelStats reports the vault's feedback for the trailing 7 days. No ids, no
// content — counts only. Cost: one range scan over the 0x29 keys of the last
// 7 days (ScanRecallDayMarkers), bounded by that window's activity, never by
// the vault's history; no per-call INFO log.
func (e *Engine) LabelStats(ctx context.Context, vault string) (*LabelStats, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	now := e.labelClock()
	today := unixDay(now)
	out := &LabelStats{WindowDays: labelStatsWindowDays, Rejected: make(map[string]int64, len(labelRejectReasons))}
	for _, r := range labelRejectReasons {
		out.Rejected[r] = 0
	}
	err := e.store.ScanRecallDayMarkers(ctx, ws, today-labelStatsWindowDays+1, today, func(_ storage.ULID, _ [8]byte, m *storage.RecallDayMarker) error {
		out.Verified += int(m.Verified)
		out.Token += int(m.Token)
		out.Legacy += int(m.Legacy)
		out.Positive += int(m.Positive)
		out.Negative += int(m.Negative)
		out.MemoryCredentialDays++
		if m.BumpedAt != 0 {
			out.Bumps++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.labels.mu.Lock()
	if vt := e.labels.vaults[ws]; vt != nil {
		out.RejectedSince = vt.since
		for _, slot := range vt.slots {
			if today-slot.day >= labelStatsWindowDays || today < slot.day {
				continue
			}
			for i, v := range slot.counts {
				out.Rejected[labelRejectReasonsArr[i]] += v
			}
		}
	}
	e.labels.mu.Unlock()
	return out, nil
}

func nonZero(b [8]byte) []byte {
	if b == ([8]byte{}) {
		return nil
	}
	out := make([]byte, 8)
	copy(out, b[:])
	return out
}

func clampU16(n int) uint16 {
	if n > 0xFFFF {
		return 0xFFFF
	}
	if n < 0 {
		return 0
	}
	return uint16(n)
}

// ulidTimeMs is the millisecond timestamp in a ULID's first 6 bytes.
func ulidTimeMs(id storage.ULID) uint64 {
	return uint64(id[0])<<40 | uint64(id[1])<<32 | uint64(id[2])<<24 | uint64(id[3])<<16 | uint64(id[4])<<8 | uint64(id[5])
}
