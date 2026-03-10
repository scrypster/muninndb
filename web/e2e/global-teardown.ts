import { rmSync } from 'fs'
import { E2E_DATA_DIR } from '../playwright.config.js'

export default async function globalTeardown() {
  rmSync(E2E_DATA_DIR, { recursive: true, force: true })
}
