import { defineConfig, devices } from '@playwright/test'
import path from 'path'
import { fileURLToPath } from 'url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const projectRoot = path.join(__dirname, '..')

export const E2E_BINARY = '/tmp/muninn-e2e'
export const E2E_DATA_DIR = '/tmp/muninn-e2e-data'

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  timeout: 30_000,
  reporter: 'list',
  expect: { timeout: 10_000 },
  globalSetup: './e2e/global-setup.ts',
  globalTeardown: './e2e/global-teardown.ts',
  use: {
    baseURL: 'http://localhost:8476',
    storageState: './e2e/.auth.json',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: `cd ${projectRoot} && go build -o ${E2E_BINARY} ./cmd/muninn && rm -rf ${E2E_DATA_DIR} && ${E2E_BINARY} --daemon --data ${E2E_DATA_DIR}`,
    url: 'http://localhost:8476/',
    timeout: 120_000,
    reuseExistingServer: false,
  },
})
