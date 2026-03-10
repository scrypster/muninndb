import { test, expect } from './fixtures/auth.js'

test.describe('Memories', () => {
  test('create memory → appears in list → searchable', async ({ page }) => {
    // Navigate via sidebar (reliable Alpine event trigger)
    await page.locator('.sidebar-item').filter({ hasText: 'Memories' }).click()
    await expect(page.getByTestId('btn-new-memory')).toBeVisible()

    // Open new memory modal
    await page.getByTestId('btn-new-memory').click()
    await expect(page.getByTestId('input-concept')).toBeVisible()

    // Fill and submit
    await page.getByTestId('input-concept').fill('playwright-test-concept')
    await page.getByTestId('input-content').fill('This memory was written by the Playwright E2E test suite.')
    await page.getByTestId('btn-create-memory').click()

    // Modal closes, memory appears in list
    await expect(page.getByTestId('input-concept')).not.toBeVisible()
    await expect(page.getByTestId('memory-item').first()).toBeVisible()
    await expect(page.getByTestId('memory-item').first()).toContainText('playwright-test-concept')

    // Search finds it
    await page.getByTestId('input-search').fill('playwright-test-concept')
    await page.keyboard.press('Enter')
    await expect(page.getByTestId('memory-item').first()).toContainText('playwright-test-concept')
  })
})
