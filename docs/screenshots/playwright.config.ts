import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  retries: 0,
  use: {
    baseURL: 'http://127.0.0.1:8090',
    viewport: { width: 1440, height: 900 },
    colorScheme: 'dark',
    timezoneId: 'America/Chicago',
  },
  projects: [
    {
      name: 'screenshots',
      use: { browserName: 'chromium' },
    },
  ],
});
