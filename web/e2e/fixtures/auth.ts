import { test as base, expect } from '@playwright/test'

export { expect }

export const test = base.extend({
  page: async ({ page }, use) => {
    await page.goto('/')
    await page.locator('.app-layout').waitFor({ state: 'visible' })
    await use(page)
  },
})
