// pipeline.test.mjs — the memory pipeline's own tests.
//
// Run: node --test .claude/hooks/tests/*.test.mjs      <- the glob, expanded by the shell
//
// NOT `node --test .claude/hooks/tests/` — the runner's directory walker skips dot-
// directories, so the directory form finds nothing and exits 1 with MODULE_NOT_FOUND in
// ~30 ms. An implausibly fast non-run is easy to read as green (Node v22.21.1).
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
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, appendFileSync, rmSync, utimesSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawn } from 'node:child_process'
import { createServer } from 'node:http'
import { validate, repair, opIdFor } from '../memory-schema.mjs'
import { canonicalRoot } from '../memory-ledger.mjs'

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

/**
 * A stand-in MuninnDB MCP endpoint. `onRemember` can mutate the world mid-drain.
 *   hang: 'muninn_status' | 'muninn_remember' — accept the request and never answer it,
 *         which is the failure a connection-refused test cannot reach.
 *   rememberStatus: an HTTP status for muninn_remember (500 = the server-error branch).
 *   onArrive(name): called the moment a request is fully received, before any response.
 */
async function fakeMuninn({ onRemember = () => ({}), hang = null, rememberStatus = 200, onArrive = null } = {}) {
  const calls = []
  const srv = createServer((req, res) => {
    let body = ''
    req.on('data', (c) => { body += c })
    req.on('end', () => {
      const env = JSON.parse(body || '{}')
      const name = env?.params?.name
      const args = env?.params?.arguments || {}
      calls.push({ name, args })
      if (onArrive) onArrive(name, args)
      if (hang === name) return                       // headers never sent, body never ends
      if (name === 'muninn_remember' && rememberStatus !== 200) {
        res.writeHead(rememberStatus, { 'Content-Type': 'text/plain' })
        res.end('internal error')
        return
      }
      let payload
      if (name === 'muninn_status') payload = { status: 'ok' }
      else if (name === 'muninn_remember') payload = { id: `eng-${calls.length}`, ...onRemember(args, calls.length) }
      else payload = {}
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ jsonrpc: '2.0', id: env.id, result: { content: [{ type: 'text', text: JSON.stringify(payload) }] } }))
    })
  })
  await new Promise((r) => srv.listen(0, '127.0.0.1', r))
  return {
    base: `http://127.0.0.1:${srv.address().port}/mcp`,
    calls,
    close: () => new Promise((r) => { srv.closeAllConnections?.(); srv.close(r) }),
  }
}

/** Spawn and expose the child, so a test can signal it deterministically. */
function spawnNode(script, args, { root, env = {} } = {}) {
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
  const done = new Promise((resolve) => p.on('close', (code, signal) => resolve({ code, signal, out, err })))
  return { proc: p, done }
}

function runNode(script, args, opts = {}) {
  return spawnNode(script, args, opts).done
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
  // The fixture has to be one that an UNDELIMITED formula gets wrong, or it pins nothing.
  // The previous pair here was ('alpha beta','gamma') vs ('alpha','beta gamma') — the space
  // that moves across the boundary is itself a delimiter, so plain concatenation
  // distinguishes them too and the test passed with the delimiting removed entirely.
  const a = { vault: 'v', concept: 'ab', content: 'ccc'.repeat(20) }
  const b = { vault: 'v', concept: 'a', content: 'b' + 'ccc'.repeat(20) }
  const naive = (p) => p.vault + p.concept + p.content
  assert.equal(naive(a), naive(b),
    'this fixture is only discriminating if an undelimited formula WOULD collide on it')
  assert.notEqual(opIdFor(a), opIdFor(b), 'a separator that can appear inside a field is not a separator')
})

// ── F1: the debounce watermark ────────────────────────────────────────────────────────────
//
// `last_run_at` moves forward and never back, and an untouched ledger's mtime does not move
// at all, so `ledgerM <= lastRun` never expires on its own. One run that advanced the
// watermark WITHOUT consuming anything therefore disabled the debounced `Stop` trigger
// permanently — measured: daemon down for one Stop, healthy for the next, still reporting
// "ledger unchanged since the last run" with considered 0 thirty days later, proposals
// stranded. That is precisely the crash/kill case `Stop` exists to cover, and `--hook`
// exit 0 means nothing reports it.
const OLD_WATERMARK = '2020-01-01T00:00:00.000Z'

