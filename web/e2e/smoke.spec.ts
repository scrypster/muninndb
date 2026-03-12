import { test, expect } from './fixtures/auth.js'

test.describe('Smoke: Full Happy Path', () => {
  test('dashboard → create memory → search → settings persistence', async ({ page }) => {
    // 1. Dashboard loads, engram count stat is visible
    await page.goto('/')
    await expect(page.getByTestId('stat-engram-count')).toBeVisible()

    // 2. Navigate to memories and create one
    await page.locator('.sidebar-item').filter({ hasText: 'Memories' }).click()
    await expect(page.getByTestId('btn-new-memory')).toBeVisible()
    await page.getByTestId('btn-new-memory').click()
    await expect(page.getByTestId('input-concept')).toBeVisible()
    await page.getByTestId('input-concept').fill('smoke-test-concept')
    await page.getByTestId('input-content').fill('Smoke test memory content.')
    await page.getByTestId('btn-create-memory').click()
    await expect(page.getByTestId('input-concept')).not.toBeVisible()
    await expect(page.getByTestId('memory-item').first()).toContainText('smoke-test-concept')

    // 3. Search finds it
    await page.getByTestId('input-search').fill('smoke-test-concept')
    await page.keyboard.press('Enter')
    await expect(page.getByTestId('memory-item').first()).toContainText('smoke-test-concept')

    // 4. Plugin config persists after reload (#168 regression guard)
    await page.locator('.sidebar-item').filter({ hasText: 'Settings' }).click()
    await page.getByTestId('tab-plugins').click()
    const enrichSection = page.getByTestId('section-enrich-plugins')
    await expect(enrichSection).toBeVisible()
    await enrichSection.getByRole('button', { name: 'Ollama' }).click()

    const modelInput = page.getByTestId('input-enrich-ollama-model')
    await expect(modelInput).toBeVisible()
    const isSelect = await modelInput.evaluate((el) => el.tagName === 'SELECT')
    if (isSelect) {
      await modelInput.selectOption({ index: 0 })
    } else {
      await modelInput.fill('llama3.2')
    }

    await page.getByTestId('btn-save-enrich').click()
    await expect(page.locator('.toast.success').last()).toBeVisible()

    await page.reload()
    await page.locator('.app-layout').waitFor({ state: 'visible' })

    // Regression guard for #168: enrich config must persist to disk after reload.
    // Verified via API — badge requires server restart, but config must be readable now.
    const cfg = await page.evaluate(async () => {
      const res = await fetch('/api/admin/plugin-config')
      return res.json()
    })
    expect(cfg.enrich_provider).toBe('ollama')
  })
})
