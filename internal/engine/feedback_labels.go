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

// Process-local label bookkeeping (two security reviews of 2a). It is
// PARTITIONED so that no bound is shared across tenants:
//
//	labelState.vaults[ws]            one partition per EXISTING vault (created
//	                                 only after VaultNameExists; dropped by
//	                                 ClearVault/DeleteVault) — so the number of
//	                                 partitions is bounded by the vaults that
//	                                 exist, never by caller-supplied names
//	  .rejected                      that vault's 7-day rejection ring
//	  .creds[lc]                     one per-UTC-day partition per credential
//	                                 hash (credentials are authenticated)
//	    .mems[mem]                   {posts, bumped} for memories that exist
//
// Limits and what happens AT each limit, chosen deliberately:
//
//   - labelPerCredentialDayMax distinct memories per (vault, credential, UTC
//     day). At the limit a NEW memory is not tracked for that credential:
//     no access bump (fail CLOSED, for that principal only), DayFirst = 0
//     (a label is never wrongly marked first), no storm count. Entries are
//     NEVER evicted within a day — eviction would forget that a memory was
//     already bumped or labelled and could re-grant a bump or a "first". The
//     whole credential partition resets at its own UTC-day boundary.
//   - labelPerVaultCredentialMax credentials per vault per day. At the limit
//     the vault's partitions from earlier days are reclaimed first (their
//     state is for a past day, so dropping them grants nothing); if none, a
//     NEW credential in that vault is untracked and fails closed as above.
//     This can only affect principals of the same vault, whose credentials
//     are minted by that vault's own full-mode key holders.
//
// None of it is persisted, so a process restart starts a fresh day for
// everyone (at most one extra bump and one extra DayFirst per restart).
var (
	labelPerCredentialDayMax   = 10_000
	labelPerVaultCredentialMax = 4096
)

// labelState is the engine's process-local label bookkeeping. It never
// touches scoring.
type labelState struct {
	mu     sync.Mutex
	vaults map[[8]byte]*labelVaultState
}

type labelVaultState struct {
	rejected [labelStatsWindowDays]labelDayTally
	since    time.Time // this vault's first counted rejection (per vault: no cross-tenant signal)
	creds    map[[8]byte]*labelCredDay
}

type labelCredDay struct {
	day  int64
	mems map[storage.ULID]*labelMemDay
}

type labelMemDay struct {
	posts  int  // accepted label posts today (storm metric, DayFirst)
	bumped bool // an access bump was granted today
}

type labelDayTally struct {
	day    int64
	counts map[string]int64
}

func (e *Engine) labelClock() time.Time {
	if e.labelNow != nil {
		return e.labelNow()
	}
	return time.Now()
}

func unixDay(t time.Time) int64 { return t.UTC().Unix() / 86400 }

// vaultPartitionLocked returns ws's partition, creating it only for a vault
// that exists.
func (s *labelState) vaultPartitionLocked(ws [8]byte, exists bool) *labelVaultState {
	if vs, ok := s.vaults[ws]; ok {
		return vs
	}
	if !exists {
		return nil
	}
	if s.vaults == nil {
		s.vaults = make(map[[8]byte]*labelVaultState)
	}
	vs := &labelVaultState{creds: make(map[[8]byte]*labelCredDay)}
	s.vaults[ws] = vs
	return vs
}

// memEntryLocked returns today's entry for (ws, lc, mem), creating it within
// the bounds above, or nil when the principal is at its own bound.
func (s *labelState) memEntryLocked(ws [8]byte, exists bool, lc [8]byte, mem storage.ULID, day int64) *labelMemDay {
	vs := s.vaultPartitionLocked(ws, exists)
	if vs == nil {
		return nil
	}
	cd, ok := vs.creds[lc]
	if ok && cd.day != day {
		cd.day, cd.mems = day, make(map[storage.ULID]*labelMemDay)
	}
	if !ok {
		if len(vs.creds) >= labelPerVaultCredentialMax {
			for k, c := range vs.creds {
				if c.day != day {
					delete(vs.creds, k) // a past day's state grants nothing
				}
			}
			if len(vs.creds) >= labelPerVaultCredentialMax {
				return nil
			}
		}
		cd = &labelCredDay{day: day, mems: make(map[storage.ULID]*labelMemDay)}
		vs.creds[lc] = cd
	}
	me, ok := cd.mems[mem]
	if !ok {
		if len(cd.mems) >= labelPerCredentialDayMax {
			return nil
		}
		me = &labelMemDay{}
		cd.mems[mem] = me
	}
	return me
}

