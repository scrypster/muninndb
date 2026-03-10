import { test, expect } from './fixtures/auth.js'

test.describe('Dashboard', () => {
  test('loads without JS errors and shows engram count', async ({ page }) => {
    const errors: string[] = []
    page.on('pageerror', (err) => errors.push(err.message))

    await page.goto('/')
    await expect(page.getByTestId('stat-engram-count')).toBeVisible()
    await expect(page.getByTestId('stat-engram-count')).toHaveText('0')

    expect(errors).toHaveLength(0)
  })

  test('navigation hash changes when clicking nav items', async ({ page }) => {
    await page.goto('/')
    await page.getByTestId('tab-plugins').click()
    await expect(page).toHaveURL(/#\/settings\/plugins/)
  })
})