function seedReceipt(root, lastRunAt) {
  writeFileSync(join(root, '.claude', 'memory-drain-receipt.json'),
    JSON.stringify({ at: lastRunAt, trigger: 'seed', outcome: 'ok', last_run_at: lastRunAt }, null, 2))
}
const readReceiptFile = (root) => JSON.parse(readFileSync(join(root, '.claude', 'memory-drain-receipt.json'), 'utf8'))

test('F1: only an outcome that consumed from the ledger advances the debounce watermark', async (t) => {
  const srv = await fakeMuninn()
  t.after(() => srv.close())

  // [name, advances?, setup -> {root, args, env}]
  const cases = [
    ['ok', true, () => {
      const { root } = makeRepo([proposal(1)]); return { root, args: ['--base', srv.base] }
    }],
    ['partial', true, () => {
      const { root } = makeRepo([proposal(1)])
      return { root, args: ['--base', srv.base], env: { MUNINN_MCP_TOKEN: '', MUNINN_TOKEN: '' } }
    }],
    ['empty', true, () => {
      const { root } = makeRepo([]); return { root, args: ['--base', srv.base] }
    }],
    ['no-ledger', true, () => {
      const { root, ledger } = makeRepo([]); rmSync(ledger); return { root, args: ['--base', srv.base] }
    }],
    ['rewrite-refused', true, () => {
      const { root, ledger } = makeRepo([proposal(1), proposal(2)])
      rewriteTarget = ledger
      return { root, args: ['--base', refuseSrv.base] }
    }],
    ['locked', false, () => {
      const { root } = makeRepo([proposal(1)])
      writeFileSync(join(root, '.claude', 'memory-proposals.lock'), JSON.stringify({ pid: 999999, at: new Date().toISOString() }))
      return { root, args: ['--base', srv.base] }
    }],
    ['unreachable', false, () => {
      const { root } = makeRepo([proposal(1)]); return { root, args: ['--base', 'http://127.0.0.1:1/mcp'] }
    }],
    ['error', false, () => {
      const { root, ledger } = makeRepo([]); rmSync(ledger); mkdirSync(ledger)   // EISDIR on read
      return { root, args: ['--base', srv.base] }
    }],
    ['debounced', false, () => {
      const { root } = makeRepo([proposal(1)])
      return { root, args: ['--base', srv.base, '--debounce', '60'], recent: true }
    }],
  ]

  // A dedicated server for the rewrite-refused case: it clobbers the consumed prefix.
  let rewriteTarget = null
  const refuseSrv = await fakeMuninn({ onRemember: () => { writeFileSync(rewriteTarget, JSON.stringify(proposal(42)) + '\n'); return {} } })
  t.after(() => refuseSrv.close())

  const wrong = []
  for (const [name, advances, setup] of cases) {
    const { root, args, env = {}, recent = false } = setup()
    const sentinel = recent ? new Date(Date.now() - 60_000).toISOString() : OLD_WATERMARK
    seedReceipt(root, sentinel)
    await runNode(DRAIN, args, { root, env })
    const rc = readReceiptFile(root)
    assert.equal(rc.outcome, name, `case '${name}' did not produce that outcome (got '${rc.outcome}' — ${rc.why})`)
    const moved = rc.last_run_at !== sentinel
    if (moved !== advances) {
      wrong.push(`${name}: watermark ${moved ? 'ADVANCED' : 'held'} (${rc.last_run_at}), expected ${advances ? 'advance' : 'hold at ' + sentinel}`)
    }
    if (advances) assert.ok(Date.parse(rc.last_run_at) > Date.parse(sentinel), `${name}: watermark must be a real forward timestamp`)
  }
  assert.deepEqual(wrong, [], `an outcome that consumed nothing must not move the debounce watermark:\n  ${wrong.join('\n  ')}`)
})

