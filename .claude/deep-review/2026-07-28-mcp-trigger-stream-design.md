# The MCP Trigger Stream — design (increment: server-push of trigger events to opt-in MCP SSE sessions)

**Status**: DESIGN. Verified against origin/develop HEAD `28345a9` (local checkout at `24f3b46`;
the 2 newer commits touch only `internal/engine/engine_entity_boost*.go` and a skill file — nothing
this design reads). **Tier-3** (auth + transport + concurrent session state) → the build gets a
mandatory Opus refuter.

**Honest framing (bake in, do not oversell)**: finding #609 killed ambient-push because current LLM
harnesses only read tool results — no harness today renders an unsolicited MCP notification into the
model's context. This increment is forward-looking infrastructure: opt-in, additive, ready for
better harnesses and for non-LLM/custom MCP clients (dashboards, watchdogs, the web console). It
composes with — and never replaces or degrades — The Push's piggyback `notices` channel
(feat/the-push; `.claude/deep-review/2026-07-28-the-push-prospective-memory-design.md`, which
explicitly defers "SSE notifications/muninn/* wiring" — this increment is that deferral, picked up).
The measurable gate proves the CHANNEL is correct and safe, not uptake.

---

## 0. FINDING FIRST — a live pre-existing bug blocks the contradiction event (principle #9)

The trigger worker reconstructs vault prefixes from the truncated uint32 vault ID:

```go
// internal/engine/trigger/worker.go:102-109
func (w *TriggerWorker) vaultWS(vaultID uint32) [8]byte {
    var ws [8]byte
    ws[0] = byte(vaultID >> 24) ... ws[3] = byte(vaultID)   // bytes 4-7 stay ZERO
    return ws
}
```

But real vault prefixes are **full 8-byte SipHash** (`internal/storage/keys/keys.go:23-28`,
`binary.BigEndian.PutUint64(prefix[:], hashVal)`), and `GetEngrams` requires the exact prefix
(`internal/storage/engram.go:67`, builds `keys.EngramKey(wsPrefix, id)`). A SipHash whose last 4
bytes are zero has probability ~2⁻³². So every worker lookup that uses `vaultWS` —
`handleContradiction` (worker.go:242), `handleCognitive` (worker.go:297), `handleSweep`
(worker.go:403) — **misses against the real PebbleStore**, and `handleContradiction` returns early
on `len(engrams)==0`. Consequence: **contradiction and threshold-crossed pushes never deliver in
production on ANY transport** (REST/gRPC/MBP). Only `new_write` (worker.go:111-173) works, because
the engram rides on the event and no lookup happens. The bug is masked in tests because both trigger
fakes ignore the prefix entirely (`worker_test.go:62-80` — `mockTriggerStore.GetEngrams` drops the
`[8]byte` arg; `VaultPrefix` returns `[8]byte{}`).

