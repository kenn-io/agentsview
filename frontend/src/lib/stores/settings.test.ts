import {
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";
import { settings } from "./settings.svelte.js";
import {
  ApiError,
  SettingsService,
} from "../api/generated/index";

const runtime = vi.hoisted(() => ({
  setAuthToken: vi.fn(),
  isRemoteConnection: vi.fn(),
}));

vi.mock("../api/runtime.js", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../api/runtime.js")>();
  return {
    ...orig,
    configureGeneratedClient: vi.fn(),
    callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
    setAuthToken: runtime.setAuthToken,
    isRemoteConnection: runtime.isRemoteConnection,
  };
});

vi.mock("../api/generated/index", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../api/generated/index")>();
  return {
    ...orig,
    SettingsService: {
      getApiV1Settings: vi.fn(),
      putApiV1Settings: vi.fn(),
    },
  };
});

const settingsService = SettingsService as unknown as {
  getApiV1Settings: ReturnType<typeof vi.fn>;
  putApiV1Settings: ReturnType<typeof vi.fn>;
};

function apiError(status: number, message: string): ApiError {
  return new ApiError(
    { method: "GET", url: "/api/v1/settings" },
    {
      url: "/api/v1/settings",
      ok: false,
      status,
      statusText: message,
      body: message,
    },
    message,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  settings.agentDirs = {};
  settings.githubConfigured = false;
  settings.terminal = { mode: "auto" };
  settings.host = "";
  settings.port = 0;
  settings.authToken = "";
  settings.requireAuth = false;
  settings.readOnly = false;
  settings.loaded = false;
  settings.loading = false;
  settings.saving = false;
  settings.error = null;
  settings.needsAuth = false;
});

describe("SettingsStore.load mode handling", () => {
  it("records read-only mode from the settings response", async () => {
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: true,
      require_auth: false,
      terminal: { mode: "auto" },
    });

    await settings.load();

    expect(settings.readOnly).toBe(true);
  });
});

describe("SettingsStore.load auth handling", () => {
  it("prompts for a token on 401 responses", async () => {
    settingsService.getApiV1Settings.mockRejectedValue(
      apiError(401, "Unauthorized"),
    );

    await settings.load();

    expect(settings.needsAuth).toBe(true);
    expect(settings.error).toBeNull();
  });

  it("surfaces an actionable hint on a bare 403", async () => {
    settingsService.getApiV1Settings.mockRejectedValue(
      apiError(403, "Forbidden"),
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
    settingsService.getApiV1Settings.mockRejectedValue(
      apiError(403, detail),
    );

    await settings.load();

    expect(settings.needsAuth).toBe(false);
    expect(settings.error).toBe(detail);
  });
});