test('F1: a failed run does not wedge the next Stop — the drain retries and drains', async (t) => {
  const { root, ledger } = makeRepo([proposal(1)])
  // Stop #1: the daemon is down. Nothing consumed.
  await runNode(DRAIN, ['--base', 'http://127.0.0.1:1/mcp', '--hook', '--trigger', 'Stop', '--debounce', '20'], { root })
  assert.equal(readReceiptFile(root).outcome, 'unreachable')
  assert.equal(lines(ledger).length, 1)

  // Stop #2, more than the debounce window later, daemon healthy, ledger untouched since.
  const srv = await fakeMuninn()
  t.after(() => srv.close())
  const past = new Date(Date.now() - 60 * 60_000)
  const rc = readReceiptFile(root)
  writeFileSync(join(root, '.claude', 'memory-drain-receipt.json'), JSON.stringify({ ...rc, at: past.toISOString(), last_run_at: rc.last_run_at }, null, 2))

  await runNode(DRAIN, ['--base', srv.base, '--hook', '--trigger', 'Stop', '--debounce', '20'], { root })
  const rc2 = readReceiptFile(root)
  assert.notEqual(rc2.outcome, 'debounced', `the ledger has never been consumed; a Stop must not debounce it away (${rc2.why})`)
  assert.equal(rc2.counts.written, 1)
  assert.equal(lines(ledger).length, 0)
})

test('F1: the debounce DOES skip when the ledger genuinely has not changed since a real run', async (t) => {
  const srv = await fakeMuninn()
  t.after(() => srv.close())
  const { root } = makeRepo([proposal(1)])
  await runNode(DRAIN, ['--base', srv.base], { root })          // consumes; ledger now empty

  // Push both the watermark and the ledger's mtime well into the past, ledger oldest, so
  // `quiet` is false and only the ledger-unchanged branch can fire.
  const rc = readReceiptFile(root)
  const runAt = new Date(Date.now() - 90 * 60_000)
  writeFileSync(join(root, '.claude', 'memory-drain-receipt.json'), JSON.stringify({ ...rc, last_run_at: runAt.toISOString() }, null, 2))
  const older = new Date(Date.now() - 120 * 60_000)
  utimesSync(join(root, '.claude', 'memory-proposals.jsonl'), older, older)

  await runNode(DRAIN, ['--base', srv.base, '--debounce', '20'], { root })
  const rc2 = readReceiptFile(root)
  assert.equal(rc2.outcome, 'debounced')
  assert.match(rc2.why, /ledger unchanged since the last run/)
  assert.equal(rc2.considered, 0)
})

// ── F2: a drain that cannot finish must still release the lock and leave a receipt ────────
//
// `mcp()` had no timeout of any kind. A daemon that accepts the connection and never
// answers hung the drain for the whole 60 s hook timeout, and the SIGTERM that ended it
// left the lock on disk with NO receipt written — so `stat` returned the PREVIOUS receipt,
// fresh-looking and wrong, and the leaked lock then blocked every producer.
test('F2: a daemon that accepts and never answers is bounded, not hung', { timeout: 20_000 }, async (t) => {
  const srv = await fakeMuninn({ hang: 'muninn_status' })
  t.after(() => srv.close())
  const { root, ledger } = makeRepo([proposal(1)])

  const t0 = Date.now()
  const r = await runNode(DRAIN, ['--base', srv.base, '--timeout', '600'], { root })
  const elapsed = Date.now() - t0

  assert.ok(elapsed < 10_000, `the drain must bound its own call, not wait to be killed (took ${elapsed} ms)`)
  const rc = readReceiptFile(root)
  assert.equal(rc.outcome, 'unreachable')
  assert.match(rc.why, /no response within 600 ms/)
  assert.equal(existsSync(join(root, '.claude', 'memory-proposals.lock')), false, 'the lock must not survive the run')
  assert.equal(lines(ledger).length, 1, 'nothing was consumed')
  assert.equal(r.code, 1)
})

test('F2: a SIGTERM mid-write releases the lock and still writes a receipt', { timeout: 20_000 }, async (t) => {
  let killer = null
  const srv = await fakeMuninn({
    hang: 'muninn_remember',
    onArrive: (name) => { if (name === 'muninn_remember') killer?.() },
  })
  t.after(() => srv.close())
  const { root, ledger } = makeRepo([proposal(1)])

  // No timeout flag: this is the harness-kills-us path, not the self-bounded one.
  const { proc, done } = spawnNode(DRAIN, ['--base', srv.base, '--timeout', '60000'], { root })
  killer = () => proc.kill('SIGTERM')                     // fires when the request lands
  const r = await done

  assert.equal(existsSync(join(root, '.claude', 'memory-proposals.lock')), false,
    'a killed drain must not leave the lock behind — it blocks every producer until the stale breaker fires')
  const rc = readReceiptFile(root)
  assert.equal(rc.outcome, 'interrupted', r.out + r.err)
  assert.match(rc.why, /SIGTERM/)
  assert.equal(rc.last_run_at, null, 'an interrupted run consumed nothing, so it must not advance the watermark')
  assert.equal(lines(ledger).length, 1, 'the ledger is rewritten only at the end, so nothing was consumed')
})

