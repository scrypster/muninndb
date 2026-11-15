package engine

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/metrics"
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/writetwin"
)

// Write-time twin check (COG-35, design 2026-11-14 write-time push v3).
//
// A new write whose vector is very close to another write made in the same
// vault within the last hour is, on two production vaults' blind-judged
// replay, usually a duplicate or an undeclared update. This file finds such
// pairs WITHOUT touching the write:
//
//   - The vector is never computed here. The retroactive embed processor
//     already embeds every write; its post-store hook (WriteTwinOnEmbedded)
//     hands this file the exact vector it stored and indexed. Shadow mode
//     therefore adds no embed call at all, and mode 1 adds none either — the
//     inline path only WAITS, bounded, for that vector.
//   - The lookup is pure: reads go through ContextWithNoAccessCacheStamp, and
//     nothing here calls Activate, TouchAccess, the Hebbian/PAS workers, or
//     writes a recall event. Naming a memory is not a use of it.
//   - Only live chain HEADS are named (retired versions resolve forward).
//   - No state is keyed on a client-supplied session id.

// WriteTwinCaller describes a writer for the shadow log. It is DESCRIPTIVE
// only — on a single-static-token install scripted and agent writers cannot
// be told apart by server-controlled identity, so nothing gates on it.
type WriteTwinCaller struct {
	CredentialKind string // static | mk | cap | open | unknown
	Mode           string // full | write | append | observe | ""
	HasOpID        bool
}

type ctxKeyWriteTwinCaller struct{}

// WithWriteTwinCaller stamps ctx with the writer description the shadow log
// records for writes made under it. Transports that know the credential set
// it; writes without it log credential_kind=unknown.
func WithWriteTwinCaller(ctx context.Context, c WriteTwinCaller) context.Context {
	return context.WithValue(ctx, ctxKeyWriteTwinCaller{}, c)
}

func writeTwinCallerFrom(ctx context.Context) WriteTwinCaller {
	if c, ok := ctx.Value(ctxKeyWriteTwinCaller{}).(WriteTwinCaller); ok {
		return c
	}
	return WriteTwinCaller{CredentialKind: "unknown"}
}

// WriteTwinCandidate is one named head for the inline block.
type WriteTwinCandidate struct {
	ID         string
	Concept    string
	Similarity float64
	CreatedAt  time.Time
}

// WriteTwinResult is what the inline (mode 1) check found. nil means the
// check does not apply (mode off/shadow, or the vault is not opted in), and
// the response must be byte-identical to a plain write.
type WriteTwinResult struct {
	// Unavailable, when non-empty, is the reason the check could not run:
	// embedder_busy | embed_timeout | no_model_hint | too_homogeneous |
	// cold_start | calibrating | no_embedder | internal_error.
	Unavailable     string
	Threshold       float64
	ThresholdSource string
	Considered      int
	Candidates      []WriteTwinCandidate
}

// Unavailable reasons.
const (
	twinEmbedderBusy   = "embedder_busy"
	twinEmbedTimeout   = "embed_timeout"
	twinNoModelHint    = "no_model_hint"
	twinNoEmbedder     = "no_embedder"
	twinCalibrating    = "calibrating"
	twinInternalError  = "internal_error"
	twinMaxInlineWait  = 100 * time.Millisecond
	twinQueueSize      = 256
	twinGuardMaxAge    = time.Hour
	twinMetaCap        = 8192
	twinNameRefreshGap = time.Minute
)

type twinMeta struct {
	ws      [8]byte
	vault   string
	batchID uint64
	caller  WriteTwinCaller
	noted   time.Time
}

type twinJob struct {
	ws          [8]byte
	entry       writetwin.Entry
	meta        twinMeta
	inlineOwned bool
	gen         uint64
}

type twinGuard struct {
	rank1      []float64
	computedAt time.Time
	computing  bool
}

// passSource is the slice of the embed processor the inline path reads.
type passSource interface {
	PassStartedAt() (time.Time, bool)
}

