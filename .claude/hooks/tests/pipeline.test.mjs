// pipeline.test.mjs — the memory pipeline's own tests.
//
// Run: node --test .claude/hooks/tests/
//
// NOT in CI: the CI budget is for the Go gate (docs/internals/drift-and-obligations.md), and
// this tooling is developer-local. It is fast (a few seconds, one loopback HTTP server, no
// daemon) so there is no excuse for not running it before touching the drain.
//
// Every test here exists because the behaviour it pins was measured broken, and each one was
// shown to fail against the pre-fix drain before the fix landed (issue #825).
//
// Fixtures are synthetic. The real ledger's contents are findings about real work and are
// gitignored for that reason; the *shapes* asserted here are copied from the real failures
// (missing `vault`; `title`/`body` for `concept`/`content`), and the validator was
// additionally run against the real 43 out-of-band.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, appendFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawn } from 'node:child_process'
import { createServer } from 'node:http'
import { validate, repair, opIdFor } from '../memory-schema.mjs'

const HOOKS = dirname(dirname(fileURLToPath(import.meta.url)))
const DRAIN = join(HOOKS, 'memory-drain.mjs')
const PROPOSE = join(HOOKS, 'memory-propose.mjs')
const MIGRATE = join(HOOKS, 'memory-migrate-ledger.mjs')

function makeRepo(lines = []) {
  const root = mkdtempSync(join(tmpdir(), 'muninn-ledger-test-'))
  mkdirSync(join(root, '.claude'), { recursive: true })
  const ledger = join(root, '.claude', 'memory-proposals.jsonl')
  writeFileSync(ledger, lines.length ? lines.map((l) => (typeof l === 'string' ? l : JSON.stringify(l))).join('\n') + '\n' : '')
  return { root, ledger }
}

function proposal(n, extra = {}) {
  return {
    vault: 'testvault',
    concept: `synthetic finding ${n}`,
    content: `Synthetic finding number ${n}, written long enough to clear the forty-character self-containment floor.`,
    summary: `synthetic ${n}`,
    type: 'fact',
    ...extra,
  }
}

/** A stand-in MuninnDB MCP endpoint. `onRemember` can mutate the world mid-drain. */
async function fakeMuninn({ onRemember = () => ({}) } = {}) {
  const calls = []
  const srv = createServer((req, res) => {
    let body = ''
    req.on('data', (c) => { body += c })
    req.on('end', () => {
      const env = JSON.parse(body || '{}')
      const name = env?.params?.name
      const args = env?.params?.arguments || {}
      calls.push({ name, args })
      let payload
      if (name === 'muninn_status') payload = { status: 'ok' }
      else if (name === 'muninn_remember') payload = { id: `eng-${calls.length}`, ...onRemember(args, calls.length) }
      else payload = {}
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ jsonrpc: '2.0', id: env.id, result: { content: [{ type: 'text', text: JSON.stringify(payload) }] } }))
    })
  })
  await new Promise((r) => srv.listen(0, '127.0.0.1', r))
  return { base: `http://127.0.0.1:${srv.address().port}/mcp`, calls, close: () => new Promise((r) => srv.close(r)) }
}