// dropLabelPartition forgets a vault's label bookkeeping (ClearVault,
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
	now := e.labelClock()
	day := unixDay(now)
	s := &e.labels
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.vaultPartitionLocked(ws, exists) // a vault that does not exist allocates nothing
	if vs == nil {
		return
	}
	if vs.since.IsZero() {
		vs.since = now
	}
	slot := &vs.rejected[day%labelStatsWindowDays]
	if slot.day != day || slot.counts == nil {
		*slot = labelDayTally{day: day, counts: make(map[string]int64, len(labelRejectReasons))}
	}
	slot.counts[reason] += int64(n)
}

// noteLabelPost counts one accepted label post for (ws, lc, mem) today, fires
// the storm metric when the count first reaches a threshold, and returns the
// count — 0 when the principal is at its own bound (untracked: never first).
func (e *Engine) noteLabelPost(ws [8]byte, exists bool, lc [8]byte, mem storage.ULID) int {
	s := &e.labels
	s.mu.Lock()
	me := s.memEntryLocked(ws, exists, lc, mem, unixDay(e.labelClock()))
	if me == nil {
		s.mu.Unlock()
		return 0
	}
	me.posts++
	n := me.posts
	s.mu.Unlock()
	for _, th := range labelStormThresholds {
		if n == th {
			metrics.FeedbackStormTotal.WithLabelValues(fmt.Sprint(th)).Inc()
		}
	}
	return n
}

