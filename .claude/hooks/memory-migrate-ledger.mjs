#!/usr/bin/env node
// memory-migrate-ledger.mjs — one-time repair of proposals written before the schema was
// enforced anywhere.
//
// These are real findings — measured numbers, dead code paths, honest negatives — and most
// of them are one mechanical rename away from valid. Dead-lettering all of them because the
// producer used `title` instead of `concept` would throw away the thing the whole mechanism
// exists to keep.
//
// It repairs only drift that was actually observed in the file (see OBSERVED_ALIASES in
// memory-schema.mjs), never guesses at content, and anything still invalid after repair is
// left for the drain to dead-letter with its reason. Run it once; it is idempotent, since a
// repaired line needs no repair.
//
// Usage: node .claude/hooks/memory-migrate-ledger.mjs [--dry-run] [--vault NAME]

import { DEFAULT_VAULT, validate, repair, explain } from './memory-schema.mjs'
import { paths, acquireLock, readPrefix, spliceConsumed } from './memory-ledger.mjs'

const P = paths()
const DRY = process.argv.includes('--dry-run')
const VAULT = argOf('--vault') || process.env.MUNINN_PROPOSAL_VAULT || DEFAULT_VAULT

function argOf(flag) {
  const i = process.argv.indexOf(flag)
  return i !== -1 ? process.argv[i + 1] : null
}

const lock = acquireLock(P.lock, { waitMs: 5000 })
if (!lock.ok) {
  console.error(`memory-migrate: ${lock.why} — nothing changed.`)
  process.exit(1)
}

try {
  const snap = readPrefix(P.ledger)
  if (!snap.exists || !snap.lines.length) {
    console.log('memory-migrate: nothing to migrate')
    process.exit(0)
  }

  const out = []
  let alreadyValid = 0
  const repaired = []
  const unrepairable = []

  for (const { no, raw } of snap.lines) {
    let p
    try { p = JSON.parse(raw) } catch {
      unrepairable.push({ no, why: 'unparseable JSON' })
      out.push(raw)
      continue
    }
    if (validate(p).ok) { alreadyValid++; out.push(raw); continue }

    const { proposal, repairs } = repair(p, { defaultVault: VAULT })
    const v = validate(proposal)
    if (v.ok) {
      repaired.push({ no, repairs })
      out.push(JSON.stringify(proposal))
    } else {
      unrepairable.push({ no, why: explain(v.problems), attempted: repairs })
      out.push(raw)
    }
  }

  console.log(
    `memory-migrate: ${snap.lines.length} line(s) — ${alreadyValid} already valid, ` +
    `${repaired.length} repaired, ${unrepairable.length} still invalid${DRY ? ' (dry run — nothing changed)' : ''}`
  )
  const byRepair = new Map()
  for (const r of repaired) {
    const k = r.repairs.join(' + ')
    byRepair.set(k, (byRepair.get(k) || 0) + 1)
  }
  for (const [k, n] of [...byRepair].sort((a, b) => b[1] - a[1])) console.log(`  repaired  ${String(n).padStart(3)}  ${k}`)
  for (const u of unrepairable) console.log(`  STILL INVALID line ${u.no}: ${u.why}${u.attempted?.length ? ` (tried: ${u.attempted.join(', ')})` : ''}`)
  if (unrepairable.length) {
    console.log('\nThese stay in the ledger; the drain will dead-letter them with the reason so they are')
    console.log(`recoverable from ${P.deadLetter} rather than deleted.`)
  }

  if (!DRY && repaired.length) {
    const spl = spliceConsumed(P.ledger, { bytes: snap.bytes, prefix: snap.prefix, retained: out })
    if (!spl.ok) { console.error(`memory-migrate: ${spl.why}`); process.exit(1) }
    if (spl.appendedDuringRun) console.log(`memory-migrate: ${spl.appendedDuringRun} byte(s) appended during the run were preserved`)
  }
} finally {
  lock.release()
}