test('F2: a producer never loses a finding to a held lock', async () => {
  const { root, ledger } = makeRepo([])
  writeFileSync(join(root, '.claude', 'memory-proposals.lock'), JSON.stringify({ pid: 999999, at: new Date().toISOString() }))
  const r = await runNode(PROPOSE, [], { root, env: { __stdin: JSON.stringify(proposal(1)) } })
  assert.equal(r.code, 0, `a held lock must not cost a finding:\n${r.err}`)
  assert.match(r.err, /appending anyway/)
  assert.equal(lines(ledger).length, 1)
})

// ── F3: the guards that had no coverage at all ────────────────────────────────────────────
test('F3: a server-side write failure retains the line — not written, not dead-lettered, not lost', async (t) => {
  const srv = await fakeMuninn({ rememberStatus: 500 })
  t.after(() => srv.close())
  const { root, ledger } = makeRepo([proposal(1)])

  const r = await runNode(DRAIN, ['--base', srv.base], { root })
  assert.equal(r.code, 1)
  assert.equal(lines(ledger).length, 1, 'a 500 is the world failing, not the line — the proposal stays queued')
  assert.equal(lines(join(root, '.claude', 'memory-proposals.deadletter.jsonl')).length, 0)
  const rc = readReceiptFile(root)
  assert.equal(rc.outcome, 'partial')
  assert.equal(rc.counts.failed, 1)
  assert.equal(rc.counts.retained, 1)
  assert.equal(rc.counts.written, 0)
})

test('F3: a drain does not run while another holds the lock, and touches nothing', async (t) => {
  const srv = await fakeMuninn()
  t.after(() => srv.close())
  const { root, ledger } = makeRepo([proposal(1)])
  const lockPath = join(root, '.claude', 'memory-proposals.lock')
  const holder = JSON.stringify({ pid: 999999, at: new Date().toISOString() })
  writeFileSync(lockPath, holder)

  await runNode(DRAIN, ['--base', srv.base], { root })
  const rc = readReceiptFile(root)
  assert.equal(rc.outcome, 'locked')
  assert.equal(lines(ledger).length, 1, 'a second drain must not consume the first one\'s ledger')
  assert.equal(srv.calls.length, 0, 'and must not write to the vault at all')
  assert.equal(readFileSync(lockPath, 'utf8'), holder, "and must not steal the holder's lock")
})

test('F3: a stale lock from a crashed holder is broken, not obeyed forever', async (t) => {
  const srv = await fakeMuninn()
  t.after(() => srv.close())
  const { root, ledger } = makeRepo([proposal(1)])
  const lockPath = join(root, '.claude', 'memory-proposals.lock')
  writeFileSync(lockPath, JSON.stringify({ pid: 999999, at: '2020-01-01T00:00:00.000Z' }))
  const ancient = new Date(Date.now() - 60 * 60_000)          // an hour old; breaker is 10 min
  utimesSync(lockPath, ancient, ancient)

  await runNode(DRAIN, ['--base', srv.base], { root })
  const rc = readReceiptFile(root)
  assert.equal(rc.outcome, 'ok', `a SIGKILLed holder must not wedge the queue forever (${rc.why})`)
  assert.equal(rc.counts.written, 1)
  assert.equal(lines(ledger).length, 0)
  assert.equal(existsSync(lockPath), false)
})

