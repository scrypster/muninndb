// memory-schema.mjs — the one definition of what a memory proposal is.
//
// Three incompatible proposal schemas appeared in the ledger in a single day, against a
// shape documented that same morning in .claude/memory-protocol.md. Prose does not enforce
// a contract. This module does, and every producer and consumer imports it, so there is
// exactly one place the shape is defined:
//
//   memory-propose.mjs  — producer. Rejects an invalid proposal before it reaches the file.
//   ledger-guard.mjs    — PostToolUse hook. Catches a raw append while the session that
//                         made it can still fix it.
//   memory-drain.mjs    — consumer. Dead-letters what can never be written.
//   memory-migrate-ledger.mjs — one-time repair of what was appended before any of this.
//
// The failure mode this is shaped against is not a stray bad line: it is a *contiguous run*
// of them (observed at lines 65–70, 100–106, 116–129, 134–137, 159–170 — each one agent
// invocation getting it wrong for its whole batch). So a producer-side rejection converts a
// batch of permanently-lost proposals into one corrected retry.

import { createHash } from 'node:crypto'

// The vault this repository's findings belong to. This is a producer-side *default*, not a
// consumer-side substitution: memory-propose.mjs fills it in when a caller omits it,
// because routing metadata is not a finding and an agent has no business guessing it.
// The drain never substitutes it — a raw line missing `vault` is a hard validation failure
// there (principle #1: explicit config is never silently substituted).
export const DEFAULT_VAULT = 'muninndb'

export const REQUIRED = ['vault', 'concept', 'content']

// A memory too short to be self-contained is a note to self, not a memory.
export const MIN_CONTENT = 40

// Fields the drain forwards to muninn_remember. Anything else on a proposal is carried in
// the archive but not written — listed here so producers can see what actually lands.
export const WRITTEN_FIELDS = [
  'vault', 'concept', 'content', 'summary', 'type', 'tags', 'entities', 'importance',
]

export const CANONICAL_SHAPE = [
  '{',
  `  "vault":      ${JSON.stringify(DEFAULT_VAULT).padEnd(22)}// required (memory-propose.mjs fills this in)`,
  '  "concept":    "short label",         // required',
  `  "content":    "the fact itself",     // required — self-contained, >= ${MIN_CONTENT} chars`,
  '  "summary":    "one line",            // strongly preferred',
  '  "type":       "fact",                // fact|decision|observation|issue|procedure|constraint',
  '  "tags":       ["..."],',
  '  "entities":   ["..."],',
  '  "importance": 0.8,                   // 0.7+ is protected from capacity pruning',
  '  "source":     "which agent proposed it"',
  '}',
].join('\n')

// Field names that have actually been observed standing in for a required one. Naming them
// lets the error message say "you wrote `title`, it is `concept`" instead of "missing
// concept", which is the difference between a retry that works and one that guesses again.
const OBSERVED_ALIASES = {
  concept: ['title', 'name', 'label', 'headline'],
  content: ['body', 'text', 'fact', 'detail', 'note'],
  summary: ['one_liner', 'tldr'],
}

/**
 * Validate a parsed proposal.
 * @returns {{ok: boolean, problems: string[]}} — problems are permanent by construction:
 *   every one of them is a property of the line itself, not of the world it is written to.
 *   That is what makes dead-lettering them safe, and what distinguishes them from a write
 *   that failed because a daemon was down.
 *
 *   The claim holds only because the caller never passes a line that is still being
 *   written. A raw append larger than the writer's buffer is two write() calls, and the
 *   fragment between them parses as neither valid JSON nor a stable shape — a TRANSIENT
 *   state that this function would rule permanently invalid. memory-ledger.readPrefix() is
 *   what keeps it out: it consumes only up to the last newline, so an unterminated trailing
 *   line is never handed here at all. If you ever read the ledger some other way, apply the
 *   same rule before calling this.
 */
export function validate(p) {
  const problems = []
  if (p === null || typeof p !== 'object' || Array.isArray(p)) {
    return { ok: false, problems: ['not a JSON object'] }
  }
  for (const f of REQUIRED) {
    if (typeof p[f] === 'string' && p[f].trim()) continue
    const alias = (OBSERVED_ALIASES[f] || []).find((a) => typeof p[a] === 'string' && p[a].trim())
    problems.push(alias ? `missing '${f}' — you used '${alias}'; the field is '${f}'` : `missing '${f}'`)
  }
  if (typeof p.content === 'string' && p.content.trim() && p.content.trim().length < MIN_CONTENT) {
    problems.push(
      `'content' is ${p.content.trim().length} chars, minimum ${MIN_CONTENT} — not self-contained (see .claude/memory-protocol.md)`
    )
  }
  if (p.importance !== undefined && (typeof p.importance !== 'number' || p.importance < 0 || p.importance > 1)) {
    problems.push(`'importance' must be a number in [0,1], got ${JSON.stringify(p.importance)}`)
  }
  for (const f of ['tags', 'entities']) {
    if (p[f] !== undefined && !Array.isArray(p[f])) problems.push(`'${f}' must be an array`)
  }
  return { ok: problems.length === 0, problems }
}