This is the silent-substitution failure class (principle #1/#2) living inside the very subsystem
this increment exposes. **File it as its own issue immediately.** Fix shape (in-memory only — no
keyspace, no on-disk, no wire change): carry the full `[8]byte` prefix on the events. Every notify
site already holds it — `engine.go:1357/1901/2763` call
`NotifyContradiction(wsVaultID(wsPrefix), …)` with `wsPrefix` in hand; add `WS [8]byte` to
`ContradictEvent`/`CognitiveEvent` (and to `Subscription` at `SubscribeWithDeliver`, engine.go:2638,
where `e.store.ResolveVaultPrefix(req.Vault)` is already computed) and have the worker use it
instead of `vaultWS`. RED proof: a trigger-worker test with a **real PebbleStore** (SipHash
prefixes) where a contradiction event must produce a push — fails at HEAD, passes with the fix.

**Sequencing decision**: this increment's slice 1 uses `TriggerNewWrite` (provably working today) so
the MCP stream does not gate on the fix. The contradiction event — the flagship for parity with The
Push's notice — lands as slice 2 **after** the prefix fix merges. Do not quietly absorb the fix into
the MCP PR (principle #5: small increments, each naming what it defers).

---

## 1. Ground truth (all file:line at HEAD)

### 1.1 How TriggerSystem emits

- `internal/engine/trigger/system.go:376-436` — `TriggerSystem` owns four buffered event channels
  (`WriteEvents` 1024, `CognitiveEvents` 4096, `ContradictEvents` 256, `EmbedEvents` 1024). Engine
  notify calls are **non-blocking, drop-on-full** (`NotifyWrite` system.go:486-492, etc.).
- Emitters: `engine.go:1446/1970` + `tree.go:358` (`NotifyWrite` after WRITE ACK),
  `engine.go:1357/1901/2763` (`NotifyContradiction` — semantic + explicit_link).
- One worker goroutine (`worker.go:61-100`, started by `TriggerSystem.Start`, system.go:439) drains
  the channels, matches per-vault subscriptions (`registry.ForVault(event.VaultID)`), scores
  (`TriggerScore` system.go:312-341), rate-limits (token bucket, system.go:157-239; contradictions
  get burst overdraft 3, worker.go via `TryConsumeOrBurst`), dedups per-engram (`pushedScores`), and
  hands off to `DeliveryRouter.Send` (worker.go:19-42): **spawns a goroutine per push, 100ms deliver
  timeout, panic-recovered; a deliver error removes the subscription**.
- Built-in abuse protection we get for free: per-vault cap 100 / global cap 1000
  (system.go:444-451), `RateLimit` default 10/s (system.go:453-456), TTL expiry pruned every 30s
  (registry.go:104-118).

### 1.2 The proven transport consumption pattern (to reuse — principle #7)

All three transports do the same thing: **buffered chan + non-blocking deliver closure +
`SubscribeWithDeliver` + defer Unsubscribe**.

- **gRPC** (`internal/transport/grpc/server.go:471-548`): re-resolves vault scope
  (`resolveRequestVault`), then

  ```go
  pushCh := make(chan *trigger.ActivationPush, 32)
  deliver := func(ctx context.Context, push *trigger.ActivationPush) error {
      select {
      case pushCh <- push: return nil
      case <-ctx.Done():   return ctx.Err()   // stream closed → router auto-removes sub
      default:             return nil          // slow client → drop push, keep sub
      }
  }
  subID, err := s.engine.SubscribeWithDeliver(ctx, req, deliver)
  ...
  defer s.engine.Unsubscribe(ctx, subID)
  ```

- **REST SSE** (`internal/transport/rest/server.go:1855-1935`): same closure plus a **T6 slow-
  subscriber circuit breaker**: atomic `consecutiveDrops`, reset on success, and at ≥50 the SSE loop
  disconnects the client so the sub is torn down.
- **MBP** (`internal/transport/mbp/server.go:421-443`): `scopeVault` then `engine.Subscribe` (nil
  deliver; pull model).
- **Engine side** (`internal/engine/engine.go:2627-2670`): `SubscribeWithDeliver` resolves
  `req.Vault → wsPrefix → wsVaultID(wsPrefix)` (uint32, engine.go:3228) and registers a
  `trigger.Subscription` bound to that single vault ID.

### 1.3 How MCP sessions are created, authed, torn down

- **Session open** (`internal/mcp/server.go:393-455`, `handleSSE`, GET /mcp): auth via
  `authFromRequest` (context.go:64-115 — mk_ fail-closed, cap_ fail-closed, static token, open
  server); random 16-byte `sessionID`; `ch := make(chan []byte, 64)`; stored under
  `sseSessionsMu` in `sseSessions map[string]*sseSession` (server.go:33-34, 413-415) with the auth
  context frozen at open: `type sseSession struct { ch chan []byte; auth AuthContext }`
  (server.go:54-57). Teardown: `defer` deletes from the map (server.go:417-421). **The channel is
  never closed** — abandoned sends just fill a garbage-collected buffer (no send-on-closed panic
  possible). Frames drain at server.go:445-452 as `event: message\ndata: <bytes>`.
- **Per-request auth on the session** (`handleSSEMessage`, server.go:459-523): every POST re-runs
  `authFromRequest` AND re-validates the cached session credential live — cap_ (server.go:495-502)
  and mk_ (server.go:509-516) — this is invariant **SEC-10**. Dispatch then runs on `sess.auth`.
- **Vault authorization** is a single choke point: `dispatchToolCall` → `resolveVault(a.Vault,
  args)` (server.go:204, context.go:126-149) — a pinned credential rejects any differing vault and
  never echoes the pinned name (**SEC-7**). Mode enforcement at server.go:215-244 (**SEC-6**:
  every tool in exactly one of `isMutatingTool`/`isReadOnlyTool`; unknown → blocked).
- **Streamable POST** (server.go:528-557): responses mirror to SSE channels found **by token**
  (`findSSEChannelsByToken`, server.go:562-575; empty token → nil, so open-server sessions never
  cross-contaminate). `pushToAll` (server.go:647-655) is already the non-blocking drop-on-full send.
- **Dormant scaffolding**: `internal/mcp/eventbus.go` defines `MCPEvent{Method:
  "notifications/muninn/contradiction"}` and an `EventBus` — **published by nothing, consumed by
  nothing** at HEAD (repo-wide grep). The Push design calls it "stillborn" and keeps it dormant.
  This increment supersedes it; slice 1 does NOT use it (it has no per-session auth routing — a
  vault-keyed broadcast bus is exactly the shape that leaks). Deleting it is a follow-up cleanup.
- **MCP zero trigger wiring today**: `EngineInterface` (internal/mcp/engine.go) has no
  Subscribe/Unsubscribe; nothing in internal/mcp references `trigger.*`.

### 1.4 The Push's notice payload (compose, don't fork)

`engine.Notice` (worktree `internal/engine/prospective.go:64-75`):

```go
type Notice struct {
    Kind          string `json:"kind"`            // "intention" | "contradiction"
    MemoryID      string `json:"memory_id"`
    ConflictsWith string `json:"conflicts_with,omitempty"`
    Note          string `json:"note"`
    Cue           string `json:"cue,omitempty"`
    Why           string `json:"why"`
    ActionHint    string `json:"action_hint,omitempty"`
    DedupKey      string `json:"-"`
}
```

Also reused precedent: The Push threads session identity through ctx
(`noticeSessionCtxKey`, worktree `internal/mcp/prospective.go`) — same technique carries the SSE
session ID to our subscribe handler. We reuse the **field names and JSON shape** of Notice as the
streamed event body; we do NOT import from the worktree branch and we force no change to its files
(one payload, two delivery channels: piggyback = at-next-exchange, stream = immediate).

---

## 2. Mechanism

### 2.1 Opt-in subscribe: a new MCP tool, `muninn_listen`

Default is structurally OFF: no tool call → no subscription → the worker's
`registry.ForVault` returns nothing for this session → zero frames. No env flag needed for
"quiet by default" (unlike The Push's `MUNINN_PROSPECTIVE`, whose firing runs inside every
recall/remember; here nothing runs unless summoned).

Why a tool call (vs SSE query param or init param): it is the only path that traverses the existing
auth choke points — `withMiddleware`-equivalent auth, **SEC-6** mode enforcement, and **SEC-7**
`resolveVault` at server.go:204 — for free. A query param on GET /mcp would need a parallel
vault-authorization path (new code = new leak surface); a tool call reuses the audited one.

```
muninn_listen (args, all optional except none):
  vault        — goes through resolveVault like every tool; pinned keys cannot escape
  context      — []string, semantic filter (trigger.Subscription.Context)
  threshold    — float 0..1, default 0.6
  ttl_seconds  — default 3600, max 86400 (never immortal; registry prunes)
  rate         — pushes/sec, default 5, clamped [1,20] (REST clamps [1,1000]; MCP is deliberately tighter)
  push_on_write— bool, default true (without it only sweep/threshold events fire)
Returns: { subscription_id, vault, expires_at, note: "events arrive as
  notifications/muninn/trigger on this session's SSE stream" }
```

- Classification (**SEC-6**): `isReadOnlyTool` — it never mutates stored data (session-local state
  only). Consequence to state in the tool description: observe-mode keys CAN listen (they can
  already `muninn_recall` the same engrams — no privilege gained); write-mode keys cannot (they
  cannot read, so streaming reads to them would be an escalation — the read-only classification
  blocks this for free, fail-closed).
- Session identity: `handleSSEMessage` knows `sessionID` (server.go:460); stamp it into ctx
  (`sseSessionCtxKey`, the Push's precedent) before `processAndPushSSE`. The handler resolves the
  live `*sseSession` under `sseSessionsMu`. A POST to /mcp with no SSE session for this exact
  session (`handleStreamablePost` path, or plain `handleRPC`) → **loud error**: `"muninn_listen
  requires an open SSE stream on this connection"` (-32002). Never a silent no-op (principle #2).
- Duplicate `muninn_listen` on a session that already holds a live subscription → error
  `"already listening (subscription <id>); muninn_unlisten first"`. (`muninn_unlisten` is deferred —
  slice 1 teardown is disconnect or TTL; see §3.)
- Engine access: `EngineInterface` is untouched. The handler asserts an optional interface
  (the Push's `prospectiveCapable` precedent):

  ```go
  type triggerCapable interface {
      SubscribeWithDeliver(ctx context.Context, req *mbp.SubscribeRequest, deliver trigger.DeliverFunc) (string, error)
      Unsubscribe(ctx context.Context, subID string) error
  }
  ```

  `mcpEngineAdapter` implements it by delegation (engine already exports both —
  engine.go:2638, 2674). Pre-Push test fakes don't implement it → handler fails loudly
  ("trigger stream not supported by this engine"), never pretends.

### 2.2 Auth-scoped fan-out — the bad state is unrepresentable (principles #3, #4)

There is **no broadcast and no filter to get wrong**. The scoping argument, end to end:

1. The ONLY place a vault enters the subscription is `resolveVault(sess.auth.Vault, args)` —
   the same SEC-7 gate every tool call passes. An mk_ key pinned to vault A cannot name vault B
   (context.go:135-143 rejects the mismatch); a cap_ token likewise (its grant IS its pinned
   vault, context.go:88-100). Static-token / open-server sessions have full access by definition
   (context.go:102-114 — matches REST's public-path posture).
2. `SubscribeWithDeliver` binds the subscription to that single vault's uint32 ID
   (engine.go:2638-2652); the registry buckets by vault (`byVault`, registry.go:41-48).
3. The worker fans out strictly `registry.ForVault(event.VaultID)` (worker.go:115, 242-ish, 297).
   An event for vault X can never reach a deliver closure registered under vault Y because **no
   code path iterates across vault buckets**. The deliver closure carries no vault-selection
   logic to audit — it can only ever be handed pushes for the vault it was registered under.

So the exact authorization check is: **subscribe-time `resolveVault` at the existing choke point;
delivery-time isolation is structural (per-vault registry routing)**. A policy check at delivery
time would be weaker — it would exist to compensate for a broadcast that shouldn't exist.

**Revocation residual, closed**: SEC-10 re-validates cached credentials on every POST, but a
server-initiated push has no POST. Without more, a revoked cap_/mk_ session keeps streaming until
TTL. The deliver closure therefore re-validates the cached credential **before enqueuing each
frame** — `s.capKeys.ValidateCapability(sess.auth.Token)` / `s.authKeys.ValidateAPIKey(...)`, the
identical calls from server.go:495-516 — and returns a non-nil error on failure, which makes
`DeliveryRouter` remove the subscription (worker.go:38-41: `err != nil → registry.Remove`). Cost is
bounded by the subscription's own rate limit (≤20/s worst case, one keys-store read per delivered
frame; dropped/rate-limited frames never validate). This extends SEC-10's guarantee: *a revoked
credential cannot keep receiving on an open SSE session* — pin as SEC-10 amendment (§5).

Vault-ID truncation residual (pre-existing, shared with gRPC/REST/MBP): `wsVaultID` keys the
registry on the first 4 bytes of the 8-byte SipHash prefix (engine.go:3228). Two vaults colliding in
those 4 bytes (~2⁻³² per pair) would share a routing bucket — cross-vault trigger *evaluation* (and,
for new_write, cross-vault *delivery*). Not new surface, not this increment's to fix, but the §0
fix (carrying the full `[8]byte` on Subscription/events) makes a full-width registry key a cheap
follow-up. Say so in the PR; do not silently absorb.

### 2.3 The notification: `notifications/muninn/trigger`, and why it fails open

Frame pushed into `sess.ch` (drained by the existing loop at server.go:445-452 — **zero changes to
the SSE writer**):

```json
{"jsonrpc":"2.0","method":"notifications/muninn/trigger","params":{
  "subscription_id":"…","vault":"default","trigger":"new_write",
  "score":0.83,"push_number":4,"at":"2026-07-28T12:00:00Z",
  "notice":{"kind":"new_write","memory_id":"01J…","note":"<engram concept>","why":"<push.Why>"},
  "engram":{"id":"01J…","concept":"…","content":"…"}
}}
```

- `params.notice` reuses The Push's Notice field names verbatim (`kind`, `memory_id`, `note`,
  `why`, later `conflicts_with`/`cue`/`action_hint`) — one payload shape, two channels. The mapping
  lives in a pure function (`activationPushToNotice(push) → map`) so slice 2 (contradiction) only
  extends the `kind` switch. `conflicts_with` is deferred: `ActivationPush` doesn't carry the
  counterpart ID today (worker.go handleContradiction sends two pushes, one per engram, with `Why`
  text only) — adding it is an in-memory struct field, bundled with the §0 fix.
- **Method choice — custom namespaced, NOT `notifications/message`**: (a) JSON-RPC 2.0:
  notifications get no response ever; a receiver that doesn't recognize the method simply drops
  it — no error frame, no broken session. The MCP spec inherits this; our own server already
  models it (server.go:175-178 blanket-202s any `notifications/*`). (b) `notifications/message` is
  the *logging* channel: it requires declaring the `logging` capability, is subject to
  `logging/setLevel` suppression, and harnesses render it as log lines — semantic lying that would
  also spam log UIs (the #609 lesson: don't pretend a channel is something it isn't). (c) The
  namespaced method matches the dormant eventbus's own anticipated naming
  (`notifications/muninn/contradiction`, eventbus.go:12). Custom method + spec-mandated
  ignore-unknown-notifications = fail-open on presentation by construction.
- **Capability advertisement**: `handleInitialize` (server.go:684-702) adds
  `"capabilities": {"tools": {}, "experimental": {"muninn": {"triggerStream": true}}}`. The
  `experimental` bucket exists in the MCP spec precisely for this; clients ignore unknown
  experimental keys. Advertised unconditionally (the tool exists unconditionally; SEC-9 keeps
  toolset filtering advertisement-only and this follows the same doctrine — presentation, not
  security).
- **stdio / POST-only clients**: MuninnDB's MCP surface is HTTP-only (`internal/mcp/server.go` —
  there is no stdio transport in-tree; stdio clients reach us via a proxy that itself speaks HTTP).
  A client that never opens GET /mcp has no `sseSession`, receives nothing, and `muninn_listen`
  errors loudly per §2.1. Every other tool is byte-identical. Clean degradation, no error state.

### 2.4 Delivery discipline & teardown (the -race story)

New session state: `sseSession` gains `triggerSubID string` and `trigDropped atomic.Int64`
(+ nothing else). All map/session mutation stays under the existing `sseSessionsMu`.

Deliver closure (registered via `SubscribeWithDeliver`, runs on `DeliveryRouter`'s per-push
goroutine with its 100ms timeout, worker.go:29-42):

```go
deliver := func(ctx context.Context, push *trigger.ActivationPush) error {
    if err := s.revalidateSessionCred(sess.auth); err != nil { return err }  // → router removes sub
    frame := marshalTriggerNotification(vault, push)                          // JSON-RPC notification
    select {
    case sess.ch <- frame:
        sess.trigDropped.Store(0)
        return nil
    default:                                                                  // 64-buffer full
        if sess.trigDropped.Add(1) >= 50 { return errSlowSubscriber }         // → router removes sub (T6)
        return nil                                                            // drop frame, keep sub
    }
}
```

Shared-state hazards, named:

1. **`sseSessions` map vs worker pushes**: the deliver closure captures the `*sseSession` pointer —
   it never touches the map. Map writes (add server.go:413, delete server.go:418) stay under
   `sseSessionsMu`. No new map access from the worker path at all.
2. **Send-after-teardown**: session defer deletes the map entry, then calls
   `engine.Unsubscribe(triggerSubID)`. `DeliveryRouter` goroutines already in flight may still send
   once into the abandoned buffered channel — harmless (buffered, **never closed** — existing
   discipline, server.go teardown closes nothing), then GC. No send-on-closed panic is possible.
3. **Slow/dead subscriber can never wedge the engine**: three independent guards — the router's own
   per-push goroutine + 100ms timeout (worker.go:36); the non-blocking `select`/`default` drop; the
   ≥50 consecutive-drops error that removes the subscription (REST T6 pattern,
   rest/server.go:1859-1875 + 1921-1927). The trigger worker goroutine itself only ever does
   `registry` reads and `Send` (which spawns) — it never blocks on MCP.
4. **`triggerSubID` write vs teardown read**: written by the `muninn_listen` handler, read by the
   session defer — both under `sseSessionsMu.Lock`. Second `listen` while first is live → rejected
   (§2.1), so no lost-subID leak.
5. **Frame interleaving**: trigger frames and RPC-response frames share `sess.ch`; SSE writes are
   serialized by the single drain loop (server.go:437-454). No interleaved partial frames possible.
6. **Idempotent `registry.Remove`**: both the router (on deliver error) and session teardown may
   remove; `Remove` on a missing ID is a no-op (registry.go:53-56).

All tests touching this run `-race` (CLAUDE.md §3: mandatory for MCP session state; also
drift-and-obligations.md:139).

---

## 3. Minimal first increment (slice 1) + explicit deferrals

**Slice 1 — one real `new_write` event on one opt-in MCP SSE session, auth-scoped** (~5 files in
`internal/mcp/` + tests; zero engine/trigger/storage changes):

1. `sseSession` += `triggerSubID`, `trigDropped` (server.go).
2. ctx session-ID stamping in `handleSSEMessage` (server.go:459) — the Push's ctx-key pattern.
3. `muninn_listen` tool: definition in `allMCPTools` (tools.go), handler in dispatch map
   (server.go:266+), `isReadOnlyTool` classification (context.go), `triggerCapable` assertion,
   deliver closure per §2.4, subscribe via existing `SubscribeWithDeliver`.
4. Teardown: `handleSSE` defer also unsubscribes `triggerSubID`.
5. `handleInitialize` experimental capability line.
6. Registry smoke test + SEC-6 census + `muninn_guide` paragraph (see §6).

**Deferred, by name** (each honest about why):
- **Contradiction event on the stream** (slice 2) — blocked on the §0 prefix bug; also adds
  `conflicts_with` to the notice mapping. This is the parity point with The Push's flagship.
- **`muninn_unlisten`** — slice 1 teardown = disconnect or TTL (≤24h). Cheap to add once the
  double-listen error proves annoying in practice.
- **Trigger-type filtering** (`events: ["contradiction"]` arg) — meaningless until slice 2 gives a
  second event type worth filtering.
- **Replay-of-missed-events on reconnect** — would require persisting events (a keyspace change,
  exactly what §"no on-disk" forbids here) and duplicates The Push's job (the piggyback channel IS
  the "you were away" mechanism). Likely never.
- **Streamable-HTTP GET-stream correlation** (`Mcp-Session-Id` header ↔ SSE stream) — `handleSSE`
  ignores that header today; listen supports the SSE transport's own sessions only. Note in the
  tool's error message.
- **REST/gRPC parity changes** — none; they already have this feature.
- **EventBus deletion** (dead scaffolding) — separate cleanup PR, coordinated with The Push (whose
  design references it as deliberately dormant).
- **Full-width registry vault key** — bundled naturally with the §0 fix, not with this PR.

**No new keyspace / on-disk / wire format**: confirmed. Slice 1 is transport + session state only.
Slice 2's `ConflictsWith`/`WS` fields are in-memory Go structs shared across transports (each
transport maps `ActivationPush` explicitly — grpc/server.go:531-545, rest/server.go:1928-1946 — so
the additions are invisible to their wires). If the refuter finds anything requiring a byte on disk
or on a wire, that's a design failure to re-litigate, not to sneak in.

---

## 4. Measurable proof (non-gameable gate)

Integration test, `internal/mcp/trigger_stream_test.go`, real `engine.Engine` over a temp
PebbleStore + real `trigger.TriggerSystem` (started), real `httptest.Server` MCP. Run under
`-race`, `-tags localassets`. All assertions on the raw SSE byte stream — nothing mocked between
write and wire.

- **G1 — delivery**: session A (mk_ key pinned to vault `alpha`) opens GET /mcp, calls
  `muninn_listen` (threshold 0, push_on_write). A `muninn_remember` into `alpha` →
  within 2s the SSE stream carries a frame with `method == "notifications/muninn/trigger"`,
  `params.vault == "alpha"`, `params.notice.memory_id ==` the created ID.
- **G2 — auth scoping RED (the one that matters)**: with A still listening, write into vault
  `beta` (open/admin credential). Assert **zero** trigger frames referencing beta arrive on A's
  stream within a settle window that provably passes G1's latency. **RED check**: replace the
  `resolveVault` call in the listen handler with the raw `args["vault"]` (i.e., disable the scope
  gate) and re-run — the pinned key can now subscribe to beta, the beta event leaks onto A's
  stream, G2 fails. A gate that passes both ways proves nothing (CLAUDE.md §3.3); this one is shown
  to fail with the check removed. (Structurally the leak requires mis-registering the
  subscription — exactly what the test forces — because delivery-time cross-vault routing does not
  exist to disable.)
- **G3 — revocation**: A listening; revoke A's mk_ key; write into alpha → no frame delivered, and
  the subscription is removed (assert via a second write also delivering nothing + drop counter
  not incrementing — the sub is gone, not muted).
- **G4 — non-SSE client unaffected**: plain POST /mcp client (no GET stream) runs
  `initialize`/`tools/list`/`muninn_remember`/`muninn_recall` — byte-for-byte normal responses;
  `muninn_listen` returns the loud -32002 error, not a hang or a silent success.
- **G5 — dead subscriber never blocks the engine**: open session, listen, then stop reading the
  SSE body while a hot loop writes N=200 engrams (rate limit set high). Assert: all writes ACK
  within normal latency (the engine is unblocked); `trigDropped` exceeded threshold; the
  subscription was removed (registry count back to 0); the trigger worker goroutine is alive
  (subsequent event to a healthy session still delivers). `-race` across the whole thing is the
  concurrency proof for §2.4's hazards.
- **G6 — fail-open framing**: a client that ignores unknown notification methods (the test client
  literally drops them) completes a full tool-call round-trip on the same stream while trigger
  frames interleave — no protocol desync.

Cost: one integration test file, single-process, no Playwright, no cluster — fits the CI budget
(unit-priced, runs in the `go` job). **Not claimed**: that any LLM acts on a frame. #609 says it
won't today. The gate proves channel correctness, isolation, and liveness — uptake is explicitly
out of scope and the PR must say so.

---

## 5. Invariant impacts (checked against docs/internals/*)

- **New invariant to pin — SEC-16 (proposed)**: *"An MCP session receives trigger events only for
  a vault it could `muninn_recall` at subscribe time: the subscription vault is resolved
  exclusively via `resolveVault(sess.auth.Vault, …)` at the dispatch choke point, and delivery is
  per-vault-bucket routing with no cross-bucket path."* Pinned by G2's RED-verified test + a unit
  test asserting the listen handler has no vault input other than `resolveVault`'s output.
- **SEC-10 amendment**: extend from "per-POST re-validation" to "per-POST **and per-server-
  initiated-frame**" — G3 pins it. Update invariants.md:60 wording in the PR.
- **SEC-6**: `muninn_listen` added to `isReadOnlyTool` + `registeredToolNames()` (server.go:370) —
  `TestToolClassification_CoversAllRegisteredHandlers` enforces automatically.
- **SEC-7** unchanged — reused, not re-implemented.
- **SEC-9** (toolset advertisement-only): `muninn_listen` joins toolset profiles as advertisement
  only; dispatch never consults toolsets. Decide which profiles include it (suggest: full only).
- **SEC-13** (cluster writes): not implicated — no client write path; subscriptions are in-memory,
  node-local. Residual to note in the PR: on a follower, triggers fire only for events that node's
  engine emits; cross-node event streaming is explicitly out of scope.
- Keyspace registry: **no new prefix** — nothing to add, collision guard untouched.
- Cognitive invariants (COG-*): untouched — this increment reads pushes; it never scores, decays,
  or reinforces. (`muninn_listen` causes no `TouchAccess`.)

## 6. Cross-surface obligations (drift-and-obligations.md walk)

1. **Obligation #1** (MCP tool handler): update `allMCPTools` in `internal/mcp/tools.go`; the
   registry-parity smoke test (`smoke_exhaustive_test.go`, runs in `cli-integration`) must pass —
   drift-guard hook (🪝) will also warn.
2. **`muninn_guide`** (guide.go): a short "Trigger stream (experimental)" section — what
   `muninn_listen` does, that frames arrive as `notifications/muninn/trigger`, and the honest
   caveat that most LLM harnesses will not surface them (point Push users at `notices` instead).
3. **initialize capabilities** — §2.3's experimental key (this IS the server's announcement).
4. **Web console**: no slice-1 surface. Candidate follow-up: the console (itself an SSE-capable
   client) could show a live trigger feed — noted, not built.
5. **SDKs / REST / gRPC / MBP**: untouched; their subscribe surfaces already exist. MCP SDK docs
   gain the tool automatically via tools/list.
6. **CI budget**: +1 integration test in the existing `go` job (seconds, not minutes). No new job.

## 7. Top 2 risks + mitigations, and mistakes not to reproduce

1. **Risk: session-credential drift** — the stream is authorized once at subscribe; vault ACLs
   (key revocation, cap TTL) change while it lives. *Mitigation*: per-frame re-validation (§2.2)
   + subscription TTL cap 24h + G3. The residual (a still-valid key whose *vault config* changed)
   matches the existing SEC-10 posture for tool calls — no weaker than the audited baseline.
2. **Risk: the §0 prefix bug ships "fixed" only for MCP** — someone patches around it inside the
   MCP mapping instead of fixing the worker, leaving REST/gRPC/MBP contradiction pushes silently
   dead and the two channels behaviorally divergent. *Mitigation*: the bug is filed independently
   with its own RED test against a real PebbleStore; slice 2 hard-depends on that issue number;
   this design refuses to include a workaround.

**Mistakes not to reproduce**: #609's error was measuring ambition by uptake it couldn't have —
this increment is measured structurally (G1-G6) and its docs say "harnesses won't show this yet"
out loud. The silent-substitution class (#582/#585/#589) — every degraded path here is loud:
no-SSE listen errors, unsupported engine errors, revocation kills the sub rather than muting it.
Unbounded-buffer/slow-consumer wedging — three independent guards (§2.4.3), proven by G5 under
`-race`, never trusted from the diff.
