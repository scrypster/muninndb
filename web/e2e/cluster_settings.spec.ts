import { test, expect } from './fixtures/auth.js'

test.describe('Cluster Settings: Persistence (#175 regression guard)', () => {
  test('all four cluster settings fields persist after save via API', async ({ page }) => {
    // Verify the settings API returns the expected defaults first.
    const defaults = await page.evaluate(async () => {
      const res = await fetch('/api/admin/cluster/settings')
      return res.json()
    })
    expect(typeof defaults.heartbeat_ms).toBe('number')
    expect(typeof defaults.sdown_beats).toBe('number')
    expect(typeof defaults.ccs_interval_seconds).toBe('number')
    expect(typeof defaults.reconcile_on_heal).toBe('boolean')

    // Save new values for all four fields via the API directly.
    const newSettings = {
      heartbeat_ms: 750,
      sdown_beats: 5,
      ccs_interval_seconds: 60,
      reconcile_on_heal: false,
    }
    const saveResp = await page.evaluate(async (settings) => {
      const res = await fetch('/api/admin/cluster/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify(settings),
      })
      return { status: res.status, body: await res.json() }
    }, newSettings)
    expect(saveResp.status).toBe(200)
    expect(saveResp.body.saved).toBe(true)

    // Read back from the GET endpoint — this is the core #175 regression guard.
    // Before the fix, only heartbeat_ms was persisted; the other three were silently dropped.
    const saved = await page.evaluate(async () => {
      const res = await fetch('/api/admin/cluster/settings')
      return res.json()
    })
    expect(saved.heartbeat_ms).toBe(750)
    expect(saved.sdown_beats).toBe(5)          // failed before #175 fix
    expect(saved.ccs_interval_seconds).toBe(60) // failed before #175 fix
    expect(saved.reconcile_on_heal).toBe(false) // failed before #175 fix
  })

  test('GET settings endpoint reflects previously saved values', async ({ page }) => {
    // Save known values, then GET and verify — end-to-end round-trip via HTTP.
    await page.evaluate(async () => {
      await fetch('/api/admin/cluster/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify({ heartbeat_ms: 900, sdown_beats: 4, ccs_interval_seconds: 20, reconcile_on_heal: true }),
      })
    })
    const got = await page.evaluate(async () => {
      const res = await fetch('/api/admin/cluster/settings', { credentials: 'same-origin' })
      return res.json()
    })
    expect(got.heartbeat_ms).toBe(900)
    expect(got.sdown_beats).toBe(4)
    expect(got.ccs_interval_seconds).toBe(20)
    expect(got.reconcile_on_heal).toBe(true)
  })

  test('cluster settings form fields are present and interact', async ({ page }) => {
    // Navigate to Settings → Cluster tab (if visible).
    // The cluster tab is only shown when cluster mode is enabled, so this test
    // just checks that the form elements exist when the cluster section is shown.
    // We navigate directly to verify the testids are wired correctly.
    await page.goto('/')
    await page.locator('.sidebar-item').filter({ hasText: 'Settings' }).click()

    // If cluster is disabled the form is hidden — skip UI interaction in that case.
    const clusterSection = page.locator('[data-testid="btn-save-cluster-settings"]')
    const isVisible = await clusterSection.isVisible()
    if (!isVisible) {
      test.skip()
      return
    }

    // When visible, verify all four inputs have the expected testids.
    await expect(page.getByTestId('input-cluster-heartbeat-ms')).toBeVisible()
    await expect(page.getByTestId('input-cluster-sdown-beats')).toBeVisible()
    await expect(page.getByTestId('input-cluster-ccs-interval')).toBeVisible()
    await expect(page.getByTestId('toggle-cluster-reconcile-heal')).toBeVisible()
  })

  test('cluster settings form loads server values when tab opens', async ({ page }) => {
    // Pre-seed known values via API.
    await page.evaluate(async () => {
      await fetch('/api/admin/cluster/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify({ heartbeat_ms: 1234, sdown_beats: 6, ccs_interval_seconds: 55, reconcile_on_heal: false }),
      })
    })

    await page.goto('/')
    await page.locator('.sidebar-item').filter({ hasText: 'Settings' }).click()

    // If cluster is disabled the form is hidden — skip.
    const saveBtn = page.locator('[data-testid="btn-save-cluster-settings"]')
    if (!(await saveBtn.isVisible())) { test.skip(); return }

    // Switch to cluster settings sub-tab — this should trigger loadClusterSettings().
    await page.locator('button', { hasText: 'Settings' }).filter({ hasNot: page.locator('.sidebar-item') }).click()

    // Wait briefly for the async fetch to complete.
    await page.waitForTimeout(300)

    // The heartbeat_ms input should reflect the server-side value, not the hardcoded default.
    const heartbeatInput = page.getByTestId('input-cluster-heartbeat-ms')
    await expect(heartbeatInput).toHaveValue('1234')
  })

  test('cluster settings form save → reload → persisted (#175 UI regression guard)', async ({ page }) => {
    await page.goto('/')
    await page.locator('.sidebar-item').filter({ hasText: 'Settings' }).click()

    // Skip when cluster is not enabled.
    const saveBtn = page.locator('[data-testid="btn-save-cluster-settings"]')
    if (!(await saveBtn.isVisible())) { test.skip(); return }

    // Fill in all four fields and save.
    await page.getByTestId('input-cluster-heartbeat-ms').fill('850')
    await page.getByTestId('input-cluster-sdown-beats').fill('8')
    await page.getByTestId('input-cluster-ccs-interval').fill('35')
    await saveBtn.click()

    // Saved message should appear.
    await expect(page.getByTestId('cluster-settings-saved-msg')).toBeVisible()

    // Reload and return to the same sub-tab.
    await page.reload()
    await page.locator('.sidebar-item').filter({ hasText: 'Settings' }).click()
    if (!(await saveBtn.isVisible())) { test.skip(); return }
    await page.locator('button', { hasText: 'Settings' }).filter({ hasNot: page.locator('.sidebar-item') }).click()
    await page.waitForTimeout(300)

    // Values should match what was saved — not the hardcoded defaults.
    await expect(page.getByTestId('input-cluster-heartbeat-ms')).toHaveValue('850')
    await expect(page.getByTestId('input-cluster-sdown-beats')).toHaveValue('8')
    await expect(page.getByTestId('input-cluster-ccs-interval')).toHaveValue('35')
  })
})
