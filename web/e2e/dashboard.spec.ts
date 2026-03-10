import { test, expect } from './fixtures/auth.js'

test.describe('Dashboard', () => {
  test('loads without JS errors and shows engram count', async ({ page }) => {
    const errors: string[] = []
    page.on('pageerror', (err) => errors.push(err.message))

    await page.goto('/')
    await expect(page.getByTestId('stat-engram-count')).toBeVisible()

    expect(errors).toHaveLength(0)
  })

  test('sidebar navigation changes hash', async ({ page }) => {
    await page.goto('/')
    await page.locator('.sidebar-item').filter({ hasText: 'Memories' }).click()
    await expect(page).toHaveURL(/#\/memories/)
  })
})
