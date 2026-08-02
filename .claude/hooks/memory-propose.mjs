#!/usr/bin/env node
// memory-propose.mjs — the only way to put a proposal in the ledger that cannot be wrong.
//
// 43 of the first 179 proposals were permanently invalid: 31 omitted `vault`, 12 used
// `title`/`body` instead of `concept`/`content`. Three incompatible schemas in one day,
// against a shape documented that same morning. The failures were contiguous runs — one
// agent invocation getting it wrong for its entire batch — which is what makes producer-side
// rejection worth so much more than consumer-side reporting: it converts a lost batch into
// one corrected retry, in the session that still knows what the finding was.
//
// This is principle #3 applied to our own tooling: make the bad state unrepresentable rather
// than policy-checked. Nothing is appended unless every record validates, so a batch cannot
// land half-good.
//
// Usage:
//   node .claude/hooks/memory-propose.mjs <<'JSON'
//   {"concept":"…","content":"…","summary":"…","type":"fact","tags":["…"],"source":"…"}
//   JSON
//
//   Accepts a single object, a JSON array of objects, or JSONL — whichever is easier to
//   emit. `vault` is filled in with the repo default when omitted. `--vault NAME` overrides.
//   `--check` validates and prints the verdict without appending.
//
// Exit 0 = every record appended. Exit 1 = nothing appended, and the reason names the field.

import { readFileSync } from 'node:fs'
import { DEFAULT_VAULT, validate, explain, CANONICAL_SHAPE } from './memory-schema.mjs'
import { paths, acquireLock, appendRecords } from './memory-ledger.mjs'

const P = paths()
const CHECK = process.argv.includes('--check')
const VAULT = argOf('--vault') || process.env.MUNINN_PROPOSAL_VAULT || DEFAULT_VAULT

function argOf(flag) {
  const i = process.argv.indexOf(flag)
  return i !== -1 ? process.argv[i + 1] : null
}

function parseInput(text) {
  const t = text.trim()
  if (!t) return { records: [], error: 'no input on stdin' }
  // A whole-document parse first: a single object or an array is the common case, and it
  // tolerates the pretty-printed multi-line JSON an agent naturally emits.
  try {
    const v = JSON.parse(t)
    return { records: Array.isArray(v) ? v : [v] }
  } catch { /* fall through to JSONL */ }
  const records = []
  for (const [i, line] of t.split('\n').entries()) {
    if (!line.trim()) continue
    try { records.push(JSON.parse(line)) } catch (e) {
      return { records: [], error: `line ${i + 1} is not valid JSON (${e.message}). Send one object, a JSON array, or one object per line.` }
    }
  }
  return { records }
}

const raw = readFileSync(0, 'utf8')
const { records, error } = parseInput(raw)
if (error) {
  console.error(`memory-propose: ${error}`)
  console.error(`\nThe shape:\n${CANONICAL_SHAPE}`)
  process.exit(1)
}

const problems = []
const prepared = []
for (const [i, r] of records.entries()) {
  const p = (r && typeof r === 'object' && !Array.isArray(r)) ? { ...r } : r
  // `vault` is routing metadata, not a finding. Filling it in here is an explicit
  // repo-level default at the producer, not a silent substitution at the consumer — the
  // drain still hard-fails a line that lacks it.
  if (p && typeof p === 'object' && !Array.isArray(p) && !(typeof p.vault === 'string' && p.vault.trim())) p.vault = VAULT
  const v = validate(p)
  if (!v.ok) problems.push(`record ${i + 1}: ${explain(v.problems)}`)
  else prepared.push(p)
}

if (problems.length) {
  console.error(`memory-propose: ${problems.length} of ${records.length} record(s) rejected — NOTHING was appended.`)
  for (const p of problems) console.error(`  ${p}`)
  console.error(`\nThe shape:\n${CANONICAL_SHAPE}`)
  console.error('\nFix and re-send the whole batch. The bar for what qualifies is in .claude/memory-protocol.md.')
  process.exit(1)
}

if (CHECK) {
  console.log(`memory-propose: ${prepared.length} record(s) valid (--check, nothing appended)`)
  process.exit(0)
}

const lock = acquireLock(P.lock, { waitMs: 5000 })
if (!lock.ok) {
  console.error(`memory-propose: ${lock.why} — nothing appended, retry in a moment.`)
  process.exit(1)
}
try {
  appendRecords(P.ledger, prepared)
} finally {
  lock.release()
}
console.log(`memory-propose: appended ${prepared.length} proposal(s) to ${P.ledger}`)