// accessBumpAllowed reports whether feedback may bump access for (ws, lc,
// mem) today, and records that it did. It is the ONE cap every feedback
// access-bump path goes through — labelled (either membership kind) and
// legacy alike — so 1,000 fresh query ids are still one use per credential,
// memory and UTC day. Callers must only ask for memories that exist. Fails
// closed for a principal at its own bound.
func (e *Engine) accessBumpAllowed(ws [8]byte, exists bool, lc [8]byte, mem storage.ULID) bool {
	s := &e.labels
	s.mu.Lock()
	defer s.mu.Unlock()
	me := s.memEntryLocked(ws, exists, lc, mem, unixDay(e.labelClock()))
	if me == nil || me.bumped {
		return false
	}
	me.bumped = true
	return true
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

type labelTarget struct {
	group   int
	eventID storage.ULID
	memID   storage.ULID
	useful  bool
}

// RecordFeedbackLabels records labels for the memories each group's recall
// returned. Synchronous: when it returns nil, every accepted label is
// committed (and replicated). It never reports per-id outcomes — not
// existence, not membership, not repetition — so its result is uniform for
// every credential; outcomes are visible only in aggregate (metrics, status).
//
// Gates: cluster single-writer (SEC-13), then observe refusal. Append-mode
// credentials MAY label (SEC-15 amendment): a label creates a child row and,
// on a first positive label, strengthens access metadata — it never
// modifies or deletes the memory.
func (e *Engine) RecordFeedbackLabels(ctx context.Context, vault string, groups []FeedbackGroup, channel string) error {
	if err := e.refuseNonLeaderWrite(); err != nil {
		return err
	}
	if auth.ObserveFromContext(ctx) {
		return ErrObserveForbidden
	}
	var ch uint8
	switch channel {
	case "", FeedbackChannelDirect:
		ch, channel = storage.LabelChannelDirect, FeedbackChannelDirect
	case FeedbackChannelHandoff:
		ch = storage.LabelChannelHandoff
	default:
		return fmt.Errorf("%w: feedback source must be direct or handoff", ErrInvalidRequest)
	}

	// Shape validation (the MCP layer already did this with exact messages;
	// the engine is the authority for every other caller), including the
	// size limits, so no caller can make one call unbounded.
	if len(groups) > maxFeedbackGroups {
		return fmt.Errorf("%w: too many groups in one feedback call (max %d)", ErrInvalidRequest, maxFeedbackGroups)
	}
	totalIDs := 0
	for _, g := range groups {
		totalIDs += len(g.IDs)
		if len(g.IDs) > maxFeedbackIDsPerGroup {
			return fmt.Errorf("%w: too many ids in one group (max %d)", ErrInvalidRequest, maxFeedbackIDsPerGroup)
		}
	}
	if totalIDs > maxFeedbackIDsPerCall {
		return fmt.Errorf("%w: too many ids in one feedback call (max %d)", ErrInvalidRequest, maxFeedbackIDsPerCall)
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
			return fmt.Errorf("%w: groups[%d]: query_id must be the query_id returned by muninn_recall", ErrInvalidRequest, gi)
		}
		eid, err := storage.ParseULID(p.ULID)
		if err != nil {
			return fmt.Errorf("%w: groups[%d]: query_id must be the query_id returned by muninn_recall", ErrInvalidRequest, gi)
		}
		mems := make([]storage.ULID, 0, len(g.IDs))
		for ii, s := range g.IDs {
			m, err := storage.ParseULID(s)
			if err != nil {
				return fmt.Errorf("%w: groups[%d].ids[%d] is not a memory id", ErrInvalidRequest, gi, ii)
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
			return fmt.Errorf("record feedback: %w", err)
		}
	}

	// Engram reads in this path must not freshen the L1 recency stamp: a
	// label is not a read.
	readCtx := storage.ContextWithNoAccessCacheStamp(ctx)
	now := e.labelClock()
	seen := make(map[[2]storage.ULID]struct{})
	for gi, r := range resolved {
		useful := groups[gi].Useful
		switch r.kind {
		case auth.QueryIDNoKey:
			e.noteLabelRejected(vault, ws, vaultExists, labelRejectNoKey, len(r.mems))
			continue
		case auth.QueryIDReadOnly:
			if !e.qidKey.VerifyReadOnly(vault, [16]byte(r.e), r.ro) {
				e.noteLabelRejected(vault, ws, vaultExists, labelRejectBadMAC, len(r.mems))
				continue
			}
		case auth.QueryIDEvent:
			if events[r.e] == nil {
				e.noteLabelRejected(vault, ws, vaultExists, labelRejectUnknownEvent, len(r.mems))
				continue
			}
		}
		for _, m := range r.mems {
			pair := [2]storage.ULID{r.e, m}
			if _, dup := seen[pair]; dup {
				continue // deduped within the call
			}
			seen[pair] = struct{}{}

			fresh := storage.RecallLabel{
				LastAt:    now.UnixMilli(),
				Channel:   ch,
				LabelSess: nonZero(ls),
			}
			if useful {
				fresh.Polarity = 1
			}
			var header *storage.RecallROHeader
			if r.kind == auth.QueryIDEvent {
				ev := events[r.e]
				idx := -1
				ms := m.String()
				for i := range ev.Entries {
					if ev.Entries[i].ID == ms {
						idx = i
						break
					}
				}
				if idx < 0 {
					e.noteLabelRejected(vault, ws, vaultExists, labelRejectNotInRecall, 1)
					continue
				}
				fresh.Membership = storage.LabelMembershipVerified
				fresh.Rank = clampU16(idx + 1)
				fresh.SetSize = clampU16(len(ev.Entries))
				fresh.Source = ev.EntrySource(idx)
				fresh.RecallCred = ev.RecallCred
				fresh.RecallSess = ev.RecallSess
			} else {
				rank, member := e.qidKey.ReadOnlyRank(vault, [16]byte(r.e), r.ro, [16]byte(m))
				if !member {
					e.noteLabelRejected(vault, ws, vaultExists, labelRejectNotInRecall, 1)
					continue
				}
				fresh.Membership = storage.LabelMembershipToken
				fresh.Rank = clampU16(rank)
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

			eng, err := e.store.GetEngram(readCtx, ws, m)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("record feedback: storage error")
			}
			exists := err == nil && eng != nil
			if !exists {
				// A verified member that was since hard-deleted: still a
				// label about what the recall served.
				fresh.Superseded = 1
			} else if eng.State == storage.StateSoftDeleted || eng.IsExpired(now) {
				fresh.Superseded = 1
			}
			fresh.Self = e.selfLabel(ctx, ws, m, fresh.RecallSess)
			if e.noteLabelPost(ws, vaultExists, lc, m) == 1 {
				fresh.DayFirst = 1
			}

			_, first, err := e.store.UpsertRecallLabel(ctx, ws, r.e, m, lc, fresh, header)
			if err != nil {
				return fmt.Errorf("record feedback: storage error")
			}
			membership := "verified"
			if fresh.Membership == storage.LabelMembershipToken {
				membership = "token"
			}
			polarity := "neg"
			if useful {
				polarity = "pos"
			}
			metrics.FeedbackLabelsTotal.WithLabelValues(membership, polarity, channel, fmt.Sprint(first)).Inc()

			// A FIRST positive label on a verified member (event or row tag —
			// there is no unverified path) is one use, capped per
			// (credential, memory, UTC day) like every bump path.
			// Synchronous, swallowing not-found. Repeats never bump;
			// useful=false never bumps; a deleted memory is never bumped.
			if first && useful && exists && e.accessBumpAllowed(ws, vaultExists, lc, m) {
				if err := e.store.TouchAccess(ctx, ws, m); err != nil && !errors.Is(err, storage.ErrNotFound) {
					slog.Warn("feedback: access bump failed", "err", err)
				}
			}
		}
	}
	return nil
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

// LabelStats is the status `labels` block for one vault: persisted label rows
// last posted in the trailing window, and the process-local rejection tally.
type LabelStats struct {
	WindowDays int
	// Persisted (survive restarts): label rows whose last post is in the window.
	// Verified: membership checked against the recall event; Token: checked
	// against a read-only id's row tags.
	Verified, Token    int
	Positive, Negative int
	// FirstInWindow counts rows whose FIRST post is in the window.
	FirstInWindow int
	// Rejected is process-local: per-reason counts in the window, since
	// RejectedSince (the first rejection this process saw, or zero).
	Rejected      map[string]int64
	RejectedSince time.Time
}

// LabelStats reports the vault's label outcomes for the trailing 7 days. No
// ids, no content — counts only.
func (e *Engine) LabelStats(ctx context.Context, vault string) (*LabelStats, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	now := e.labelClock()
	cutoff := now.AddDate(0, 0, -labelStatsWindowDays).UnixMilli()
	out := &LabelStats{WindowDays: labelStatsWindowDays, Rejected: make(map[string]int64, len(labelRejectReasons))}
	for _, r := range labelRejectReasons {
		out.Rejected[r] = 0
	}
	err := e.store.ScanRecallLabels(ctx, ws, storage.RecallPurposeLabel, func(row storage.RecallLabelRow) error {
		if row.Label.LastAt < cutoff {
			return nil
		}
		if row.Label.Membership == storage.LabelMembershipToken {
			out.Token++
		} else {
			out.Verified++
		}
		if row.Label.Polarity == 1 {
			out.Positive++
		} else {
			out.Negative++
		}
		if row.Label.FirstAt >= cutoff {
			out.FirstInWindow++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	today := unixDay(now)
	e.labels.mu.Lock()
	if vs := e.labels.vaults[ws]; vs != nil {
		out.RejectedSince = vs.since
		for _, slot := range vs.rejected {
			if slot.counts == nil || today-slot.day >= labelStatsWindowDays {
				continue
			}
			for k, v := range slot.counts {
				out.Rejected[k] += v
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