type writeTwin struct {
	mode writetwin.Mode
	ring *writetwin.Ring
	now  func() time.Time

	mu       sync.Mutex
	meta     map[[16]byte]twinMeta
	waiters  map[[16]byte]chan writetwin.Entry
	names    map[[8]byte]string
	namesAt  time.Time
	gens     map[[8]byte]uint64
	guards   map[[8]byte]*twinGuard
	batchSeq uint64
	ewma     time.Duration // measured delivery time for inline waits
	feed     passSource    // nil = no embed processor wired → no_embedder

	jobs    chan twinJob
	stop    chan struct{}
	stopped sync.Once

	idleMu   sync.Mutex
	idleCond *sync.Cond
	inflight int

	// guardMinSample is writetwin.GuardMinSample in production; tests of the
	// guard's store wiring lower it rather than writing 500 engrams.
	guardMinSample int
	// after is the inline wait's timer (time.After in production); tests
	// inject one that records the budget and fires on command.
	after func(time.Duration) <-chan time.Time
	// testAfterVector, when set, runs on the inline path after the vector
	// arrived (a panic-injection seam for TestWriteTwin_PanicAfterCommit…).
	testAfterVector func()
	// testOnWait, when set, runs once the inline path has registered its
	// waiter — the deterministic point for a test to deliver the vector.
	testOnWait func()
}

func newWriteTwin(mode writetwin.Mode) *writeTwin {
	t := &writeTwin{
		mode:           mode,
		ring:           writetwin.NewRing(nil),
		now:            time.Now,
		meta:           make(map[[16]byte]twinMeta),
		waiters:        make(map[[16]byte]chan writetwin.Entry),
		names:          make(map[[8]byte]string),
		gens:           make(map[[8]byte]uint64),
		guards:         make(map[[8]byte]*twinGuard),
		jobs:           make(chan twinJob, twinQueueSize),
		stop:           make(chan struct{}),
		guardMinSample: writetwin.GuardMinSample,
		after:          time.After,
	}
	t.idleCond = sync.NewCond(&t.idleMu)
	return t
}

func (t *writeTwin) begin() {
	t.idleMu.Lock()
	t.inflight++
	t.idleMu.Unlock()
}

func (t *writeTwin) end() {
	t.idleMu.Lock()
	t.inflight--
	if t.inflight == 0 {
		t.idleCond.Broadcast()
	}
	t.idleMu.Unlock()
}

// waitIdle blocks until every queued shadow job and guard computation has
// finished — the deterministic drain for tests (obligation #12).
func (t *writeTwin) waitIdle() {
	t.idleMu.Lock()
	for t.inflight > 0 {
		t.idleCond.Wait()
	}
	t.idleMu.Unlock()
}

func (t *writeTwin) shutdown() {
	t.stopped.Do(func() { close(t.stop) })
}

// SetWriteTwinFeed records the embed processor that feeds the ring (its
// post-store hook must be wired to WriteTwinOnEmbedded). Without a feed the
// inline path reports no_embedder instead of waiting for a vector that will
// never come. No-op when the check is off.
func (e *Engine) SetWriteTwinFeed(src interface{ PassStartedAt() (time.Time, bool) }) {
	if e.twin == nil {
		return
	}
	e.twin.mu.Lock()
	e.twin.feed = src
	e.twin.mu.Unlock()
}

// noteTwinWrite records, for a just-committed engram, what the embed hook
// cannot know: its vault name, its batch call, and the writer description.
// Called on the write path only when the check is on; O(1).
func (e *Engine) noteTwinWrite(ctx context.Context, ws [8]byte, vault string, id storage.ULID, batchID uint64) {
	t := e.twin
	if t == nil {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.meta) >= twinMetaCap {
		// Entries for writes that never embedded (embed failed, no embedder)
		// would otherwise accumulate. Drop the stale ones; if none are stale,
		// drop arbitrary ones — the meta only enriches a log line.
		for k, m := range t.meta {
			if now.Sub(m.noted) > writetwin.Window || len(t.meta) >= twinMetaCap {
				delete(t.meta, k)
			}
		}
	}
	t.meta[id] = twinMeta{ws: ws, vault: vault, batchID: batchID, caller: writeTwinCallerFrom(ctx), noted: now}
	t.names[ws] = vault
}

