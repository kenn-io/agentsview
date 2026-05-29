import {
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";
import { settings } from "./settings.svelte.js";
import * as api from "../api/client.js";
import { ApiError } from "../api/client.js";

vi.mock("../api/client.js", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../api/client.js")>();
  return {
    ...orig,
    getSettings: vi.fn(),
    updateSettings: vi.fn(),
    setAuthToken: vi.fn(),
    isRemoteConnection: vi.fn(),
  };
});

beforeEach(() => {
  vi.clearAllMocks();
  settings.agentDirs = {};
  settings.githubConfigured = false;
  settings.terminal = { mode: "auto" };
  settings.host = "";
  settings.port = 0;
  settings.authToken = "";
  settings.requireAuth = false;
  settings.loading = false;
  settings.saving = false;
  settings.error = null;
  settings.needsAuth = false;
});

describe("SettingsStore.load auth handling", () => {
  it("prompts for a token on 401 responses", async () => {
    vi.mocked(api.getSettings).mockRejectedValue(
      new ApiError(401, "Unauthorized"),
    );

    await settings.load();

    expect(settings.needsAuth).toBe(true);
    expect(settings.error).toBeNull();
  });

  it("surfaces an actionable hint on a bare 403", async () => {
    vi.mocked(api.getSettings).mockRejectedValue(
      new ApiError(403, "Forbidden"),
    );

    await settings.load();

    expect(settings.needsAuth).toBe(false);
    expect(settings.error).toContain("--public-url");
  });

  it("preserves a descriptive 403 body from the server", async () => {
    const detail =
      'Forbidden: request Host "127.0.0.1:18080" is not in the ' +
      "allowed set [127.0.0.1:8080 localhost:8080]. restart with " +
      "--public-url http://127.0.0.1:18080.";
    vi.mocked(api.getSettings).mockRejectedValue(
      new ApiError(403, detail),
    );

    await settings.load();

    expect(settings.needsAuth).toBe(false);
    expect(settings.error).toBe(detail);
  });
});
