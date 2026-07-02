import { defineConfig } from "@playwright/test";

const isCI = process.env.CI === "true";

export default defineConfig({
  testDir: "e2e",
  timeout: isCI ? 45_000 : 20_000,
  retries: 0,
  use: {
    baseURL: "http://127.0.0.1:8090",
    headless: true,
    // Wide enough that the kit-ui TopBar renders its expanded tab row (at
    // Playwright's 1280px default the eight tabs collapse into the nav
    // SelectDropdown), so navigation specs can drive the tabs directly.
    viewport: { width: 1600, height: 900 },
  },
  projects: [
    {
      name: "chromium",
      use: { browserName: "chromium" },
    },
    {
      name: "webkit",
      use: { browserName: "webkit" },
    },
  ],
  webServer: {
    command: "bash ../scripts/e2e-server.sh",
    port: 8090,
    reuseExistingServer: false,
    timeout: 30_000,
  },
});
