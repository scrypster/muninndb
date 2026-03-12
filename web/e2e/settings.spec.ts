import { test, expect } from './fixtures/auth.js'

test.describe('Settings: Plugin Config Persistence', () => {
  test('plugin config persists after page reload (#168 regression guard)', async ({ page }) => {
    // Navigate to settings → plugins
    await page.locator('.sidebar-item').filter({ hasText: 'Settings' }).click()
    const pluginsTab = page.getByTestId('tab-plugins')
    await expect(pluginsTab).toBeVisible()
    await pluginsTab.click()

    const enrichSection = page.getByTestId('section-enrich-plugins')
    await expect(enrichSection).toBeVisible()

    // Select Ollama as the enrich provider (label is "Ollama (local)")
    await enrichSection.getByRole('button', { name: 'Ollama' }).click()

    // Wait for the model input to appear (either text input or select dropdown)
    const modelInput = page.getByTestId('input-enrich-ollama-model')
    await expect(modelInput).toBeVisible()

    // Set a model value — works for both text input (no Ollama) and select (Ollama running)
    const isSelect = await modelInput.evaluate((el) => el.tagName === 'SELECT')
    if (isSelect) {
      await modelInput.selectOption({ index: 0 })
    } else {
      await modelInput.fill('llama3.2')
    }

    // Save
    await page.getByTestId('btn-save-enrich').click()
    await expect(page.locator('.toast.success').last()).toBeVisible()

    // Hard reload — the key assertion: config must survive this
    await page.reload()
    await page.locator('.app-layout').waitFor({ state: 'visible' })

    // Core regression guard for #168: the saved enrich config must persist to disk.
    // We verify via the API directly — the badge requires a server restart to appear,
    // but the config should be readable from GET /api/admin/plugin-config immediately.
    const cfg = await page.evaluate(async () => {
      const res = await fetch('/api/admin/plugin-config')
      return res.json()
    })
    expect(cfg.enrich_provider).toBe('ollama')
  })
})
