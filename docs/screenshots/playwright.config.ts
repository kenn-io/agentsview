import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  retries: 0,
  workers: 1,
  maxFailures: 1,
  outputDir: '/result/playwright-results',
  use: {
    baseURL: 'http://127.0.0.1:8090',
    viewport: { width: 1440, height: 900 },
    colorScheme: 'dark',
    locale: 'en-US',
    deviceScaleFactor: 1,
    reducedMotion: 'reduce',
    timezoneId: process.env.SCREENSHOT_TIMEZONE,
  },
  projects: [
    {
      name: 'screenshots',
      use: { browserName: 'chromium' },
    },
  ],
});