// ── F5: an idempotency hit must not silently swallow a correction ──────────────────────────
test('F5: a re-proposal whose non-identity fields changed is reported, never silently dropped', async (t) => {
  const srv = await fakeMuninn({ onRemember: () => ({ id: 'eng-existing', idempotent: true }) })
  t.after(() => srv.close())
  const corrected = proposal(1, { summary: 'a corrected summary', type: 'decision', entities: ['thing'] })
  const { root } = makeRepo([corrected])

  const r = await runNode(DRAIN, ['--base', srv.base], { root })
  const rc = readReceiptFile(root)
  assert.equal(rc.counts.idempotent, 1)
  assert.equal(rc.counts.unapplied_annotations, 1,
    'identity is (vault, concept, content); a changed summary/type/entities does NOT land, so it has to be said')
  assert.match(r.out, /NOT APPLIED/)
  assert.match(r.out, /summary/)
  const arch = JSON.parse(lines(join(root, '.claude', 'memory-proposals.drained.jsonl'))[0])
  assert.deepEqual(arch.annotations_not_applied, ['summary', 'type', 'entities'])
  assert.equal(arch.summary, 'a corrected summary', 'the correction is recoverable from the archive')
})

test('F5: an idempotency hit with nothing to correct stays quiet', async (t) => {
  const srv = await fakeMuninn({ onRemember: () => ({ id: 'eng-existing', idempotent: true }) })
  t.after(() => srv.close())
  const bare = { vault: 'testvault', concept: 'bare', content: 'A proposal carrying identity fields and nothing else at all, comfortably past forty.' }
  const { root } = makeRepo([bare])
  const r = await runNode(DRAIN, ['--base', srv.base], { root })
  assert.equal(readReceiptFile(root).counts.unapplied_annotations, 0)
  assert.doesNotMatch(r.out, /NOT APPLIED/)
})

// ── The read boundary: an unterminated trailing line is transient, not permanent ───────────
test('a half-written trailing line is left for the next run, never dead-lettered', async (t) => {
  const srv = await fakeMuninn()
  t.after(() => srv.close())
  const { root, ledger } = makeRepo([proposal(1)])
  const torn = '{"vault":"testvault","concept":"torn","cont'
  appendFileSync(ledger, torn)                                  // no newline: still being written

  await runNode(DRAIN, ['--base', srv.base], { root })
  assert.equal(lines(join(root, '.claude', 'memory-proposals.deadletter.jsonl')).length, 0,
    'an incomplete line is a transient state of the world; dead-lettering calls it permanent')
  assert.equal(readFileSync(ledger, 'utf8'), torn, 'the fragment survives byte-identical, so its completion still parses')
  const rc = readReceiptFile(root)
  assert.equal(rc.counts.written, 1)
  assert.equal(rc.ledger.partial_tail_bytes, torn.length)

  // …and once the writer finishes the line, it drains normally.
  appendFileSync(ledger, 'ent":"The rest of the line arrives on the second write, which is what makes it transient."}\n')
  await runNode(DRAIN, ['--base', srv.base], { root })
  assert.equal(lines(ledger).length, 0)
  assert.ok(srv.calls.some((c) => c.name === 'muninn_remember' && c.args.concept === 'torn'))
})

// ── F6: one ledger per repository, not one per worktree ───────────────────────────────────
//
// 17 real proposals sat undrained in two scratch worktrees. CLAUDE_PROJECT_DIR is empty in
// a Bash tool environment, so `paths()` fell back to cwd; an append made with a worktree as
// cwd landed there, and no session ever has a scratch worktree as its project root. The
// protocol claimed "no ledger is invisible to the process that would drain it".
test('F6: a linked worktree resolves to the main checkout, so there is one queue per repo', () => {
  const main = mkdtempSync(join(tmpdir(), 'muninn-mainrepo-'))
  mkdirSync(join(main, '.git', 'worktrees', 'scratch'), { recursive: true })
  writeFileSync(join(main, '.git', 'worktrees', 'scratch', 'commondir'), '../..\n')
  const wt = mkdtempSync(join(tmpdir(), 'muninn-worktree-'))
  writeFileSync(join(wt, '.git'), `gitdir: ${join(main, '.git', 'worktrees', 'scratch')}\n`)

  assert.equal(canonicalRoot(wt), main, 'a proposal appended from a worktree must queue where a drain will look')
  assert.equal(canonicalRoot(main), main, 'a main checkout resolves to itself')
  const notARepo = mkdtempSync(join(tmpdir(), 'muninn-norepo-'))
  assert.equal(canonicalRoot(notARepo), notARepo, 'and a directory that is not a checkout is left alone')
})

