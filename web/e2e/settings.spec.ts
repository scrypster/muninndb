import { test, expect } from './fixtures/auth.js'

test.describe('Settings: Plugin Config Persistence', () => {
  test('plugin config persists after page reload (#168 regression guard)', async ({ page }) => {
    await page.goto('/#/settings/plugins')

    const pluginsTab = page.getByTestId('tab-plugins')
    await expect(pluginsTab).toBeVisible()
    await pluginsTab.click()

    const enrichSection = page.getByTestId('section-enrich-plugins')
    await expect(enrichSection).toBeVisible()

    // Select Ollama as the enrich provider
    await enrichSection.getByRole('button', { name: 'Ollama' }).click()

    // Fill in model name (text fallback — no Ollama running in CI)
    const modelInput = page.getByTestId('input-enrich-ollama-model')
    await expect(modelInput).toBeVisible()
    await modelInput.fill('llama3.2')

    // Save
    await page.getByTestId('btn-save-enrich').click()
    await expect(page.getByTestId('enrich-saved-msg')).toBeVisible()
    await expect(page.getByTestId('enrich-saved-msg')).toContainText('Saved')

    // Hard reload
    await page.reload()
    await page.locator('.app-layout').waitFor({ state: 'visible' })

    // Navigate back to plugins tab
    await page.getByTestId('tab-plugins').click()

    const enrichSectionAfterReload = page.getByTestId('section-enrich-plugins')
    await expect(enrichSectionAfterReload).toBeVisible()

    // Ollama pill should still be active
    await expect(enrichSectionAfterReload.getByRole('button', { name: 'Ollama' })).toHaveClass(/active/)

    // Model value should be preserved
    await expect(page.getByTestId('input-enrich-ollama-model')).toHaveValue('llama3.2')
  })
})
