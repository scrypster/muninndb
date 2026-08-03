#!/usr/bin/env node
// memory-freshness.mjs — the receipt's reader. SessionStart.
//
// The receipt was write-only. Every invocation of the drain wrote one and NOTHING anywhere
// read it back: a grep across the tree found the drain's own debounce, .gitignore, and
// prose. So the claim "`stat .claude/memory-drain-receipt.json` answers it now" was worse
// than silence — a seconds-old receipt reading `debounced` while findings rot LOOKS like an
// answer, and nobody stats a file they have no reason to suspect.
//
// This closes the loop at the one moment a session can act on it. It reports only when
// there is something wrong:
//
//   - proposals queued that the last run did not consume;
//   - a last outcome that is not a healthy one (unreachable, locked, error, interrupted,
//     rewrite-refused, partial);
//   - a ledger with proposals and no receipt at all — the mechanism has never run here.
//
// Shaped exactly like ledger-guard.mjs and drift-guard.mjs (principle #7): never block,
// never throw, always exit 0, speak through additionalContext. A guard that can fail is a
// guard that gets removed.

import { readFileSync, statSync } from 'node:fs'
import { paths, readPrefix } from './memory-ledger.mjs'

// Outcomes that mean the pipe is working. Anything else is worth a sentence at session
// start, because every one of them is a state that persists silently until someone looks.
const HEALTHY = new Set(['ok', 'empty', 'no-ledger', 'debounced'])

try {
  const input = JSON.parse(await readStdin())
  const P = paths(process.env.CLAUDE_PROJECT_DIR || input.cwd || process.cwd())

  let queued = 0
  try { queued = readPrefix(P.ledger).lines.length } catch { /* unreadable — say nothing */ }

  let receipt = null
  try { receipt = JSON.parse(readFileSync(P.receipt, 'utf8')) } catch { /* never run here */ }

  const notes = []
  if (!receipt && queued) {
    notes.push(`${queued} memory proposal(s) are queued and the drain has never run in this checkout.`)
  } else if (receipt) {
    const ageMin = Math.round((Date.now() - Date.parse(receipt.at || 0)) / 60_000)
    if (!HEALTHY.has(receipt.outcome)) {
      notes.push(`the last drain (${ageMin} min ago, trigger \`${receipt.trigger}\`) ended \`${receipt.outcome}\`${receipt.why ? ` — ${receipt.why}` : ''}.`)
    }
    if (queued) {
      notes.push(`${queued} proposal(s) are still queued in \`${P.ledger}\`.`)
    }
    if (receipt.counts?.unapplied_annotations) {
      notes.push(`${receipt.counts.unapplied_annotations} proposal(s) matched an existing engram, so their summary/type/entities were not applied — muninn_evolve is how those land.`)
    }
  }
  if (!notes.length) process.exit(0)

  const note = [
    '**[memory ledger] the drain is not current.**',
    '',
    ...notes.map((n) => `- ${n}`),
    '',
    'Findings only survive the session once they are in the vault. To drain now:',
    '',
    '```sh',
    'node .claude/hooks/memory-drain.mjs',
    '```',
    '',
    `Full state: \`${P.receipt}\`. If the daemon is down, the queue is intact and the next run picks it up.`,
  ].join('\n')

  process.stdout.write(JSON.stringify({
    hookSpecificOutput: { hookEventName: 'SessionStart', additionalContext: note },
  }))
} catch {
  // Swallow. Reporting a stale queue is worth something; failing a session start is not.
}
process.exit(0)

async function readStdin() {
  const chunks = []
  for await (const chunk of process.stdin) chunks.push(chunk)
  return Buffer.concat(chunks).toString('utf8') || '{}'
}
