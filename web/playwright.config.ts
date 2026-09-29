import { defineConfig } from '@playwright/test'

// Driven by test/e2e/browser_windows_test.go, which starts a real Hub and
// passes its address and secrets through the environment.
export default defineConfig({
  testDir: './tests',
  testIgnore: 'unit/**', // node:test unit tests (npm run test:unit)
  timeout: 120_000,
  expect: { timeout: 15_000 },
  retries: 0,
  workers: 1,
  reporter: [['line']],
  use: {
    baseURL: process.env.TH_URL,
    channel: process.env.PLAYWRIGHT_CHANNEL, // use an installed browser when supplied by the test runner
    ignoreHTTPSErrors: true, // the Hub's LAN certificate is self-signed
    launchOptions: { args: ['--ignore-certificate-errors'] }, // so the service worker can register too
    viewport: { width: 1280, height: 800 },
    locale: 'zh-CN',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
})