/**
 * Best-effort repair of the schema drift that has actually been observed. Used only by the
 * one-time migration and (for `vault` only) by the producer helper — never by the drain.
 * @returns {{proposal: object, repairs: string[]}}
 */
export function repair(p, { defaultVault = DEFAULT_VAULT } = {}) {
  if (p === null || typeof p !== 'object' || Array.isArray(p)) return { proposal: p, repairs: [] }
  const out = { ...p }
  const repairs = []

  for (const [field, aliases] of Object.entries(OBSERVED_ALIASES)) {
    if (typeof out[field] === 'string' && out[field].trim()) continue
    for (const a of aliases) {
      if (typeof out[a] === 'string' && out[a].trim()) {
        out[field] = out[a]
        delete out[a]
        repairs.push(`${a} -> ${field}`)
        break
      }
    }
  }

  if (!(typeof out.vault === 'string' && out.vault.trim())) {
    out.vault = defaultVault
    repairs.push(`vault -> "${defaultVault}"`)
  }

  // `issue` / `refs` carried tracker references on two of the observed drift schemas. They
  // are real provenance; fold them into tags rather than dropping them on the floor.
  const refs = []
  for (const f of ['issue', 'refs', 'ref']) {
    if (out[f] === undefined) continue
    for (const v of Array.isArray(out[f]) ? out[f] : [out[f]]) {
      const s = String(v).trim().replace(/^#/, '')
      if (/^\d+$/.test(s)) refs.push(`issue-${s}`)
      else if (s) refs.push(s.slice(0, 64))
    }
    delete out[f]
    repairs.push(`${f} -> tags`)
  }
  if (refs.length) out.tags = [...new Set([...(Array.isArray(out.tags) ? out.tags : []), ...refs])]

  return { proposal: out, repairs }
}

// The fields that ARE the memory. Everything else on a proposal — including `summary`,
// `type` and `entities`, which the drain does write on a first write — is an annotation on
// this identity, not part of it.
//
// Stated precisely because the earlier comment said "excludes tags/importance" and excluded
// three more fields than it named. The consequence is real and has to be said out loud: a
// re-proposal that corrects a `summary`, `type` or `entities` has the same op_id, so the
// server returns the existing engram and the correction does not land. That is the right
// trade — including them would mint a rival near-duplicate for a one-word summary edit,
// which is the pollution the whole design avoids — but it is only acceptable because the
// drain now REPORTS it (counts.unapplied_annotations, a NOT APPLIED line, and
// `annotations_not_applied` in the archive) instead of logging "already had" and moving on.
// Correcting a live memory's annotations is muninn_evolve's job, not a re-proposal's.
export const IDENTITY_FIELDS = ['vault', 'concept', 'content']

// Content-derived, so the same finding proposed twice is the same op_id and the server's
// idempotency receipt answers "have I already written this?" exactly, in O(1), with no
// embedder and no similarity heuristic.
//
// The fields are JSON-encoded rather than joined by a separator character. The previous
// version used a literal NUL between fields — correct delimiting, but two invisible NUL
// bytes in a source file, which is why `git diff` reported this script as *binary* and
// showed no diff at all for it. A delimiter you cannot see in a review is a bad delimiter in
// a public repo. JSON.stringify escapes any quote or backslash in the values, so it is
// unambiguous the same way NUL was, and it is printable.
//
// Note this changes the op_id of a given proposal versus the pre-#825 formula. That is
// harmless: op_id exists to make a *re-drain* a no-op, and idempotency receipts expire after
// 30 days anyway (engine.go), so it can only matter to a line that is queued across the
// change — of which there are none.
export function opIdFor(p) {
  return 'mp-' + createHash('sha256')
    .update(JSON.stringify(IDENTITY_FIELDS.map((f) => p[f])))
    .digest('hex').slice(0, 24)
}

/** Human-readable, fix-oriented rendering of a validation failure. */
export function explain(problems, { line = null } = {}) {
  const where = line === null ? '' : `line ${line}: `
  return `${where}${problems.join('; ')}`
}
