import { test, expect } from './fixtures/auth.js'

test.describe('Smoke: Full Happy Path', () => {
  test('dashboard → create memory → search → settings persistence', async ({ page }) => {
    // 1. Dashboard loads, engram count is 0
    await page.goto('/')
    await expect(page.getByTestId('stat-engram-count')).toBeVisible()
    await expect(page.getByTestId('stat-engram-count')).toHaveText('0')

    // 2. Create a memory
    await page.getByTestId('btn-new-memory').click()
    await expect(page.getByTestId('input-concept')).toBeVisible()
    await page.getByTestId('input-concept').fill('smoke-test-concept')
    await page.getByTestId('input-content').fill('Smoke test memory content.')
    await page.getByTestId('btn-create-memory').click()
    await expect(page.getByTestId('input-concept')).not.toBeVisible()
    await expect(page.getByTestId('memory-item').first()).toContainText('smoke-test-concept')
    await expect(page.getByTestId('stat-engram-count')).toHaveText('1')

    // 3. Search finds it
    await page.getByTestId('input-search').fill('smoke-test-concept')
    await page.keyboard.press('Enter')
    await expect(page.getByTestId('memory-item').first()).toContainText('smoke-test-concept')

    // 4. Plugin config persists after reload (#168 regression guard)
    await page.goto('/#/settings/plugins')
    await page.getByTestId('tab-plugins').click()
    const enrichSection = page.getByTestId('section-enrich-plugins')
    await expect(enrichSection).toBeVisible()
    await enrichSection.getByRole('button', { name: 'Ollama' }).click()
    const modelInput = page.getByTestId('input-enrich-ollama-model')
    await expect(modelInput).toBeVisible()
    await modelInput.fill('llama3.2')
    await page.getByTestId('btn-save-enrich').click()
    await expect(page.getByTestId('enrich-saved-msg')).toBeVisible()

    await page.reload()
    await page.locator('.app-layout').waitFor({ state: 'visible' })
    await page.getByTestId('tab-plugins').click()
    await expect(page.getByTestId('section-enrich-plugins').getByRole('button', { name: 'Ollama' })).toHaveClass(/active/)
    await expect(page.getByTestId('input-enrich-ollama-model')).toHaveValue('llama3.2')
  })
})
