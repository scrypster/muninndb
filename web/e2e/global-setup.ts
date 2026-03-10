import { request, FullConfig } from '@playwright/test'

export default async function globalSetup(config: FullConfig) {
  const { baseURL } = config.projects[0].use
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
