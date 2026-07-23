import { afterEach, describe, expect, it, vi } from "vitest";

interface PlaywrightConfigShape {
  use?: { baseURL?: string };
  webServer?: { port?: number };
}

const originalPort = process.env.AGENTSVIEW_E2E_PORT;

async function loadConfig(port: string | undefined): Promise<PlaywrightConfigShape> {
  vi.resetModules();
  if (port === undefined) {
    delete process.env.AGENTSVIEW_E2E_PORT;
  } else {
    process.env.AGENTSVIEW_E2E_PORT = port;
  }
  const loaded = await import("./playwright.config");
  return loaded.default as unknown as PlaywrightConfigShape;
}

afterEach(() => {
  if (originalPort === undefined) {
    delete process.env.AGENTSVIEW_E2E_PORT;
  } else {
    process.env.AGENTSVIEW_E2E_PORT = originalPort;
  }
  vi.resetModules();
});

describe("Playwright e2e port configuration", () => {
  it.each([undefined, ""])("defaults unset or empty port %s", async (port) => {
    const config = await loadConfig(port);

    expect(config.use?.baseURL).toBe("http://127.0.0.1:8090");
    expect(config.webServer?.port).toBe(8090);
  });

  it("uses a valid custom port", async () => {
    const config = await loadConfig("48123");

    expect(config.use?.baseURL).toBe("http://127.0.0.1:48123");
    expect(config.webServer?.port).toBe(48123);
  });

  it.each(["0", "-1", "65536", "1.5", "abc", " 80 "])(
    "rejects invalid port %s",
    async (port) => {
      await expect(loadConfig(port)).rejects.toThrow(
        /AGENTSVIEW_E2E_PORT must be an integer from 1 to 65535/,
      );
    },
  );
});
