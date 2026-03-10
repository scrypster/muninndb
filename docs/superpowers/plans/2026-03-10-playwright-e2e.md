# Playwright E2E Test Suite Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a Playwright E2E test suite that provides ironclad regression protection for the MuninnDB web UI — specifically the plugin config persistence bug (issue #168), the memory CRUD flow, and the dashboard.

**Architecture:** Playwright manages the full server lifecycle — it builds the `muninn` binary, starts the server against a clean temp data directory, and kills it after the run. Every test run starts with a blank database: zero contamination, no cleanup logic needed. Auth is handled once via global setup using default credentials, persisted as browser storage state.

**Tech Stack:** `@playwright/test ^1.44.0`, Chromium only, TypeScript, single worker (sequential).

---

## Background: Key File Locations

| File | Purpose |
|---|---|
| `web/templates/index.html` | Main HTML — all data-testid attributes added here |
| `web/static/js/app.js` | Alpine.js component — apiCall uses relative URLs to port 8476 |
| `web/package.json` | Add @playwright/test and npm scripts |
| `web/playwright.config.ts` | New — Playwright config |
| `web/e2e/global-setup.ts` | New — logs in once, saves browser storage state |
| `web/e2e/fixtures/auth.ts` | New — provides authenticated `page` to all tests |
| `web/e2e/dashboard.spec.ts` | New — dashboard load test |
| `web/e2e/memories.spec.ts` | New — memory create + search test |
| `web/e2e/settings.spec.ts` | New — plugin config persist test (the #168 guard) |
| `web/e2e/smoke.spec.ts` | New — full happy path |

## Background: DOM Reference (no data-testid yet)

These are the exact elements we'll be tagging:

| What | data-testid | Location in index.html | Line |
|---|---|---|---|
| Engram count value | `stat-engram-count` | `<div class="stat-value" x-text="stats.engramCount.toLocaleString()">` | ~199 |
| New Memory button | `btn-new-memory` | `<button class="btn-primary" @click="showNewMemoryModal=true"` | ~350 |
| Memory search input | `input-search` | `<input id="memory-search-input" class="input-field"` | ~327 |
| Memory list items | `memory-item` | `<div class="memory-card"` (inside `x-for="m in memories"`) | ~364 |
| Concept input (modal) | `input-concept` | `<input class="input-field" x-model="newMemoryForm.concept"` | ~2514 |
| Content textarea (modal) | `input-content` | `<textarea class="input-field" x-model="newMemoryForm.content"` | ~2518 |
| Create Memory submit | `btn-create-memory` | `<button type="submit" class="btn-primary">Create Memory</button>` | ~2530 |
| Plugins tab button | `tab-plugins` | `<button class="tab-btn" ... @click="settingsTab='plugins'...">Plugins</button>` | ~889 |
| Enrich provider pills | *(no testid — selected by button text via `getByRole`)* | `<button @click="pluginCfg.enrichProvider=opt.val"` (x-for loop) | ~1531 |
| Enrich Ollama model input | `input-enrich-ollama-model` | `<input class="input-field" x-model="pluginCfg.enrichOllamaModel"` (inside `x-if="pluginCfg.ollamaDetected !== true"`) | ~1581 |
| Enrich save button | `btn-save-enrich` | `<button class="btn-primary" @click="savePluginConfig('enrich')">Save</button>` | ~1619 |
| Enrich saved message | `enrich-saved-msg` | `<span x-show="pluginCfg.enrichSaved" ...>Saved — restart MuninnDB to apply.</span>` | ~1623 |

**Note on selector naming:** The spec design table used `btn-save-memory` but the actual HTML submit button reads "Create Memory". The plan uses `btn-create-memory` which matches the button text — this is intentional and correct.

**Note on enrich provider pills:** The pills are rendered by `x-for` so a static `data-testid` is not practical. Tests select them using `page.getByRole('button', { name: 'Ollama' })` — role-based selectors are stable and idiomatic Playwright.

## Background: Auth Flow

- Default credentials: `{"username":"root","password":"password"}`
- Login endpoint: `POST /api/auth/login` (served by port 8476 web UI server)
- Session stored as cookie `muninn_session`
- All `apiCall()` in app.js uses relative URLs — browser sends to port 8476

## Background: Settings Test Logic

On a fresh server:
- No plugins configured → `plugins.some(p => p.tier === 3)` is `false`
- This means `x-if="!plugins.some(p => p.tier === 3) || pluginCfg.enrichShowForm"` evaluates `true`
- So the enrich config form IS visible without any extra clicks
- Ollama is not running → `pluginCfg.ollamaDetected !== true` is `true` → manual model text input shown (line ~1581)
- After save: `pluginCfg.enrichSaved = true` → success message appears
- After hard reload: `loadSavedPluginConfig()` repopulates `pluginCfg.enrichProvider` and `pluginCfg.enrichOllamaModel`

---

## Chunk 1: Infrastructure (install, config, auth)

### Task 1: Install Playwright

**Files:**
- Modify: `web/package.json`

- [ ] **Step 1: Add @playwright/test to package.json**

  In `web/package.json`, replace the current content with:
  ```json
  {
    "name": "muninndb-web",
    "private": true,
    "version": "1.0.0",
    "type": "module",
    "scripts": {
      "dev": "vite",
      "build": "vite build",
      "preview": "vite preview",
      "test": "vitest run",
      "test:e2e": "playwright test",
      "test:e2e:ui": "playwright test --ui"
    },
    "devDependencies": {
      "vite": "^6.0.0",
      "tailwindcss": "^3.4.0",
      "postcss": "^8.4.0",
      "autoprefixer": "^10.4.0",
      "vitest": "^2.0.0",
      "@playwright/test": "^1.44.0"
    }
  }
  ```

- [ ] **Step 2: Install dependencies**

  ```bash
  cd web && npm install
  ```
  Expected: `package-lock.json` updated, `node_modules/@playwright` present.

- [ ] **Step 3: Install Chromium browser**

  ```bash
  cd web && npx playwright install chromium
  ```
  Expected: Chromium downloaded to `~/.cache/ms-playwright/`.

- [ ] **Step 4: Verify playwright is installed**

  ```bash
  cd web && npx playwright --version
  ```
  Expected: prints `Version 1.44.x` or higher.

- [ ] **Step 5: Commit**

  ```bash
  git add web/package.json web/package-lock.json
  git commit -m "test(e2e): install @playwright/test"
  ```

---

### Task 2: Create playwright.config.ts

**Files:**
- Create: `web/playwright.config.ts`

Key decisions (post Opus round 2):
- Binary path and data dir extracted as constants (referenced by both config and global-setup)
- `rm -rf` removed from shell command — moved to `globalSetup` TypeScript (safer, controlled)
- `retries: process.env.CI ? 1 : 0` — one retry on CI catches transient runner slowness
- `expect.timeout: 10_000` — default; per-assertion overrides removed from spec files
- `globalTeardown` added to clean up temp dir after the run

- [ ] **Step 1: Create web/playwright.config.ts**

  ```typescript
  import { defineConfig, devices } from '@playwright/test'
  import path from 'path'
  import { fileURLToPath } from 'url'

  const __dirname = path.dirname(fileURLToPath(import.meta.url))
  const projectRoot = path.join(__dirname, '..')

  // Exported so global-setup.ts and global-teardown.ts can import them
  export const E2E_BINARY = '/tmp/muninn-e2e'
  export const E2E_DATA_DIR = '/tmp/muninn-e2e-data'

  export default defineConfig({
    testDir: './e2e',
    fullyParallel: false,   // single worker — all tests share one server
    workers: 1,
    retries: process.env.CI ? 1 : 0,
    timeout: 30_000,
    reporter: 'list',

    expect: {
      timeout: 10_000,  // default for all toBeVisible/toHaveValue/etc assertions
    },

    globalSetup: './e2e/global-setup.ts',
    globalTeardown: './e2e/global-teardown.ts',

    use: {
      baseURL: 'http://localhost:8476',
      storageState: './e2e/.auth.json',
      trace: 'retain-on-failure',
      screenshot: 'only-on-failure',
    },

    projects: [
      {
        name: 'chromium',
        use: { ...devices['Desktop Chrome'] },
      },
    ],

    webServer: {
      // Build binary only — data dir cleanup is in globalSetup (TypeScript, not raw shell)
      // rm -rf is safe: E2E_DATA_DIR is a constant we define (/tmp/muninn-e2e-data), not user input
      command: `cd ${projectRoot} && go build -o ${E2E_BINARY} ./cmd/muninn && rm -rf ${E2E_DATA_DIR} && ${E2E_BINARY} server --data-dir ${E2E_DATA_DIR}`,
      // url (not port) — Playwright polls for HTTP 2xx, avoiding race with route registration
      url: 'http://localhost:8476/',
      timeout: 120_000,
      reuseExistingServer: false,
    },
  })
  ```

- [ ] **Step 2: Add .auth.json to .gitignore**

  Append to `web/.gitignore` (create if not present):
  ```
  e2e/.auth.json
  ```
  (Or append to project root `.gitignore` if `web/.gitignore` doesn't exist.)

  Check if root .gitignore exists first:
  ```bash
  ls /Users/mjbonanno/github.com/scrypster/muninndb/.gitignore
  ```
  If it does, append:
  ```bash
  echo "web/e2e/.auth.json" >> /Users/mjbonanno/github.com/scrypster/muninndb/.gitignore
  ```

- [ ] **Step 3: Commit**

  ```bash
  git add web/playwright.config.ts .gitignore
  git commit -m "test(e2e): add playwright config with fresh-server webServer"
  ```

---

### Task 3: Create global-setup.ts, global-teardown.ts, and auth fixture

**Files:**
- Create: `web/e2e/global-setup.ts`
- Create: `web/e2e/global-teardown.ts`
- Create: `web/e2e/fixtures/auth.ts`

The global setup:
1. Wipes `/tmp/muninn-e2e-data` (controlled TypeScript, not raw shell `rm -rf`)
2. Logs in via `request.newContext()` — no browser launch, correct cookie persistence
3. Saves storage state to `.auth.json`

The global teardown cleans up the temp data dir after all tests finish.

- [ ] **Step 1: Create web/e2e/global-setup.ts**

  Using `request.newContext()` from `@playwright/test` directly — no browser needed, and `APIRequestContext.storageState()` correctly persists cookies from API responses.

  ```typescript
  import { request, FullConfig } from '@playwright/test'
  import { rmSync, mkdirSync } from 'fs'
  import { E2E_DATA_DIR } from '../playwright.config.js'

  export default async function globalSetup(config: FullConfig) {
    // Clean data dir before server starts (webServer starts after globalSetup returns)
    // Note: webServer has NOT started yet here — this is purely disk cleanup
    rmSync(E2E_DATA_DIR, { recursive: true, force: true })
    mkdirSync(E2E_DATA_DIR, { recursive: true })

    const { baseURL } = config.projects[0].use

    // API request context — no browser launch needed for pure API calls
    // APIRequestContext.storageState() correctly captures cookies from responses
    const ctx = await request.newContext({ baseURL })
    const response = await ctx.post('/api/auth/login', {
      data: { username: 'root', password: 'password' },
    })

    if (!response.ok()) {
      await ctx.dispose()
      throw new Error(`Login failed: ${response.status()} ${await response.text()}`)
    }

    // Save cookies for all tests to reuse via use.storageState in config
    await ctx.storageState({ path: './e2e/.auth.json' })
    await ctx.dispose()
  }
  ```

  **Important:** `globalSetup` runs BEFORE `webServer` starts. The disk cleanup (`rmSync`) is safe here. The API login call runs AFTER the server is up because Playwright starts `webServer` first, then calls `globalSetup`.

  **Correction:** Actually in Playwright, `globalSetup` runs AFTER `webServer` is ready. The order is: `webServer` starts → server becomes reachable at `url` → `globalSetup` runs → tests run → `globalTeardown` runs → `webServer` killed. So the disk cleanup should happen BEFORE the server starts. Move the `rmSync`/`mkdirSync` into the webServer `command` preamble OR accept that on the first run the data dir doesn't exist (which is fine — `force: true` handles that). The `mkdirSync` before the server starts is guaranteed since globalSetup runs after the server is already up and has already created the data dir. Remove the `mkdirSync` call — just the `rmSync` is wrong here too since the server is already running.

  **Revised approach:** The data dir cleanup belongs in the webServer `command` shell before starting the binary. But we moved it out of the shell for safety. The resolution: use a dedicated pre-script or accept that the data dir is wiped by `rm -rf` in a controlled way. Since the `rm -rf` is on a known, fixed path (`/tmp/muninn-e2e-data`) that we own, it IS safe. Put it back in the shell command as the simplest correct approach:

  ```typescript
  import { request, FullConfig } from '@playwright/test'

  export default async function globalSetup(config: FullConfig) {
    const { baseURL } = config.projects[0].use

    // Server is already running at this point (webServer started before globalSetup)
    // Authenticate and save storage state for all tests
    const ctx = await request.newContext({ baseURL })
    const response = await ctx.post('/api/auth/login', {
      data: { username: 'root', password: 'password' },
    })

    if (!response.ok()) {
      await ctx.dispose()
      throw new Error(`Login failed: ${response.status()} ${await response.text()}`)
    }

    await ctx.storageState({ path: './e2e/.auth.json' })
    await ctx.dispose()
  }
  ```

  And update `playwright.config.ts` webServer command to include the `rm -rf` as a controlled pre-step (it is safe — fixed path we own):
  ```typescript
  command: `cd ${projectRoot} && go build -o ${E2E_BINARY} ./cmd/muninn && rm -rf ${E2E_DATA_DIR} && ${E2E_BINARY} server --data-dir ${E2E_DATA_DIR}`,
  ```
  This is the canonical correct approach. The `rm -rf` is on a constant we define, not user input.

- [ ] **Step 2: Create web/e2e/global-teardown.ts**

  ```typescript
  import { rmSync } from 'fs'
  import { E2E_DATA_DIR } from '../playwright.config.js'

  export default async function globalTeardown() {
    // Best-effort cleanup — server has already been killed by Playwright at this point
    rmSync(E2E_DATA_DIR, { recursive: true, force: true })
  }
  ```

- [ ] **Step 2: Create web/e2e/fixtures/auth.ts**

  This file exports a `test` object extended with an authenticated page fixture. All specs import from here instead of directly from `@playwright/test`.

  ```typescript
  import { test as base, expect } from '@playwright/test'

  // Re-export expect for convenience
  export { expect }

  // Extend test with an authenticated page that waits for the app to be ready
  export const test = base.extend({
    page: async ({ page }, use) => {
      // Navigate to home and wait for Alpine to initialize
      await page.goto('/')
      // The app-layout div is shown only when authenticated (x-show="isAuthenticated")
      await page.locator('.app-layout').waitFor({ state: 'visible' })
      await use(page)
    },
  })
  ```

- [ ] **Step 3: Verify the file structure**

  ```bash
  ls web/e2e/ web/e2e/fixtures/
  ```
  Expected:
  ```
  web/e2e/:
  fixtures/  global-setup.ts  global-teardown.ts

  web/e2e/fixtures/:
  auth.ts
  ```

- [ ] **Step 4: Commit**

  ```bash
  git add web/e2e/
  git commit -m "test(e2e): add global-setup, global-teardown, and auth fixture"
  ```

---

## Chunk 2: data-testid Attributes

### Task 4: Add data-testid to index.html

**Files:**
- Modify: `web/templates/index.html`

Add `data-testid` attributes ONLY to elements the tests actually use. No other changes.

**Important:** `<template x-for>` and `<template x-if>` are Vue/Alpine directives that render to the DOM — the `data-testid` goes on the real rendered element inside the template, not on `<template>` itself.

- [ ] **Step 1: Tag the engram count stat (~line 199)**

  Find:
  ```html
  <div class="stat-value" x-text="stats.engramCount.toLocaleString()">0</div>
  ```
  Replace with:
  ```html
  <div class="stat-value" data-testid="stat-engram-count" x-text="stats.engramCount.toLocaleString()">0</div>
  ```

- [ ] **Step 2: Tag the New Memory button (~line 350)**

  Find:
  ```html
  <button class="btn-primary" @click="showNewMemoryModal=true" title="New memory (n)">+ New Memory</button>
  ```
  Replace with:
  ```html
  <button class="btn-primary" data-testid="btn-new-memory" @click="showNewMemoryModal=true" title="New memory (n)">+ New Memory</button>
  ```

- [ ] **Step 3: Tag the memory search input (~line 327)**

  Find:
  ```html
  <input id="memory-search-input" class="input-field" placeholder="Search memories…
  ```
  Replace with:
  ```html
  <input id="memory-search-input" data-testid="input-search" class="input-field" placeholder="Search memories…
  ```

- [ ] **Step 4: Tag memory list items (~line 364)**

  Find:
  ```html
  <div class="memory-card" @click="multiSelectMode ? toggleMemorySelection(m.id) : openMemory(m)"
  ```
  Replace with:
  ```html
  <div class="memory-card" data-testid="memory-item" @click="multiSelectMode ? toggleMemorySelection(m.id) : openMemory(m)"
  ```

- [ ] **Step 5: Tag the New Memory modal inputs (~line 2514)**

  Find:
  ```html
  <input class="input-field" x-model="newMemoryForm.concept" required placeholder="golang channels" />
  ```
  Replace with:
  ```html
  <input class="input-field" data-testid="input-concept" x-model="newMemoryForm.concept" required placeholder="golang channels" />
  ```

  Find:
  ```html
  <textarea class="input-field" x-model="newMemoryForm.content" required rows="4" placeholder="Channels are the pipes that connect concurrent goroutines…"></textarea>
  ```
  Replace with:
  ```html
  <textarea class="input-field" data-testid="input-content" x-model="newMemoryForm.content" required rows="4" placeholder="Channels are the pipes that connect concurrent goroutines…"></textarea>
  ```

- [ ] **Step 6: Tag the Create Memory submit button (~line 2530)**

  Find:
  ```html
  <button type="submit" class="btn-primary">Create Memory</button>
  ```
  Replace with:
  ```html
  <button type="submit" data-testid="btn-create-memory" class="btn-primary">Create Memory</button>
  ```

- [ ] **Step 7: Tag the Plugins tab button (~line 889)**

  Find:
  ```html
  <button class="tab-btn" :class="{ active: settingsTab==='plugins' }" @click="settingsTab='plugins'; loadPlugins(); loadEmbedStatus(); navigateTo('settings/plugins')">Plugins</button>
  ```
  Replace with:
  ```html
  <button class="tab-btn" data-testid="tab-plugins" :class="{ active: settingsTab==='plugins' }" @click="settingsTab='plugins'; loadPlugins(); loadEmbedStatus(); navigateTo('settings/plugins')">Plugins</button>
  ```

- [ ] **Step 8: Tag the enrich Ollama model input (~line 1581)**

  Find (the one inside `x-if="pluginCfg.ollamaDetected !== true"`):
  ```html
  <input class="input-field" x-model="pluginCfg.enrichOllamaModel" placeholder="llama3.2" />
  ```
  Replace with:
  ```html
  <input class="input-field" data-testid="input-enrich-ollama-model" x-model="pluginCfg.enrichOllamaModel" placeholder="llama3.2" />
  ```

- [ ] **Step 8b: Tag the enrich plugin card container (~line 1486)**

  This scopes the Ollama pill selector to the enrich section, avoiding `.last()` fragility.
  Find the LLM Enricher card div — it is the second card in the plugin cards grid. Look for the comment `<!-- ── LLM Enricher card ──` or the heading "LLM Enricher". Add `data-testid="section-enrich-plugins"` to its outermost `<div class="card-polished">`.

  Example — find:
  ```html
  <!-- ── LLM Enricher card ── -->
  <div class="card-polished">
  ```
  Replace with:
  ```html
  <!-- ── LLM Enricher card ── -->
  <div class="card-polished" data-testid="section-enrich-plugins">
  ```

- [ ] **Step 9: Tag the enrich save button (~line 1619)**

  Find:
  ```html
  <button class="btn-primary" style="font-size:0.8125rem;" @click="savePluginConfig('enrich')">Save</button>
  ```
  Replace with:
  ```html
  <button class="btn-primary" data-testid="btn-save-enrich" style="font-size:0.8125rem;" @click="savePluginConfig('enrich')">Save</button>
  ```

- [ ] **Step 10: Tag the enrich saved confirmation message (~line 1623)**

  Find:
  ```html
  <span x-show="pluginCfg.enrichSaved" style="font-size:0.8125rem;color:var(--success,#22c55e);">Saved — restart MuninnDB to apply.</span>
  ```
  Replace with:
  ```html
  <span data-testid="enrich-saved-msg" x-show="pluginCfg.enrichSaved" style="font-size:0.8125rem;color:var(--success,#22c55e);">Saved — restart MuninnDB to apply.</span>
  ```

- [ ] **Step 11: Verify the server still serves correctly**

  Start the server manually and spot-check the page loads:
  ```bash
  cd /Users/mjbonanno/github.com/scrypster/muninndb
  go run ./cmd/muninn server &
  sleep 2
  curl -s http://localhost:8476 | grep 'data-testid' | head -5
  kill %1
  ```
  Expected: several `data-testid=` lines printed.

- [ ] **Step 12: Commit**

  ```bash
  git add web/templates/index.html
  git commit -m "test(e2e): add data-testid attributes to index.html"
  ```

---

## Chunk 3: Test Specs

### Task 5: dashboard.spec.ts

**Files:**
- Create: `web/e2e/dashboard.spec.ts`

- [ ] **Step 1: Create web/e2e/dashboard.spec.ts**

  ```typescript
  import { test, expect } from './fixtures/auth.js'

  // Timeouts: expect.timeout defaults to 10_000ms (set in playwright.config.ts)
  // No inline { timeout } needed unless overriding the default

  test.describe('Dashboard', () => {
    test('loads and shows engram count', async ({ page }) => {
      await page.goto('/#/dashboard')

      const count = page.getByTestId('stat-engram-count')
      await expect(count).toBeVisible()
      // Fresh server — engram count should be 0
      await expect(count).toHaveText('0')
    })

    test('navigation changes the active view', async ({ page }) => {
      await page.goto('/#/dashboard')
      await expect(page.getByTestId('stat-engram-count')).toBeVisible()
      // Navigate to memories view
      await page.getByRole('button', { name: /memories/i }).first().click()
      await expect(page).toHaveURL(/#\/memories/)
    })
  })
  ```

- [ ] **Step 2: Run just this spec to verify it works**

  ```bash
  cd web && npx playwright test e2e/dashboard.spec.ts --reporter=list
  ```
  Expected: `2 passed`.

- [ ] **Step 3: Commit**

  ```bash
  git add web/e2e/dashboard.spec.ts
  git commit -m "test(e2e): add dashboard spec"
  ```

---

### Task 6: memories.spec.ts

**Files:**
- Create: `web/e2e/memories.spec.ts`

- [ ] **Step 1: Create web/e2e/memories.spec.ts**

  ```typescript
  import { test, expect } from './fixtures/auth.js'

  // Timeouts: expect.timeout defaults to 10_000ms (set in playwright.config.ts)

  test.describe('Memories', () => {
    test('creates a memory and finds it in the list', async ({ page }) => {
      await page.goto('/#/memories')
      await expect(page.getByTestId('btn-new-memory')).toBeVisible()
      await page.getByTestId('btn-new-memory').click()

      await page.getByTestId('input-concept').fill('playwright test concept')
      await page.getByTestId('input-content').fill('This is a test memory created by Playwright E2E tests')
      await page.getByTestId('btn-create-memory').click()

      await expect(page.getByTestId('btn-create-memory')).toBeHidden()
      await expect(page.getByTestId('memory-item').first()).toContainText('playwright test concept')
    })

    test('searches for a memory by concept', async ({ page }) => {
      await page.goto('/#/memories')
      await expect(page.getByTestId('btn-new-memory')).toBeVisible()

      await page.getByTestId('btn-new-memory').click()
      await page.getByTestId('input-concept').fill('searchable concept xyz')
      await page.getByTestId('input-content').fill('Unique content for search test')
      await page.getByTestId('btn-create-memory').click()
      await expect(page.getByTestId('btn-create-memory')).toBeHidden()

      await page.getByTestId('input-search').fill('searchable concept xyz')
      await page.getByRole('button', { name: 'Search' }).click()
      await expect(page.getByTestId('memory-item').first()).toContainText('searchable concept xyz')
    })
  })
  ```

- [ ] **Step 2: Run just this spec**

  ```bash
  cd web && npx playwright test e2e/memories.spec.ts --reporter=list
  ```
  Expected: `2 passed`.

- [ ] **Step 3: Commit**

  ```bash
  git add web/e2e/memories.spec.ts
  git commit -m "test(e2e): add memories spec (create + search)"
  ```

---

### Task 7: settings.spec.ts — the #168 regression guard

**Files:**
- Create: `web/e2e/settings.spec.ts`

This is the most important spec. It directly proves issue #168 is fixed and will catch any future regression where plugin config isn't loaded on page reload.

**How it works:**
1. Navigate to Settings → Plugins tab
2. The form is visible (fresh server, no active plugins)
3. Click "Ollama" provider pill button
4. Fill the model name input (shown because Ollama is not running)
5. Click Save → wait for "Saved — restart MuninnDB to apply." message
6. Hard reload (`page.reload()`) — simulates a browser refresh
7. Navigate back to Settings → Plugins
8. Assert: Ollama pill is active (has `.active` class) AND model name input has the saved value

- [ ] **Step 1: Create web/e2e/settings.spec.ts**

  Key changes from initial draft:
  - Ollama pill selected via `getByTestId('section-enrich-plugins').getByRole(...)` — scoped, no `.last()` fragility
  - `networkidle` replaced with explicit UI-state waits

  ```typescript
  import { test, expect } from './fixtures/auth.js'

  // Timeouts: expect.timeout defaults to 10_000ms (set in playwright.config.ts)

  test.describe('Settings: Plugin Config Persistence', () => {
    test('plugin config persists after page reload (#168 regression guard)', async ({ page }) => {
      await page.goto('/#/settings/plugins')

      const pluginsTab = page.getByTestId('tab-plugins')
      await expect(pluginsTab).toBeVisible()
      await pluginsTab.click()

      // Scope to enrich section — avoids ambiguity with embed section Ollama pill
      const enrichSection = page.getByTestId('section-enrich-plugins')
      await expect(enrichSection).toBeVisible()
      await enrichSection.getByRole('button', { name: 'Ollama' }).click()

      // Manual model input shown when Ollama not running (fresh server)
      const modelInput = page.getByTestId('input-enrich-ollama-model')
      await expect(modelInput).toBeVisible()
      await modelInput.fill('llama3.2')

      await page.getByTestId('btn-save-enrich').click()
      await expect(page.getByTestId('enrich-saved-msg')).toContainText('Saved')

      // ── Hard reload — this is the #168 regression guard ──
      await page.reload()
      await page.locator('.app-layout').waitFor({ state: 'visible' })

      await page.getByTestId('tab-plugins').click()
      const enrichSectionAfterReload = page.getByTestId('section-enrich-plugins')
      await expect(enrichSectionAfterReload).toBeVisible()

      // ASSERT: loadSavedPluginConfig() repopulated enrichProvider → pill is active
      await expect(enrichSectionAfterReload.getByRole('button', { name: 'Ollama' })).toHaveClass(/active/)
      // ASSERT: loadSavedPluginConfig() repopulated enrichOllamaModel → input has value
      await expect(page.getByTestId('input-enrich-ollama-model')).toHaveValue('llama3.2')
    })
  })
  ```

- [ ] **Step 2: Run just this spec**

  ```bash
  cd web && npx playwright test e2e/settings.spec.ts --reporter=list
  ```
  Expected: `1 passed`.

  If it fails, debug with:
  ```bash
  cd web && npx playwright test e2e/settings.spec.ts --headed --reporter=list
  ```

- [ ] **Step 3: Commit**

  ```bash
  git add web/e2e/settings.spec.ts
  git commit -m "test(e2e): add settings spec — plugin config persistence (#168 regression guard)"
  ```

---

### Task 8: smoke.spec.ts

**Files:**
- Create: `web/e2e/smoke.spec.ts`

The smoke test chains all three flows into one sequential run. If it passes, the whole happy path works end-to-end.

- [ ] **Step 1: Create web/e2e/smoke.spec.ts**

  ```typescript
  import { test, expect } from './fixtures/auth.js'

  // Timeouts: expect.timeout defaults to 10_000ms (set in playwright.config.ts)

  test('full happy path smoke test', async ({ page }) => {
    // ── 1. Dashboard ──
    await page.goto('/#/dashboard')
    await expect(page.getByTestId('stat-engram-count')).toBeVisible()

    // ── 2. Create a memory ──
    await page.goto('/#/memories')
    await expect(page.getByTestId('btn-new-memory')).toBeVisible()
    await page.getByTestId('btn-new-memory').click()
    await page.getByTestId('input-concept').fill('smoke test memory')
    await page.getByTestId('input-content').fill('Created during the E2E smoke test run')
    await page.getByTestId('btn-create-memory').click()
    await expect(page.getByTestId('btn-create-memory')).toBeHidden()
    await expect(page.getByTestId('memory-item').first()).toContainText('smoke test memory')

    // ── 3. Search for the memory ──
    await page.getByTestId('input-search').fill('smoke test memory')
    await page.getByRole('button', { name: 'Search' }).click()
    await expect(page.getByTestId('memory-item').first()).toContainText('smoke test memory')

    // ── 4. Plugin config persists after reload (#168 guard) ──
    await page.goto('/#/settings/plugins')
    const pluginsTab = page.getByTestId('tab-plugins')
    await expect(pluginsTab).toBeVisible()
    await pluginsTab.click()

    const enrichSection = page.getByTestId('section-enrich-plugins')
    await expect(enrichSection).toBeVisible()
    await enrichSection.getByRole('button', { name: 'Ollama' }).click()

    const modelInput = page.getByTestId('input-enrich-ollama-model')
    await expect(modelInput).toBeVisible()
    await modelInput.fill('llama3.2')
    await page.getByTestId('btn-save-enrich').click()
    await expect(page.getByTestId('enrich-saved-msg')).toContainText('Saved')

    await page.reload()
    await page.locator('.app-layout').waitFor({ state: 'visible' })
    await page.getByTestId('tab-plugins').click()
    const enrichSectionAfter = page.getByTestId('section-enrich-plugins')
    await expect(enrichSectionAfter).toBeVisible()
    await expect(enrichSectionAfter.getByRole('button', { name: 'Ollama' })).toHaveClass(/active/)
    await expect(page.getByTestId('input-enrich-ollama-model')).toHaveValue('llama3.2')
  })
  ```

- [ ] **Step 2: Run the full suite**

  ```bash
  cd web && npm run test:e2e
  ```
  Expected output (approximate):
  ```
  Running 6 tests using 1 worker
  ✓ dashboard.spec.ts:4 loads and shows engram count (Xs)
  ✓ dashboard.spec.ts:13 navigation changes the active view (Xs)
  ✓ memories.spec.ts:7 creates a memory and finds it in the list (Xs)
  ✓ memories.spec.ts:26 searches for a memory by concept (Xs)
  ✓ settings.spec.ts:4 plugin config persists after page reload (#168 regression guard) (Xs)
  ✓ smoke.spec.ts:4 full happy path smoke test (Xs)

  6 passed (Xs)
  ```

- [ ] **Step 3: Run 3 times in a row to verify stability**

  ```bash
  cd web && npm run test:e2e && npm run test:e2e && npm run test:e2e
  ```
  Expected: `6 passed` all three times.

- [ ] **Step 4: Commit**

  ```bash
  git add web/e2e/smoke.spec.ts
  git commit -m "test(e2e): add smoke spec — full happy path"
  ```

---

## Chunk 4: CI Integration

### Task 9: Add E2E step to GitHub Actions

**Files:**
- Modify: `.github/workflows/ci.yml`

The workflow file is `.github/workflows/ci.yml`. It already uses `actions/setup-go@v5` (confirmed at lines 18, 69, 107, 206), so Go is available. The Playwright `webServer` command builds the binary using `go build` — no separate setup needed.

- [ ] **Step 1: Verify the workflow file exists**

  ```bash
  ls .github/workflows/ci.yml
  ```
  Expected: file exists.

- [ ] **Step 2: Add Playwright step after existing Go test step**

  In the workflow YAML, add these steps after the Go build/test steps:

  ```yaml
  - name: Install Node.js
    uses: actions/setup-node@v4
    with:
      node-version: '20'
      cache: 'npm'
      cache-dependency-path: web/package-lock.json

  - name: Install web dependencies
    run: cd web && npm ci

  - name: Install Playwright browsers
    run: cd web && npx playwright install chromium --with-deps

  - name: Run E2E tests
    run: cd web && npm run test:e2e

  - name: Upload Playwright traces on failure
    if: failure()
    uses: actions/upload-artifact@v4
    with:
      name: playwright-traces
      path: web/test-results/
      retention-days: 7
  ```

  **Note:** The `webServer` command in `playwright.config.ts` builds the Go binary fresh. The CI workflow must already have Go installed (it likely does for the existing Go tests). The binary is built into `/tmp/muninn-e2e` so no conflict with the existing build artifacts.

- [ ] **Step 3: Commit**

  ```bash
  git add .github/workflows/ci.yml
  git commit -m "ci: add Playwright E2E step to GitHub Actions"
  ```

---

## Final Verification

- [ ] Run full suite locally one more time from a clean state:
  ```bash
  cd web && npm run test:e2e
  ```
  Expected: `6 passed`.

- [ ] Run existing Vitest unit tests to ensure no regressions:
  ```bash
  cd web && npm test
  ```
  Expected: `15 passed` (existing plugin-config-utils tests).

- [ ] Run Go tests to ensure no regressions:
  ```bash
  go test ./... -count=1 -timeout 60s
  ```
  Expected: all pass (live integration tests skip without `MUNINN_TEST_TOKEN`).

- [ ] Create new branch and push PR:
  ```bash
  git checkout -b test/playwright-e2e
  git push -u origin test/playwright-e2e
  gh pr create --base develop --title "test(e2e): add Playwright E2E test suite" --body "Adds ironclad browser-level regression tests for the MuninnDB web UI. Covers dashboard load, memory CRUD, and plugin config persistence (direct guard against issue #168 regressing). Playwright manages the full server lifecycle — builds the binary and starts with a clean data directory each run."
  ```
