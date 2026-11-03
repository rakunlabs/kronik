import { defineConfig } from '@playwright/test'

// End-to-end tests run against a kronik that is already up: KRONIK_E2E_URL
// (default http://127.0.0.1:18181/). Every request carries X-User, as the
// reverse proxy would send it.
export default defineConfig({
  testDir: 'e2e',
  timeout: 30_000,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: process.env.KRONIK_E2E_URL ?? 'http://127.0.0.1:18181/',
    extraHTTPHeaders: { 'X-User': 'e2e' },
    channel: process.env.PLAYWRIGHT_CHANNEL || undefined,
    trace: 'retain-on-failure',
  },
})
