// memory-ledger.mjs — the paths, the lock, and the only two safe ways to touch the ledger.
//
// The ledger is an append-only JSONL queue written by many agents and consumed by one
// drain. The defect this module exists to make unrepresentable: the drain used to read the
// whole file, spend minutes working, then rewrite it from an in-memory array — so every
// proposal appended in between was erased. The loss window was the entire run, and the
// blast radius was precisely the findings the mechanism exists to protect.
//
// Two independent mechanisms close it, because each one alone has a residual:
//
//   1. A lock file. Serialises drain against any producer that goes through
//      memory-propose.mjs. Residual: a raw `echo >>` append does not take the lock.
//   2. Prefix-range consumption. The drain records the byte length it read and, at rewrite
//      time, re-reads the file, verifies the prefix is byte-identical to what it consumed,
//      and splices [retained prefix lines] + [everything appended after the offset]. A raw
//      append lands in the tail and survives. If the prefix changed under it, the rewrite
//      is refused outright and the ledger is left alone — nothing is lost, worst case a
//      proposal is written twice, and op_id makes the second write a no-op.
//
// Residual, stated rather than hidden: an O_APPEND write that lands between the tail
// re-read and the rename is still lost. That window is the duration of one readFileSync
// plus one rename — sub-millisecond, versus the whole-run window it replaces — and it is
// zero for any producer that takes the lock.

import { existsSync, mkdirSync, openSync, closeSync, fstatSync, readSync, readFileSync, writeFileSync, renameSync, unlinkSync, statSync, appendFileSync } from 'node:fs'
import { join, dirname } from 'node:path'

export function paths(root = process.env.CLAUDE_PROJECT_DIR || process.cwd()) {
  const d = join(root, '.claude')
  return {
    root,
    dir: d,
    ledger: join(d, 'memory-proposals.jsonl'),
    archive: join(d, 'memory-proposals.drained.jsonl'),
    deadLetter: join(d, 'memory-proposals.deadletter.jsonl'),
    receipt: join(d, 'memory-drain-receipt.json'),
    receiptLog: join(d, 'memory-drain-receipts.jsonl'),
    lock: join(d, 'memory-proposals.lock'),
  }
}

const STALE_LOCK_MS = 10 * 60 * 1000

/**
 * Acquire the ledger lock. Non-blocking by default: a drain that cannot get the lock is a
 * drain that has nothing useful to do right now, and blocking a session-close hook on
 * another process is worse than skipping a run the next trigger will repeat.
 * @returns {{ok: boolean, why?: string, release: () => void}}
 */
export function acquireLock(lockPath, { waitMs = 0 } = {}) {
  mkdirSync(dirname(lockPath), { recursive: true })
  const deadline = Date.now() + waitMs
  for (;;) {
    try {
      const fd = openSync(lockPath, 'wx')
      writeFileSync(fd, JSON.stringify({ pid: process.pid, at: new Date().toISOString() }))
      closeSync(fd)
      return { ok: true, release: () => { try { unlinkSync(lockPath) } catch { /* already gone */ } } }
    } catch (e) {
      if (e?.code !== 'EEXIST') return { ok: false, why: `lock error: ${e?.message || e}`, release: () => {} }
      // A crashed holder must not wedge the queue forever.
      let stale = false
      try { stale = Date.now() - statSync(lockPath).mtimeMs > STALE_LOCK_MS } catch { stale = true }
      if (stale) { try { unlinkSync(lockPath) } catch { /* raced */ } continue }
      if (Date.now() >= deadline) return { ok: false, why: 'ledger is locked by another process', release: () => {} }
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 25)
    }
  }
}

/**
 * Read the ledger and pin the byte offset that was read. Everything the caller may consume
 * lives in [0, bytes); everything appended later lives beyond it and is not the caller's to
 * touch.
 * @returns {{exists: boolean, bytes: number, prefix: Buffer, lines: {no: number, raw: string}[]}}
 */
export function readPrefix(ledgerPath) {
  if (!existsSync(ledgerPath)) return { exists: false, bytes: 0, prefix: Buffer.alloc(0), lines: [] }
  const fd = openSync(ledgerPath, 'r')
  try {
    const size = fstatSync(fd).size
    const buf = Buffer.alloc(size)
    let off = 0
    while (off < size) {
      const n = readSync(fd, buf, off, size - off, off)
      if (n <= 0) break
      off += n
    }
    const lines = []
    buf.toString('utf8').split('\n').forEach((raw, i) => {
      if (raw.trim()) lines.push({ no: i + 1, raw: raw.trim() })
    })
    return { exists: true, bytes: size, prefix: buf, lines }
  } finally {
    closeSync(fd)
  }
}

/**
 * Replace the consumed prefix with `retained`, preserving verbatim anything appended after
 * `bytes`.
 * @returns {{ok: boolean, why?: string, appendedDuringRun: number}}
 */
export function spliceConsumed(ledgerPath, { bytes, prefix, retained }) {
  let current
  try { current = readFileSync(ledgerPath) } catch (e) {
    return { ok: false, why: `cannot re-read ledger: ${e?.message || e}`, appendedDuringRun: 0 }
  }
  if (current.length < bytes || !current.subarray(0, bytes).equals(prefix)) {
    // Someone rewrote the region we consumed. Refuse: leaving the ledger alone costs at
    // most a duplicate write attempt (which op_id absorbs); guessing costs data.
    return {
      ok: false,
      why: 'ledger prefix changed under the drain — refusing to rewrite (nothing lost; op_id makes a re-drain a no-op)',
      appendedDuringRun: Math.max(0, current.length - bytes),
    }
  }
  const tail = current.subarray(bytes)
  const head = retained.length ? Buffer.from(retained.join('\n') + '\n', 'utf8') : Buffer.alloc(0)
  const tmp = `${ledgerPath}.tmp`
  writeFileSync(tmp, Buffer.concat([head, tail]))
  renameSync(tmp, ledgerPath)
  return { ok: true, appendedDuringRun: tail.length }
}

/** Append complete JSONL records in one write. */
export function appendRecords(path, records) {
  if (!records.length) return
  mkdirSync(dirname(path), { recursive: true })
  appendFileSync(path, records.map((r) => (typeof r === 'string' ? r : JSON.stringify(r))).join('\n') + '\n')
}

const RECEIPT_LOG_MAX = 2000

/**
 * Write a receipt. This happens on EVERY invocation — no-op, debounced, failed, or
 * successful — because the defect that cost three sessions was that "never invoked",
 * "invoked, ledger empty" and "invoked, everything held" were the identical filesystem
 * state: no file at all. Liveness has to be observable from outside the process.
 */
export function writeReceipt(p, receipt) {
  mkdirSync(p.dir, { recursive: true })
  const json = JSON.stringify(receipt, null, 2)
  const tmp = `${p.receipt}.tmp`
  writeFileSync(tmp, json + '\n')
  renameSync(tmp, p.receipt)

  let log = []
  try { log = readFileSync(p.receiptLog, 'utf8').split('\n').filter((l) => l.trim()) } catch { /* first run */ }
  log.push(JSON.stringify(receipt))
  if (log.length > RECEIPT_LOG_MAX) log = log.slice(log.length - RECEIPT_LOG_MAX)
  const ltmp = `${p.receiptLog}.tmp`
  writeFileSync(ltmp, log.join('\n') + '\n')
  renameSync(ltmp, p.receiptLog)
}

/** The previous receipt, or null if this mechanism has never run here. */
export function readReceipt(p) {
  try { return JSON.parse(readFileSync(p.receipt, 'utf8')) } catch { return null }
}