// nextTwinBatchID allocates one id for one WriteBatch call (0 when off).
func (e *Engine) nextTwinBatchID() uint64 {
	t := e.twin
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.batchSeq++
	return t.batchSeq
}

// WriteTwinOnEmbedded is the embed processor's post-store hook. vec is the
// exact vector just stored and inserted into HNSW for eng (copied by the
// processor). It is cheap and never blocks: insert into the ring, hand the
// vector to a waiting inline check if there is one, and queue the shadow
// work. No-op when the check is off.
func (e *Engine) WriteTwinOnEmbedded(ws [8]byte, eng *storage.Engram, vec []float32) {
	t := e.twin
	if t == nil || eng == nil || len(vec) == 0 {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("write twin: embed hook panicked (the write is unaffected)", "panic", fmt.Sprint(r))
		}
	}()
	t.mu.Lock()
	meta, ok := t.meta[eng.ID]
	if ok {
		delete(t.meta, eng.ID)
	} else {
		meta = twinMeta{ws: ws, caller: WriteTwinCaller{CredentialKind: "unknown"}}
	}
	gen := t.gens[ws]
	now := t.now()
	t.mu.Unlock()
	// Only a WRITE is a candidate. The processor also embeds backlog — a
	// re-embed, a first-time embedder, an import that predates this process —
	// and those engrams were not just written: comparing them with each other
	// would log "twins" nobody wrote in the last hour. A write noted by this
	// process qualifies (even with a back-dated created_at); otherwise only an
	// engram created within the window (evolve/consolidate/decide successors).
	if !ok && now.Sub(eng.CreatedAt) > writetwin.Window {
		return
	}

	entry := writetwin.Entry{ID: eng.ID, Vec: vec, CreatedAt: eng.CreatedAt, BatchID: meta.batchID}
	t.ring.Insert(ws, entry)

	owned := false
	t.mu.Lock()
	if ch, waiting := t.waiters[eng.ID]; waiting {
		delete(t.waiters, eng.ID)
		ch <- entry // buffered(1); the waiter owns the recent rule now
		owned = true
	}
	t.mu.Unlock()

	t.begin()
	select {
	case t.jobs <- twinJob{ws: ws, entry: entry, meta: meta, inlineOwned: owned, gen: gen}:
	default:
		t.end()
		metrics.WriteTwinDroppedTotal.Inc()
	}
}

// resetTwinVault drops the vault's ring, guard and pending identity — called
// by vault clear/delete and re-embed, whose vectors no longer exist.
func (e *Engine) resetTwinVault(ws [8]byte) {
	t := e.twin
	if t == nil {
		return
	}
	t.ring.ClearVault(ws)
	t.mu.Lock()
	t.gens[ws]++
	delete(t.guards, ws)
	t.mu.Unlock()
}

func (e *Engine) runWriteTwinWorker() {
	t := e.twin
	for {
		select {
		case <-t.stop:
			// Drain so waitIdle cannot hang on jobs that will never run.
			for {
				select {
				case <-t.jobs:
					t.end()
				default:
					return
				}
			}
		case job := <-t.jobs:
			e.processTwinJob(job)
			t.end()
		}
	}
}

func (e *Engine) twinVaultName(ws [8]byte, hint string) string {
	if hint != "" {
		return hint
	}
	t := e.twin
	t.mu.Lock()
	name, ok := t.names[ws]
	stale := t.now().Sub(t.namesAt) > twinNameRefreshGap
	t.mu.Unlock()
	if ok || !stale {
		return name
	}
	names, err := e.store.ListVaultNames()
	if err != nil {
		return ""
	}
	t.mu.Lock()
	t.namesAt = t.now()
	for _, n := range names {
		t.names[e.store.ResolveVaultPrefix(n)] = n
	}
	name = t.names[ws]
	t.mu.Unlock()
	return name
}

