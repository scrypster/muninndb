# Playwright E2E Test Suite — Design Spec

**Date:** 2026-03-10
**Status:** Approved
**Motivation:** Issue #168 (plugin config not persisted) was caught by manual testing. No automated guard exists for the browser-side save→reload flow. This suite provides ironclad regression protection for the full web UI happy path.

---

## Goals

- Automatically verify the full UI happy path on every CI run
- Catch regressions in: dashboard load, memory CRUD, plugin config persistence
- Run against a hermetic, fresh-data server — no shared state, no cleanup logic

## Out of Scope

- Cross-browser testing (Chromium only)
- Mobile viewports
- Visual regression / screenshot diffing
- Observability, graph, logs, cluster views (future)

---

## Architecture

### Approach: Fresh Server Per Run (Option A)

`playwright.config.ts` uses the `webServer` option to:
1. Build the `muninn` binary (`go build -o /tmp/muninn-e2e ./cmd/muninn`)
2. Start `muninn server` pointing at a temp data directory (`/tmp/muninn-e2e-data-{timestamp}`)
3. Wait for port 8476 to be ready
4. Kill the process and delete the temp dir after the run

Every test run starts with a completely empty database. No vault cleanup hooks. No shared state hazards between tests.

### File Structure

```
web/
├── e2e/
│   ├── fixtures/
│   │   └── auth.ts          # authenticated page fixture (login once per worker)
│   ├── dashboard.spec.ts    # dashboard loads, engram count visible
│   ├── memories.spec.ts     # create → search → verify → (implicit cleanup via fresh server)
│   ├── settings.spec.ts     # plugin config save → hard reload → verify persistence
│   └── smoke.spec.ts        # full happy path end-to-end
├── playwright.config.ts
├── package.json             # adds @playwright/test
```

### Auth Fixture

The server starts with default credentials. The `auth.ts` fixture logs in once per worker via the UI login flow, stores the authenticated browser state, and provides a pre-authenticated `page` to every test. Tests never handle login themselves.

---

## Selectors Strategy

`data-testid` attributes are added surgically to the HTML/templates — only on elements actually tested. This makes tests immune to CSS class changes and label renames.

| Element | `data-testid` |
|---|---|
| Engram count stat | `stat-engram-count` |
| New memory button | `btn-new-memory` |
| Memory concept input | `input-concept` |
| Memory content textarea | `input-content` |
| Save memory button | `btn-save-memory` |
| Memory list item | `memory-item` |
| Search input | `input-search` |
| Settings → Plugins tab | `tab-plugins` |
| Enrich provider select | `select-enrich-provider` |
| Enrich URL input | `input-enrich-url` |
| Save plugin config button | `btn-save-plugin-config` |

---

## Test Suites

### `dashboard.spec.ts`
- Page loads without JS errors
- Engram count stat is visible (value: 0 on fresh server)
- Navigation hash changes when clicking nav items

### `memories.spec.ts`
- Click "New Memory" → modal appears
- Fill concept + content → save → modal closes
- Memory appears in the list with correct concept
- Search for the concept → memory is findable

### `settings.spec.ts` ← **primary regression guard for issue #168**
- Navigate to Settings → Plugins tab
- Set enrich provider to "ollama", enter URL `ollama://localhost:11434/llama3`
- Click Save → success indicator appears
- `page.reload()` (hard browser reload)
- Verify enrich provider is still "ollama"
- Verify enrich URL field still shows `llama3`

### `smoke.spec.ts`
- Full sequential run of the above three flows in one test

---

## Build Integration

**`package.json` additions:**
```json
"scripts": {
  "test": "vitest run",
  "test:e2e": "playwright test",
  "test:e2e:ui": "playwright test --ui"
},
"devDependencies": {
  "@playwright/test": "^1.44.0"
}
```

**Local usage:**
```bash
cd web
npm run test:e2e
```

**CI:** Added as a step after existing Go tests in GitHub Actions. The workflow already builds the binary; Playwright just needs `npx playwright install chromium` added before the test step.

---

## Error Handling

- All waits use `expect(...).toBeVisible({ timeout: 5000 })` — no arbitrary `sleep()`
- If the server fails to start, `webServer` fails fast with a clear error
- Failed tests capture a screenshot automatically (Playwright default)
- Single worker (sequential) to avoid port conflicts on the shared server

---

## Success Criteria

- `npm run test:e2e` exits 0 on a clean build
- The settings persistence test (`settings.spec.ts`) would have caught issue #168
- Tests are stable across 3 consecutive runs with no flakiness