test('F6: a proposal made from a worktree lands in the repo ledger the drain reads', async (t) => {
  const srv = await fakeMuninn()
  t.after(() => srv.close())
  const main = mkdtempSync(join(tmpdir(), 'muninn-mainrepo-'))
  mkdirSync(join(main, '.git', 'worktrees', 'scratch'), { recursive: true })
  writeFileSync(join(main, '.git', 'worktrees', 'scratch', 'commondir'), '../..\n')
  const wt = mkdtempSync(join(tmpdir(), 'muninn-worktree-'))
  writeFileSync(join(wt, '.git'), `gitdir: ${join(main, '.git', 'worktrees', 'scratch')}\n`)

  const r = await runNode(PROPOSE, [], { root: wt, env: { __stdin: JSON.stringify(proposal(1)) } })
  assert.equal(r.code, 0, r.err)
  assert.equal(existsSync(join(wt, '.claude', 'memory-proposals.jsonl')), false, 'nothing may be stranded in the worktree')
  assert.equal(lines(join(main, '.claude', 'memory-proposals.jsonl')).length, 1)

  // A drain running in the main checkout — which is the only place one ever runs — sees it.
  await runNode(DRAIN, ['--base', srv.base], { root: main })
  assert.ok(srv.calls.some((c) => c.name === 'muninn_remember' && c.args.concept === proposal(1).concept))
})

// ── The receipt has a reader ───────────────────────────────────────────────────────────────
//
// Nothing anywhere read the receipt: a grep found the drain's own debounce, .gitignore and
// prose. A receipt nobody reads is a file, not observability — and combined with a watermark
// that could not un-wedge itself, `stat` showed a seconds-old `debounced` receipt while
// findings rotted, which is worse than silence because it looks like an answer.
const FRESH = join(HOOKS, 'memory-freshness.mjs')
const context = (r) => { try { return JSON.parse(r.out).hookSpecificOutput.additionalContext } catch { return '' } }

test('freshness: a queue with no receipt at all is reported at session start', async () => {
  const { root } = makeRepo([proposal(1), proposal(2)])
  const r = await runNode(FRESH, [], { root, env: { __stdin: JSON.stringify({ cwd: root, session_id: 's1' }) } })
  assert.equal(r.code, 0)
  assert.match(context(r), /2 memory proposal\(s\) are queued and the drain has never run/)
})

test('freshness: an unhealthy last outcome is reported, and a healthy empty one is silent', async (t) => {
  const srv = await fakeMuninn()
  t.after(() => srv.close())

  const bad = makeRepo([proposal(1)])
  await runNode(DRAIN, ['--base', 'http://127.0.0.1:1/mcp'], { root: bad.root })      // unreachable
  const r1 = await runNode(FRESH, [], { root: bad.root, env: { __stdin: JSON.stringify({ cwd: bad.root }) } })
  assert.match(context(r1), /ended `unreachable`/)
  assert.match(context(r1), /1 proposal\(s\) are still queued/)

  const good = makeRepo([proposal(1)])
  await runNode(DRAIN, ['--base', srv.base], { root: good.root })                     // ok, queue empty
  const r2 = await runNode(FRESH, [], { root: good.root, env: { __stdin: JSON.stringify({ cwd: good.root }) } })
  assert.equal(r2.out, '', 'a working pipe must say nothing at all')
  assert.equal(r2.code, 0)
})

test('freshness: it cannot fail a session start, whatever it is fed', async () => {
  const { root } = makeRepo([proposal(1)])
  for (const stdin of ['', 'not json', '[]', 'null', '{"cwd":123}', JSON.stringify({ cwd: '/nonexistent/nope' })]) {
    const r = await runNode(FRESH, [], { root, env: { __stdin: stdin } })
    assert.equal(r.code, 0, `exit 0 for stdin ${JSON.stringify(stdin)}; stderr: ${r.err}`)
  }
})

test('no committed hook script contains a NUL byte', async () => {
  const { readdirSync } = await import('node:fs')
  for (const f of readdirSync(HOOKS).filter((n) => n.endsWith('.mjs'))) {
    const buf = readFileSync(join(HOOKS, f))
    assert.equal(buf.includes(0), false, `${f} contains a NUL byte — git will treat it as binary and show no diff`)
  }
})