// twinThreshold resolves T for a vault: the plasticity override, else the
// hint for the vault's recorded embed model (else the process's model).
func (e *Engine) twinThreshold(vault string, ws [8]byte) writetwin.Threshold {
	resolved := e.ResolveVaultPlasticity(vault)
	model := ""
	if m, err := e.store.GetEmbedModel(ws); err == nil {
		model = m
	}
	if model == "" {
		model = e.embedModelName
	}
	return writetwin.ResolveThreshold(resolved.WriteTwinThresholdOverride, model, embedpkg.WriteTwinHint)
}

// twinGuardStatus evaluates the vault's homogeneity guard against t, kicking
// an asynchronous (re)computation when there is none or it is over an hour
// old. "calibrating" means no measurement exists yet in this process.
func (e *Engine) twinGuardStatus(ws [8]byte, thr float64) (status string, share float64) {
	t := e.twin
	t.mu.Lock()
	g := t.guards[ws]
	if g == nil {
		g = &twinGuard{}
		t.guards[ws] = g
	}
	needs := !g.computing && (g.computedAt.IsZero() || t.now().Sub(g.computedAt) > twinGuardMaxAge)
	if needs {
		g.computing = true
	}
	rank1, computed := g.rank1, !g.computedAt.IsZero()
	gen := t.gens[ws]
	t.mu.Unlock()
	if needs {
		t.begin()
		go func() {
			defer t.end()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("write twin: guard computation panicked", "panic", fmt.Sprint(r))
				}
			}()
			samples := e.computeTwinGuard(context.Background(), ws)
			t.mu.Lock()
			if t.gens[ws] == gen {
				if cur := t.guards[ws]; cur != nil {
					cur.rank1, cur.computedAt, cur.computing = samples, t.now(), false
				}
			}
			t.mu.Unlock()
		}()
	}
	if !computed {
		return twinCalibrating, 0
	}
	return writetwin.GuardVerdictMin(rank1, thr, t.guardMinSample)
}

// computeTwinGuard samples the vault's most recent vectored, non-successor
// active engrams (up to GuardMaxSample) and returns each one's nearest
// EARLIER neighbour cosine via HNSW (NaN when none of its 11 nearest is
// earlier — an approximation that can only under-count would-flags for a
// sample whose top neighbours are all newer). Read-only.
func (e *Engine) computeTwinGuard(ctx context.Context, ws [8]byte) []float64 {
	if e.hnswRegistry == nil {
		return nil
	}
	ctx = storage.ContextWithNoAccessCacheStamp(ctx)
	now := e.twin.now()
	var ids []storage.ULID
	for _, back := range []time.Duration{7 * 24 * time.Hour, 30 * 24 * time.Hour, 365 * 24 * time.Hour, 0} {
		since := time.Unix(0, 0)
		if back > 0 {
			since = now.Add(-back)
		}
		got, err := e.store.ListByStateInRange(ctx, ws, storage.StateActive, since, now.Add(time.Minute), 200000)
		if err != nil {
			return nil
		}
		ids = got
		if len(ids) >= 2*writetwin.GuardMaxSample {
			break
		}
	}
	var out []float64
	const chunk = 200
	for end := len(ids); end > 0 && len(out) < writetwin.GuardMaxSample; end -= chunk {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		part := ids[start:end]
		vecs, err := e.store.GetEmbeddings(ctx, ws, part)
		if err != nil {
			return out
		}
		fwd, err := e.store.GetAssociations(ctx, ws, part, 0)
		if err != nil {
			return out
		}
		for i := len(part) - 1; i >= 0 && len(out) < writetwin.GuardMaxSample; i-- {
			if len(vecs[i]) == 0 {
				continue
			}
			successor := false
			for _, a := range fwd[part[i]] {
				if a.RelType == storage.RelSupersedes {
					successor = true
					break
				}
			}
			if successor {
				continue
			}
			res, err := e.hnswRegistry.Search(ctx, ws, vecs[i], 11)
			if err != nil {
				continue
			}
			best := math.NaN()
			for _, r := range res {
				if r.ID == [16]byte(part[i]) || bytes.Compare(r.ID[:], part[i][:]) >= 0 {
					continue
				}
				if math.IsNaN(best) || r.Score > best {
					best = r.Score
				}
			}
			out = append(out, best)
		}
	}
	return out
}

