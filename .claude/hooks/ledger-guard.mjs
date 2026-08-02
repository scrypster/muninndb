#!/usr/bin/env node
// ledger-guard.mjs — catch an invalid memory proposal in the session that can still fix it.
//
// Shaped after drift-guard.mjs (principle #7: extend a proven in-tree mechanism). Same
// PostToolUse shape, same never-block/always-exit-0 discipline, same additionalContext
// channel back to the model.
//
// Why it is not enough to validate at drain time: 43 of the first 179 proposals were
// permanently invalid, and the drain runs long after the agent that wrote them is gone. The
// failures were contiguous runs — lines 65–70, 100–106, 116–129, 134–137, 159–170 — each one
// invocation getting it wrong for its whole batch. Reported in-session, those 43 lost
// proposals are ~5 corrected agent runs.
//
// Why it hooks Bash as well as Write|Edit: the observed producers appended with a shell
// heredoc, not with the Write tool. The guard checks the FILE, so it does not care how the
// line got there — including a raw `>>` that bypasses memory-propose.mjs entirely.
//
// It re-warns on every change to the ledger while any invalid line remains, deliberately.
// A one-shot warning is a warning that gets deferred; this one stays until the queue is
// clean or the line is removed.

import { readFileSync, writeFileSync, mkdirSync, statSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { validate, explain, CANONICAL_SHAPE } from './memory-schema.mjs'
import { paths } from './memory-ledger.mjs'

const MAX_REPORTED = 8

try {
  const input = JSON.parse(await readStdin())
  const root = process.env.CLAUDE_PROJECT_DIR || input.cwd || process.cwd()
  const P = paths(root)

  let st
  try { st = statSync(P.ledger) } catch { process.exit(0) }   // no ledger, nothing to guard
  if (!st.size) process.exit(0)

  const stateDir = join(tmpdir(), 'muninndb-ledger-guard', String(input.session_id || 'nosession'))
  mkdirSync(stateDir, { recursive: true })
  const stateFile = join(stateDir, 'seen.json')
  let seen = null
  try { seen = JSON.parse(readFileSync(stateFile, 'utf8')) } catch { /* first check this session */ }
  // Nothing appended since the last check → nothing new to say.
  if (seen && seen.size === st.size && seen.mtimeMs === st.mtimeMs) process.exit(0)
  writeFileSync(stateFile, JSON.stringify({ size: st.size, mtimeMs: st.mtimeMs }))

  const bad = []
  const lines = readFileSync(P.ledger, 'utf8').split('\n')
  for (const [i, raw] of lines.entries()) {
    if (!raw.trim()) continue
    let p
    try { p = JSON.parse(raw) } catch { bad.push(`line ${i + 1}: not valid JSON — one object per line, no trailing commas`); continue }
    const v = validate(p)
    if (!v.ok) bad.push(explain(v.problems, { line: i + 1 }))
  }
  if (!bad.length) process.exit(0)

  const shown = bad.slice(0, MAX_REPORTED)
  const note = [
    `**[memory ledger] ${bad.length} proposal(s) in \`.claude/memory-proposals.jsonl\` will never reach the vault.**`,
    '',
    ...shown.map((b) => `- ${b}`),
    ...(bad.length > shown.length ? [`- …and ${bad.length - shown.length} more`] : []),
    '',
    'The drain dead-letters these rather than writing them, so the finding is lost unless it is',
    'fixed now — while this session still knows what it was. Fix the lines in place, or append',
    'through the helper, which refuses an invalid batch instead of queueing it:',
    '',
    '```sh',
    "node .claude/hooks/memory-propose.mjs <<'JSON'",
    '{"concept":"…","content":"… at least 40 chars, self-contained …","summary":"…","type":"fact"}',
    'JSON',
    '```',
    '',
    'The shape:',
    '',
    '```jsonc',
    CANONICAL_SHAPE,
    '```',
  ].join('\n')

  process.stdout.write(JSON.stringify({
    hookSpecificOutput: { hookEventName: 'PostToolUse', additionalContext: note },
  }))
} catch {
  // Swallow. A guard that fails closed would be worse than one that misses a warning.
}
process.exit(0)

async function readStdin() {
  const chunks = []
  for await (const chunk of process.stdin) chunks.push(chunk)
  return Buffer.concat(chunks).toString('utf8') || '{}'
}
