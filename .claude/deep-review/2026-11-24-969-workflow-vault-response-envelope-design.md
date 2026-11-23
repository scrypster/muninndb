# Design: workflow-vault success response gets the MCP content envelope (#969)

Date: 2026-11-24 · Issue: scrypster/muninndb#969 · Base: e7b3f2ce (develop tip)
Scope: MCP tool response shape for `muninn_create_workflow_vault`. Public surface fix.

## Mechanism

`handleCreateWorkflowVault` (internal/mcp/handlers.go:3521) passes a bare `map[string]any`
to `sendResult`, so the JSON-RPC result carries `vault` / `capability_id` /
`capability_secret` at top level with no `content` array. MCP clients render
`result.content` (see `textContent`, internal/mcp/convert.go:203, used by 50 other
handler call sites), so a fully successful create+mint displays as "completed with no
output" and the once-shown `capability_secret` is discarded by the client.

The test helper `decodeRPCResult` (internal/mcp/create_workflow_vault_test.go:32) decodes
the raw result map and asserts fields on it, so it pins the wrong shape and the suite
stays green. A response-shape contract is only pinned if the test decodes through the
same envelope the client uses.

## Minimal first slice

1. Harden `decodeRPCResult` to require the envelope: decode `result.content[0].text` as
   JSON and return that inner map. Every existing success assertion then runs against
   what a client actually sees.
2. Wrap the `handleCreateWorkflowVault` success payload in `textContent(mustJSON(...))`.
   One call site. Field names and values unchanged.

Explicit deferrals (none expected): no change to tool schema text, no other handlers
(verified none other send a bare map), no REST/gRPC surfaces (they do not use this
envelope), no re-mint path for lost secrets.

## Composition with landed work

- RFC #597 / PR #612 introduced the tool; this fix keeps its security structure intact:
  the `wf-` name guard, existence guard, TTL floor/cap, and `IsAPIKey` recursion guard
  (dispatch layer) are untouched. The capability stays shown-once.
- The wire shape converges with the 50 existing `textContent` call sites, so
  `tool_classification_test.go` and the registry smoke obligations see no new surface.

## Invariant impacts

- Fail-closed auth unchanged (the fix is presentation-layer only).
- No keyspace, on-disk format, or auth change. Not Tier-3 by the repo rubric
  (not auth / on-disk format / migration / concurrency / crypto / replication / mbp wire).
- Drift obligations: none triggered (no registry, preset, or prefix change).

## Measurable proof (pre-committed acceptance)

RED first: the hardened `decodeRPCResult` FAILS against unmodified handlers.go (the
envelope assertion cannot find `content`), then PASSES after the one-line wrap. Observed
kill, not predicted. Acceptance rule: the create-workflow-vault test file goes red without
the fix and green with it, and the full `go test -tags localassets ./...` stays green.

## Top risks

1. Any out-of-tree client that read the bare top-level fields would break. Mitigation:
   the fields remain, nested inside the standard `content[0].text` JSON; the only in-tree
   consumers read through MCP and already expect `content`.
2. `mustJSON` swallows marshal failure into `{}`. The payload is static strings, a bool
   and a timestamp; marshal failure is not reachable here.