// twinHead resolves a candidate to the live head of its supersedes chain
// under the default view. ok=false when there is nothing nameable, or the
// head is the new memory itself.
func (e *Engine) twinHead(ctx context.Context, ws [8]byte, cand, newID storage.ULID, gate *visibilityGate) (*storage.Engram, bool) {
	eng, err := e.store.GetEngram(ctx, ws, cand)
	if err != nil || eng == nil {
		return nil, false
	}
	nameable := func(x *storage.Engram) bool { return gate.Nameable(ctx, e.store, ws, x) }
	walk := e.resolveSupersessionHead(ctx, ws, cand, gate, nameable)
	var head *storage.Engram
	switch {
	case walk.OK && gate.Admits(ctx, e.store, ws, walk.Head):
		head = walk.Head
	case !walk.OK && walk.Reason == supersessionBlockNotSuperseded && gate.Admits(ctx, e.store, ws, eng):
		head = eng
	}
	if head == nil || head.ID == newID {
		return nil, false
	}
	return head, true
}

type twinNamed struct {
	head *storage.Engram
	sim  float64
}

// twinHeadsFor resolves matches to distinct live heads, best first, at most
// limit, skipping any id in exclude.
func (e *Engine) twinHeadsFor(ctx context.Context, ws [8]byte, newID storage.ULID, matches []writetwin.Match, limit int, exclude map[storage.ULID]bool) []twinNamed {
	gate := newVisibilityGate(&activation.ActivateRequest{}, e.twin.now())
	var out []twinNamed
	for _, m := range matches {
		if len(out) >= limit {
			break
		}
		head, ok := e.twinHead(ctx, ws, storage.ULID(m.ID), newID, gate)
		if !ok || exclude[head.ID] {
			continue
		}
		exclude[head.ID] = true
		out = append(out, twinNamed{head: head, sim: m.Similarity})
	}
	return out
}

func (e *Engine) logTwinFlag(ws [8]byte, newID storage.ULID, newCreated time.Time, n twinNamed, rule string, shown bool, meta twinMeta, thr writetwin.Threshold, guard string) {
	metrics.WriteTwinFlaggedTotal.WithLabelValues(rule, fmt.Sprint(shown)).Inc()
	slog.Info(writetwin.FlagLogMsg,
		"vault_ws", writetwin.HexWS(ws),
		"new_id", newID.String(),
		"head_id", n.head.ID.String(),
		"similarity", math.Round(n.sim*10000)/10000,
		"age_s", int64(newCreated.Sub(n.head.CreatedAt).Seconds()),
		"rule", rule,
		"shown", shown,
		"batch_call", meta.batchID != 0,
		"has_op_id", meta.caller.HasOpID,
		"credential_kind", meta.caller.CredentialKind,
		"mode", meta.caller.Mode,
		"threshold", thr.Value,
		"threshold_source", thr.Source,
		"guard", guard,
	)
}