function runNode(script, args, { root, env = {} } = {}) {
  return new Promise((resolve) => {
    const p = spawn(process.execPath, [script, ...args], {
      cwd: root,
      env: { ...process.env, CLAUDE_PROJECT_DIR: root, MUNINN_MCP_TOKEN: 'mdb_test', ...env },
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    let out = '', err = ''
    p.stdout.on('data', (c) => { out += c })
    p.stderr.on('data', (c) => { err += c })
    if (env.__stdin !== undefined) p.stdin.write(env.__stdin)
    p.stdin.end()
    p.on('close', (code) => resolve({ code, out, err }))
  })
}

function lines(path) {
  if (!existsSync(path)) return []
  return readFileSync(path, 'utf8').split('\n').filter((l) => l.trim())
}

// ── D1: a proposal appended DURING a drain must survive it ───────────────────────────────
//
// RED against the pre-fix drain: it read the ledger once and rewrote it from an in-memory
// array at the end, so the concurrently-appended line was erased — never written, not
// retained, gone. Verbatim failure recorded in the PR.
test('D1: a proposal appended during the drain is preserved, not erased', async (t) => {
  const { root, ledger } = makeRepo([proposal(1), proposal(2)])
  const late = proposal(99, { concept: 'appended mid-drain' })
  let appended = false
  const srv = await fakeMuninn({
    onRemember: () => {
      if (!appended) { appended = true; appendFileSync(ledger, JSON.stringify(late) + '\n') }
      return {}
    },
  })
  t.after(() => srv.close())

  const r = await runNode(DRAIN, ['--base', srv.base], { root })
  assert.equal(r.code, 0, r.out + r.err)

  const remaining = lines(ledger).map((l) => JSON.parse(l))
  assert.equal(remaining.length, 1, `expected the mid-drain append to survive; ledger now: ${remaining.length} line(s)`)
  assert.equal(remaining[0].concept, 'appended mid-drain')

  // …and it is still a pending proposal, not a silently-dropped one: the next drain writes it.
  const r2 = await runNode(DRAIN, ['--base', srv.base], { root })
  assert.equal(r2.code, 0, r2.out + r2.err)
  assert.equal(lines(ledger).length, 0)
  assert.ok(srv.calls.some((c) => c.name === 'muninn_remember' && c.args.concept === 'appended mid-drain'))
})

test('D1: the drain refuses to rewrite a ledger whose consumed prefix changed under it', async (t) => {
  const { root, ledger } = makeRepo([proposal(1), proposal(2)])
  const srv = await fakeMuninn({
    onRemember: () => { writeFileSync(ledger, JSON.stringify(proposal(42)) + '\n'); return {} },
  })
  t.after(() => srv.close())

  const r = await runNode(DRAIN, ['--base', srv.base], { root })
  const receipt = JSON.parse(readFileSync(join(root, '.claude', 'memory-drain-receipt.json'), 'utf8'))
  assert.equal(receipt.outcome, 'rewrite-refused', r.out + r.err)
  // The rewritten content is intact — the drain did not overwrite someone else's file.
  assert.equal(lines(ledger).length, 1)
  assert.equal(JSON.parse(lines(ledger)[0]).concept, proposal(42).concept)
})

// ── P1: liveness is observable, and the three "nothing happened" states are distinct ──────
//
// RED against the pre-fix drain: it appended to the archive only on a successful write, so
// "never invoked", "invoked with an empty ledger" and "invoked, everything failed" were the
// identical filesystem state — no file. There was no receipt to assert on at all.
test('P1: never-invoked, empty-ledger and all-invalid produce three distinguishable receipts', async (t) => {
  const srv = await fakeMuninn()
  t.after(() => srv.close())

  // (a) never invoked
  const a = makeRepo([proposal(1)])
  const receiptPath = (root) => join(root, '.claude', 'memory-drain-receipt.json')
  assert.equal(existsSync(receiptPath(a.root)), false, 'a repo that never ran the drain must have no receipt')

  // (b) invoked, ledger empty
  const b = makeRepo([])
  await runNode(DRAIN, ['--base', srv.base], { root: b.root })
  const rb = JSON.parse(readFileSync(receiptPath(b.root), 'utf8'))
  assert.equal(rb.outcome, 'empty')
  assert.equal(rb.considered, 0)

  // (c) invoked, every proposal permanently invalid
  const c = makeRepo([{ title: 'no vault, no concept', body: 'short' }, { concept: 'x' }])
  await runNode(DRAIN, ['--base', srv.base], { root: c.root })
  const rc = JSON.parse(readFileSync(receiptPath(c.root), 'utf8'))
  assert.equal(rc.outcome, 'ok')
  assert.equal(rc.considered, 2)
  assert.equal(rc.counts.dead_lettered, 2)
  assert.equal(rc.counts.written, 0)

  // (d) invoked, daemon unreachable — distinct again, and the ledger is untouched
  const d = makeRepo([proposal(1)])
  await runNode(DRAIN, ['--base', 'http://127.0.0.1:1/mcp'], { root: d.root })
  const rd = JSON.parse(readFileSync(receiptPath(d.root), 'utf8'))
  assert.equal(rd.outcome, 'unreachable')
  assert.equal(rd.counts.retained, 1)
  assert.equal(lines(d.ledger).length, 1)

  const outcomes = new Set([rb.outcome, rc.outcome, rd.outcome])
  assert.equal(outcomes.size, 3, `three invocations must be three distinguishable states, got ${[...outcomes]}`)
})

test('P1: a receipt is written even when the run is a debounced no-op', async (t) => {
  const srv = await fakeMuninn()
  t.after(() => srv.close())
  const { root } = makeRepo([proposal(1)])
  await runNode(DRAIN, ['--base', srv.base], { root })
  const first = JSON.parse(readFileSync(join(root, '.claude', 'memory-drain-receipt.json'), 'utf8'))

  await runNode(DRAIN, ['--base', srv.base, '--debounce', '60', '--trigger', 'Stop'], { root })
  const second = JSON.parse(readFileSync(join(root, '.claude', 'memory-drain-receipt.json'), 'utf8'))
  assert.equal(second.outcome, 'debounced')
  assert.equal(second.trigger, 'Stop')
  // The debounce clock does not advance on a debounced run, or a stream of them would
  // postpone the next real run forever.
  assert.equal(second.last_run_at, first.last_run_at)
  assert.equal(lines(join(root, '.claude', 'memory-drain-receipts.jsonl')).length, 2)
})

// ── D2: the ledger can reach empty, and the drain can exit 0 ──────────────────────────────
//
// RED against the pre-fix drain: `return failed.length ? 1 : 0` with permanently-invalid
// lines kept in the ledger meant exit 1 on every run, forever.
test('D2: permanently-invalid lines dead-letter with their reason and stop blocking the queue', async (t) => {
  const srv = await fakeMuninn()
  t.after(() => srv.close())
  const { root, ledger } = makeRepo([
    { concept: 'no vault here', content: 'A finding with everything except the vault field, long enough to be valid otherwise.' },
    { type: 'fact', title: 'title/body drift', body: 'The producer used title and body instead of concept and content, as observed.' },
    proposal(1),
  ])
  const r = await runNode(DRAIN, ['--base', srv.base], { root })
  assert.equal(r.code, 0, `dead-lettered lines are resolved, not failures:\n${r.out}${r.err}`)
  assert.equal(lines(ledger).length, 0, 'the ledger must be able to reach empty')

  const dl = lines(join(root, '.claude', 'memory-proposals.deadletter.jsonl')).map((l) => JSON.parse(l))
  assert.equal(dl.length, 2)
  assert.match(dl[0].reason, /missing 'vault'/)
  assert.match(dl[1].reason, /you used 'title'/)
  assert.match(dl[1].reason, /you used 'body'/)
  // Dead-lettered is out of the queue, not deleted.
  assert.equal(JSON.parse(dl[0].proposal).concept, 'no vault here')

  // Re-running is clean and quiet.
  const r2 = await runNode(DRAIN, ['--base', srv.base], { root })
  assert.equal(r2.code, 0)
  assert.equal(JSON.parse(readFileSync(join(root, '.claude', 'memory-drain-receipt.json'), 'utf8')).outcome, 'empty')
})

test('D2: a write that fails transiently keeps its line — it is not dead-lettered', async (t) => {
  const { root, ledger } = makeRepo([proposal(1)])
  const srv = await fakeMuninn()
  t.after(() => srv.close())
  const r = await runNode(DRAIN, ['--base', srv.base], { root, env: { MUNINN_MCP_TOKEN: '', MUNINN_TOKEN: '' } })
  assert.equal(r.code, 1, 'a manual run says so out loud when it could not write')
  assert.equal(lines(ledger).length, 1, 'a failed write never consumes its line')
  assert.equal(lines(join(root, '.claude', 'memory-proposals.deadletter.jsonl')).length, 0)
  const rc = JSON.parse(readFileSync(join(root, '.claude', 'memory-drain-receipt.json'), 'utf8'))
  assert.equal(rc.counts.failed, 1)
  assert.equal(rc.counts.dead_lettered, 0)
})

test('the --hook form never reports failure, but the receipt still does', async (t) => {
  const { root } = makeRepo([proposal(1)])
  const r = await runNode(DRAIN, ['--base', 'http://127.0.0.1:1/mcp', '--hook', '--trigger', 'SessionEnd'], { root })
  assert.equal(r.code, 0, 'a hook that cries wolf at session close is a hook people disable')
  const rc = JSON.parse(readFileSync(join(root, '.claude', 'memory-drain-receipt.json'), 'utf8'))
  assert.equal(rc.outcome, 'unreachable')
  assert.equal(rc.trigger, 'SessionEnd')
})

// ── D3/D6/D7/D8: the pipe does not recall, and identity is exact ──────────────────────────
test('the drain performs no recall — identity is answered by op_id, not by a relevance band', async (t) => {
  const { root } = makeRepo([proposal(1), proposal(2)])
  const srv = await fakeMuninn()
  t.after(() => srv.close())
  await runNode(DRAIN, ['--base', srv.base], { root })
  assert.equal(srv.calls.filter((c) => c.name === 'muninn_recall').length, 0,
    'a relevance band measures ranking under a query, not whether a record committed')
  const remembers = srv.calls.filter((c) => c.name === 'muninn_remember')
  assert.equal(remembers.length, 2)
  for (const c of remembers) assert.match(c.args.op_id, /^mp-[0-9a-f]{24}$/)
})

test("a server-side idempotency hit is counted as 'already present', not as a new write", async (t) => {
  const { root, ledger } = makeRepo([proposal(1)])
  const srv = await fakeMuninn({ onRemember: () => ({ id: 'eng-existing', idempotent: true }) })
  t.after(() => srv.close())
  await runNode(DRAIN, ['--base', srv.base], { root })
  const rc = JSON.parse(readFileSync(join(root, '.claude', 'memory-drain-receipt.json'), 'utf8'))
  assert.equal(rc.counts.idempotent, 1)
  assert.equal(rc.counts.written, 0)
  assert.equal(lines(ledger).length, 0)
  assert.equal(JSON.parse(lines(join(root, '.claude', 'memory-proposals.drained.jsonl'))[0]).idempotent, true)
})

test('op_id is content-derived and stable across tag/importance refinement', () => {
  const a = proposal(7)
  const b = { ...a, tags: ['x'], importance: 0.9, source: 'someone-else' }
  assert.equal(opIdFor(a), opIdFor(b))
  assert.notEqual(opIdFor(a), opIdFor(proposal(8)))
})

// ── D4: the schema is enforced at the producer ────────────────────────────────────────────
test('D4: the validator rejects every shape the real ledger actually drifted into', () => {
  // Copied from the observed contiguous runs. Content is synthetic; the shapes are not.
  const observed = [
    [{ concept: 'c', content: 'x'.repeat(60), type: 'fact', entities: [], issue: 800 }, /missing 'vault'/],
    [{ type: 'fact', title: 't', body: 'x'.repeat(60), tags: [], date: '2026-08-02' }, /you used 'title'/],
    [{ type: 'fact', title: 't', body: 'x'.repeat(60), tags: [], refs: ['#1'] }, /you used 'body'/],
    [{ vault: 'v', concept: 'c', content: 'too short' }, /minimum 40/],
    [{ vault: 'v', concept: 'c', content: 'x'.repeat(60), importance: 5 }, /importance/],
    [{ vault: 'v', concept: 'c', content: 'x'.repeat(60), tags: 'not-an-array' }, /'tags' must be an array/],
  ]
  for (const [p, re] of observed) {
    const v = validate(p)
    assert.equal(v.ok, false, `expected rejection for ${JSON.stringify(p).slice(0, 60)}`)
    assert.match(v.problems.join('; '), re)
  }
  assert.equal(validate(proposal(1)).ok, true)
})

test('D4: memory-propose rejects a batch atomically — a bad record appends nothing', async () => {
  const { root, ledger } = makeRepo([])
  const batch = [proposal(1), { concept: 'bad', content: 'short' }].map((r) => JSON.stringify(r)).join('\n')
  const r = await runNode(PROPOSE, [], { root, env: { __stdin: batch } })
  assert.equal(r.code, 1)
  assert.match(r.err, /record 2: 'content' is 5 chars/)
  assert.equal(lines(ledger).length, 0, 'a batch must not land half-good')
})

test('D4: memory-propose fills in the vault, so the largest observed failure class cannot recur', async () => {
  const { root, ledger } = makeRepo([])
  const rec = proposal(1)
  delete rec.vault
  const r = await runNode(PROPOSE, ['--vault', 'testvault'], { root, env: { __stdin: JSON.stringify(rec) } })
  assert.equal(r.code, 0, r.err)
  assert.equal(JSON.parse(lines(ledger)[0]).vault, 'testvault')
})

test('D4: memory-propose accepts pretty-printed JSON, an array, and JSONL', async () => {
  for (const stdin of [
    JSON.stringify(proposal(1), null, 2),
    JSON.stringify([proposal(1), proposal(2)]),
    [JSON.stringify(proposal(3)), JSON.stringify(proposal(4))].join('\n'),
  ]) {
    const { root, ledger } = makeRepo([])
    const r = await runNode(PROPOSE, [], { root, env: { __stdin: stdin } })
    assert.equal(r.code, 0, r.err)
    assert.ok(lines(ledger).length >= 1)
  }
})

// ── The migration ─────────────────────────────────────────────────────────────────────────
test('migration repairs the observed drift and leaves the genuinely broken for dead-lettering', async () => {
  const { root, ledger } = makeRepo([
    { concept: 'missing vault only', content: 'x'.repeat(60), type: 'fact', issue: 825 },
    { type: 'fact', title: 'title/body', body: 'y'.repeat(60), tags: ['t'] },
    { vault: 'v', concept: 'ok already', content: 'z'.repeat(60) },
    { vault: 'v', concept: 'unfixable', content: 'nope' },
  ])
  const r = await runNode(MIGRATE, ['--vault', 'testvault'], { root })
  assert.equal(r.code, 0, r.err)
  const after = lines(ledger).map((l) => JSON.parse(l))
  assert.equal(after.length, 4, 'migration never drops a line')
  assert.equal(after[0].vault, 'testvault')
  assert.deepEqual(after[0].tags, ['issue-825'], 'tracker provenance is folded into tags, not dropped')
  assert.equal(after[1].concept, 'title/body')
  assert.equal(after[1].content, 'y'.repeat(60))
  assert.equal(after[1].vault, 'testvault')
  assert.equal(after[3].content, 'nope', 'an unrepairable line is left verbatim for the drain to dead-letter')
  assert.match(r.out, /1 already valid/)
  assert.match(r.out, /2 repaired/)
  assert.match(r.out, /1 still invalid/)
})

test('repair is idempotent — a migrated ledger needs no second pass', () => {
  const once = repair({ title: 't', body: 'x'.repeat(60) }).proposal
  const twice = repair(once)
  assert.equal(twice.repairs.length, 0)
})

// ── The batch cap ─────────────────────────────────────────────────────────────────────────
test('--max caps a run and leaves the remainder queued for the next one', async (t) => {
  const { root, ledger } = makeRepo([proposal(1), proposal(2), proposal(3)])
  const srv = await fakeMuninn()
  t.after(() => srv.close())
  await runNode(DRAIN, ['--base', srv.base, '--max', '2'], { root })
  assert.equal(lines(ledger).length, 1)
  const rc = JSON.parse(readFileSync(join(root, '.claude', 'memory-drain-receipt.json'), 'utf8'))
  assert.equal(rc.counts.written, 2)
  assert.equal(rc.counts.retained, 1)
})

test('op_id delimiting is unambiguous — a shifted field boundary is a different memory', () => {
  const a = { vault: 'v', concept: 'alpha beta', content: 'gamma' }
  const b = { vault: 'v', concept: 'alpha', content: 'beta gamma' }
  assert.notEqual(opIdFor(a), opIdFor(b), 'a separator that can appear inside a field is not a separator')
})

test('no committed hook script contains a NUL byte', async () => {
  const { readdirSync } = await import('node:fs')
  for (const f of readdirSync(HOOKS).filter((n) => n.endsWith('.mjs'))) {
    const buf = readFileSync(join(HOOKS, f))
    assert.equal(buf.includes(0), false, `${f} contains a NUL byte — git will treat it as binary and show no diff`)
  }
})
