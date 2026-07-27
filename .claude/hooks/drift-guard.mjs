#!/usr/bin/env node
// drift-guard.mjs — mechanical enforcement for the cross-surface obligations in
// docs/internals/drift-and-obligations.md.
//
// That document lists eleven "if a PR touches X, it must also do Y" obligations and is
// honest that most of them have no automated check. This hook covers the four that are
// purely path-shaped, so they stop depending on a reviewer remembering. The other seven
// need judgment (or a build) and stay manual — see the doc.
//
// Replaces .claude/hookify.{sdk-types,api-spec}-drift.local.md, which were never wired to
// anything: the `hookify` plugin is not installed and the repo had no settings.json, so
// neither warning had ever fired. Obligation #3 in drift-and-obligations.md cited the
// sdk-types file as its only protection; that citation is updated in this same commit.
//
// Every rule WARNS, never blocks. Each fires at most once per session, and stays silent if
// the session already touched the surface it would ask about. Add a rule by appending to
// RULES — nothing else here is rule-specific.

import { appendFileSync, existsSync, mkdirSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, relative, sep } from 'node:path'

const RULES = [
  {
    // Obligation #3 — REST request/response types vs. the seven SDKs.
    id: 'sdk-types-drift',
    triggers: (p) => p === 'internal/transport/rest/types.go',
    satisfies: (p) => p.startsWith('sdk/'),
    message: [
      '**[Drift #3] `internal/transport/rest/types.go` was edited — do the SDKs still match?**',
      '',
      'These structs are the contract every SDK is written against, and there is no automated',
      'check. If a field was added, renamed, retyped, or removed:',
      '',
      '- `sdk/python/` + `sdk/muninndb/` (alias stub) — published to PyPI on tag',
      '- `sdk/node/` — `@muninndb/client`, published to npm on tag',
      '- `sdk/php/` — split-pushed to the `muninndb-php` repo on tag',
      '- `sdk/go/`, `sdk/kotlin/`, `sdk/swift/` — ship in-repo, no separate publish',
      '',
      'Bump versions and changelogs for anything published. Purely internal changes',
      '(unexported fields, comments, logic) can ignore this.',
    ],
  },
  {
    // Obligation #2 — REST routes vs. the OpenAPI spec. Pattern-matched rather than a
    // filename list so new handler files are covered the day they land. (The old hookify
    // rule listed admin_vault_handlers.go, which no longer exists.)
    id: 'api-spec-drift',
    triggers: (p) =>
      p.startsWith('internal/transport/rest/') &&
      !p.endsWith('_test.go') &&
      (/handlers?\.go$/.test(p) || p.endsWith('/server.go')),
    satisfies: (p) => p === 'internal/transport/rest/openapi.yaml',
    message: [
      '**[Drift #2] A REST handler was edited — does `openapi.yaml` still match?**',
      '',
      "CI's route-count parity check is informational only (±5 tolerance) and cannot see",
      'field-level drift, so this is on you. If a route was added, removed, or changed shape:',
      '',
      '1. Update the path in `internal/transport/rest/openapi.yaml`',
      '2. Validate: `npx @redocly/cli lint internal/transport/rest/openapi.yaml`',
    ],
  },
  {
    // Obligation #4 — presets are hand-duplicated across Go and the web UI, no parity test.
    id: 'plasticity-preset-drift',
    triggers: (p) => p === 'internal/auth/plasticity.go',
    satisfies: (p) => p === 'web/templates/index.html' || p === 'web/static/js/app.js',
    message: [
      '**[Drift #4] `internal/auth/plasticity.go` was edited — the web UI duplicates these values.**',
      '',
      'Presets are hand-duplicated across Go and the web UI with no parity test. If a preset',
      'value changed or a preset was added:',
      '',
      '- `web/templates/index.html` — the preset cards',
      '- `web/static/js/app.js` — descriptions and radar data',
      '- any docs table listing preset values',
      '',
      'If the preset is derived from another, add a `reflect.DeepEqual`-style pinning test (see #599).',
    ],
  },
  {
    // Obligation #7 — no CI step verifies the generated code is current.
    id: 'proto-regen-drift',
    triggers: (p) => p.startsWith('proto/') && p.endsWith('.proto'),
    satisfies: (p) => p.startsWith('proto/gen/'),
    message: [
      '**[Drift #7] A `.proto` was edited — regenerate `proto/gen/go/`.**',
      '',
      'No CI step verifies the generated code is current, so stale generated Go will pass the',
      'pipeline and fail at runtime. Run the regen and commit the result alongside this change.',
    ],
  },
]

// A broken guard must never break the session: everything below is best-effort and always
// exits 0.
try {
  const input = JSON.parse(await readStdin())
  const filePath = input?.tool_input?.file_path
  if (!filePath) process.exit(0)

  const root = process.env.CLAUDE_PROJECT_DIR || input.cwd || process.cwd()
  const rel = relative(root, filePath).split(sep).join('/')
  // Edits outside the repo are not our business.
  if (!rel || rel.startsWith('..')) process.exit(0)

  const stateDir = join(tmpdir(), 'muninndb-drift-guard', String(input.session_id || 'nosession'))
  mkdirSync(stateDir, { recursive: true })

  const notes = []
  for (const rule of RULES) {
    const satisfied = join(stateDir, `${rule.id}.satisfied`)
    const fired = join(stateDir, `${rule.id}.fired`)

    // Record satisfaction first, so an edit that both satisfies and triggers in the same
    // session resolves in favor of staying quiet.
    if (rule.satisfies(rel)) {
      appendFileSync(satisfied, `${rel}\n`)
      continue
    }
    if (!rule.triggers(rel)) continue
    if (existsSync(satisfied) || existsSync(fired)) continue

    appendFileSync(fired, `${rel}\n`)
    notes.push(rule.message.join('\n'))
  }

  if (notes.length) {
    process.stdout.write(
      JSON.stringify({
        hookSpecificOutput: {
          hookEventName: 'PostToolUse',
          additionalContext: notes.join('\n\n---\n\n'),
        },
      })
    )
  }
} catch {
  // Swallow. A guard that fails closed would be worse than one that misses a warning.
}
process.exit(0)

async function readStdin() {
  const chunks = []
  for await (const chunk of process.stdin) chunks.push(chunk)
  return Buffer.concat(chunks).toString('utf8') || '{}'
}