// processTwinJob is the shadow work for one embedded write: intra-batch
// twins, recent twins (unless an inline check owns them), and historic
// candidates — all logged, none shown.
func (e *Engine) processTwinJob(job twinJob) {
	t := e.twin
	defer func() {
		if r := recover(); r != nil {
			slog.Error("write twin: shadow job panicked (the write is unaffected)", "panic", fmt.Sprint(r))
		}
	}()
	t.mu.Lock()
	stale := t.gens[job.ws] != job.gen
	t.mu.Unlock()
	if stale {
		return
	}
	start := t.now()
	ctx := storage.ContextWithNoAccessCacheStamp(context.Background())
	vault := e.twinVaultName(job.ws, job.meta.vault)
	thr := e.twinThreshold(vault, job.ws)
	if !thr.OK {
		metrics.WriteTwinCheckedTotal.WithLabelValues("retro", "unavailable").Inc()
		metrics.WriteTwinUnavailableTotal.WithLabelValues(twinNoModelHint).Inc()
		return
	}
	guard, _ := e.twinGuardStatus(job.ws, thr.Value)
	newID := storage.ULID(job.entry.ID)
	named := map[storage.ULID]bool{}
	flagged := false

	if job.entry.BatchID != 0 {
		m, _ := t.ring.Scan(job.ws, job.entry, thr.Value, writetwin.SelectBatch)
		for _, n := range e.twinHeadsFor(ctx, job.ws, newID, m, writetwin.MaxShown, named) {
			e.logTwinFlag(job.ws, newID, job.entry.CreatedAt, n, "batch", false, job.meta, thr, guard)
			flagged = true
		}
	}
	if !job.inlineOwned {
		m, _ := t.ring.Scan(job.ws, job.entry, thr.Value, writetwin.SelectRecent)
		for _, n := range e.twinHeadsFor(ctx, job.ws, newID, m, writetwin.MaxShown, named) {
			e.logTwinFlag(job.ws, newID, job.entry.CreatedAt, n, "recent", false, job.meta, thr, guard)
			flagged = true
		}
	}
	if e.hnswRegistry != nil {
		if res, err := e.hnswRegistry.Search(ctx, job.ws, job.entry.Vec, writetwin.HistoricOverFetch); err == nil {
			var hm []writetwin.Match
			for _, r := range res {
				if r.Score < thr.Value || r.ID == job.entry.ID {
					continue
				}
				hm = append(hm, writetwin.Match{ID: r.ID, Similarity: r.Score})
			}
			cutoff := job.entry.CreatedAt.Add(-writetwin.Window)
			for _, n := range e.twinHeadsFor(ctx, job.ws, newID, hm, writetwin.HistoricKeep, named) {
				if !n.head.CreatedAt.Before(cutoff) {
					continue // recent: the ring rule's business, not historic
				}
				e.logTwinFlag(job.ws, newID, job.entry.CreatedAt, n, "historic", false, job.meta, thr, guard)
				flagged = true
			}
		}
	}
	outcome := "none"
	if flagged {
		outcome = "flagged"
	}
	metrics.WriteTwinCheckedTotal.WithLabelValues("retro", outcome).Inc()
	metrics.WriteTwinLookupSeconds.WithLabelValues("retro").Observe(t.now().Sub(start).Seconds())
}

// inlineWaitBudget is N = min(100 ms, 2 × this process's measured delivery
// time for a write's vector). With no measurement yet, the 100 ms cap — a
// stated product budget, not a measurement.
func (t *writeTwin) inlineWaitBudget() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ewma <= 0 {
		return twinMaxInlineWait
	}
	if n := 2 * t.ewma; n < twinMaxInlineWait {
		return n
	}
	return twinMaxInlineWait
}

func (t *writeTwin) observeDelivery(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ewma <= 0 {
		t.ewma = d
		return
	}
	t.ewma = time.Duration(0.8*float64(t.ewma) + 0.2*float64(d))
}

// awaitVector returns the write's own ring entry, waiting at most budget for
// the embed hook to deliver it. It never embeds and never queues on an embed
// lane — the write's vector is computed by the processor that embeds every
// write anyway.
func (t *writeTwin) awaitVector(ctx context.Context, ws [8]byte, id storage.ULID, budget time.Duration) (writetwin.Entry, bool) {
	ch := make(chan writetwin.Entry, 1)
	t.mu.Lock()
	if e, ok := t.ring.Get(ws, id); ok {
		t.mu.Unlock()
		return e, true
	}
	t.waiters[id] = ch
	t.mu.Unlock()
	if t.testOnWait != nil {
		t.testOnWait()
	}

	select {
	case e := <-ch:
		return e, true
	case <-t.after(budget):
	case <-ctx.Done():
	}
	t.mu.Lock()
	delete(t.waiters, id)
	t.mu.Unlock()
	// The hook may have delivered between the timer and the delete.
	select {
	case e := <-ch:
		return e, true
	default:
		return writetwin.Entry{}, false
	}
}

// WriteTwinInline runs the mode-1 check for a just-committed single write. It
// returns nil when the check does not apply (mode off or shadow, or the vault
// has not opted in with write_twin_inline), so the caller's response stays
// byte-identical. It never delays the write beyond the bounded wait, never
// fails it, and recovers its own panics. The caller has already decided the
// credential may see the result (full mode only) and that the write created a
// new memory (not a dedup, idempotent or upsert result).
func (e *Engine) WriteTwinInline(ctx context.Context, vault, newID string) (res *WriteTwinResult) {
	t := e.twin
	if t == nil || t.mode != writetwin.Inline {
		return nil
	}
	if vault == "" {
		vault = "default"
	}
	if !e.ResolveVaultPlasticity(vault).WriteTwinInline {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("write twin: inline check panicked (the committed write is unaffected)", "panic", fmt.Sprint(r))
			metrics.WriteTwinCheckedTotal.WithLabelValues("inline", "unavailable").Inc()
			metrics.WriteTwinUnavailableTotal.WithLabelValues(twinInternalError).Inc()
			res = &WriteTwinResult{Unavailable: twinInternalError}
		}
	}()
	id, err := storage.ParseULID(newID)
	if err != nil {
		return nil
	}
	ws := e.store.ResolveVaultPrefix(vault)
	unavailable := func(reason string, r *WriteTwinResult) *WriteTwinResult {
		metrics.WriteTwinCheckedTotal.WithLabelValues("inline", "unavailable").Inc()
		metrics.WriteTwinUnavailableTotal.WithLabelValues(reason).Inc()
		if r == nil {
			r = &WriteTwinResult{}
		}
		r.Unavailable = reason
		return r
	}

	thr := e.twinThreshold(vault, ws)
	if !thr.OK {
		return unavailable(twinNoModelHint, nil)
	}
	base := &WriteTwinResult{Threshold: thr.Value, ThresholdSource: thr.Source}
	t.mu.Lock()
	feed := t.feed
	t.mu.Unlock()
	if feed == nil {
		return unavailable(twinNoEmbedder, base)
	}
	guard, _ := e.twinGuardStatus(ws, thr.Value)
	if guard != writetwin.GuardOK {
		// No wait: nothing could be shown. The shadow worker still logs this
		// write from the embed hook (no waiter registered → it owns it).
		return unavailable(guard, base)
	}

	committedAt := t.now()
	budget := t.inlineWaitBudget()
	entry, ok := t.awaitVector(ctx, ws, id, budget)
	waited := t.now().Sub(committedAt)
	metrics.WriteTwinInlineWaitSeconds.Observe(waited.Seconds())
	if !ok {
		t.observeDelivery(budget) // censored at the budget: pushes N back up
		if start, busy := feed.PassStartedAt(); busy && start.Before(committedAt) {
			metrics.WriteTwinEmbedSkippedBusyTotal.Inc()
			return unavailable(twinEmbedderBusy, base)
		}
		return unavailable(twinEmbedTimeout, base)
	}
	t.observeDelivery(waited)
	if t.testAfterVector != nil {
		t.testAfterVector()
	}

	start := t.now()
	rctx := storage.ContextWithNoAccessCacheStamp(ctx)
	matches, considered := t.ring.Scan(ws, entry, thr.Value, writetwin.SelectRecent)
	base.Considered = considered
	named := e.twinHeadsFor(rctx, ws, id, matches, writetwin.MaxShown, map[storage.ULID]bool{})
	meta := twinMeta{caller: writeTwinCallerFrom(ctx)}
	for _, n := range named {
		base.Candidates = append(base.Candidates, WriteTwinCandidate{
			ID: n.head.ID.String(), Concept: truncateRunes(n.head.Concept, writetwin.MaxConceptRunes),
			Similarity: n.sim, CreatedAt: n.head.CreatedAt,
		})
		e.logTwinFlag(ws, id, entry.CreatedAt, n, "recent", true, meta, thr, guard)
	}
	outcome := "none"
	if len(named) > 0 {
		outcome = "flagged"
	}
	metrics.WriteTwinCheckedTotal.WithLabelValues("inline", outcome).Inc()
	metrics.WriteTwinLookupSeconds.WithLabelValues("inline").Observe(t.now().Sub(start).Seconds())
	return base
}

func truncateRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
