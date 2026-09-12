import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { SettingsService } from "../api/generated/index";
import { ApiError } from "../api/runtime.js";

let settings: typeof import("./settings.svelte.js").settings;
let ui: typeof import("./ui.svelte.js").ui;

const runtime = vi.hoisted(() => ({
  setAuthToken: vi.fn(),
  isRemoteConnection: vi.fn(),
}));

vi.mock("../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
    setAuthToken: runtime.setAuthToken,
    isRemoteConnection: runtime.isRemoteConnection,
  };
});

vi.mock("../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../api/generated/index")>();
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
  return new ApiError(status, message);
}

beforeEach(async () => {
  vi.resetModules();
  vi.clearAllMocks();
  localStorage.clear();
  ({ ui } = await import("./ui.svelte.js"));
  ({ settings } = await import("./settings.svelte.js"));
});

describe("SettingsStore.load mode handling", () => {
  it("records read-only mode from the settings response", async () => {
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
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

  it("keeps current metadata when a stale settings load succeeds", async () => {
    let finishFirst!: (value: Record<string, unknown>) => void;
    let finishSecond!: (value: Record<string, unknown>) => void;
    settingsService.getApiV1Settings
      .mockReturnValueOnce(
        new Promise((resolve) => {
          finishFirst = resolve;
        }),
      )
      .mockReturnValueOnce(
        new Promise((resolve) => {
          finishSecond = resolve;
        }),
      );

    const first = settings.load();
    const second = settings.load();
    finishSecond({
      agent_dirs: { current: ["/current"] },
      session_providers: [],
      disabled_agents: [],
      chart_palette: "agentsview",
      github_configured: true,
      host: "current.example",
      port: 9090,
      auth_token: "current-token",
      read_only: false,
      require_auth: true,
      terminal: { mode: "current" },
      tool_result_images: "drop",
      zoom_level: 150,
    });
    await second;
    finishFirst({
      agent_dirs: { stale: ["/stale"] },
      session_providers: [],
      disabled_agents: [],
      chart_palette: "matplotlib",
      github_configured: false,
      host: "stale.example",
      port: 7070,
      auth_token: "stale-token",
      read_only: true,
      require_auth: false,
      terminal: { mode: "stale" },
      tool_result_images: "keep",
      zoom_level: 120,
    });
    await first;

    expect(ui.zoomLevel).toBe(150);
    expect(settings.agentDirs).toEqual({ current: ["/current"] });
    expect(settings.chartPalette).toBe("agentsview");
    expect(settings.githubConfigured).toBe(true);
    expect(settings.host).toBe("current.example");
    expect(settings.port).toBe(9090);
    expect(settings.authToken).toBe("current-token");
    expect(settings.requireAuth).toBe(true);
    expect(settings.readOnly).toBe(false);
    expect(settings.terminal).toEqual({ mode: "current" });
    expect(settings.toolResultImages).toBe("drop");
    expect(settings.error).toBeNull();
    expect(settings.needsAuth).toBe(false);
    expect(settings.loading).toBe(false);
    expect(settings.loaded).toBe(true);
    expect(runtime.setAuthToken).toHaveBeenCalledWith("current-token");
    expect(runtime.setAuthToken).not.toHaveBeenCalledWith("stale-token");
  });

  it("ignores a stale settings success before validation", async () => {
    let finishFirst!: (value: Record<string, unknown>) => void;
    let finishSecond!: (value: Record<string, unknown>) => void;
    settingsService.getApiV1Settings
      .mockReturnValueOnce(
        new Promise((resolve) => {
          finishFirst = resolve;
        }),
      )
      .mockReturnValueOnce(
        new Promise((resolve) => {
          finishSecond = resolve;
        }),
      );

    const first = settings.load();
    const second = settings.load();
    finishSecond({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "current.example",
      port: 9090,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    await second;
    finishFirst({
      agent_dirs: {},
      chart_palette: "obsolete",
      github_configured: false,
      host: "stale.example",
      port: 7070,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    await first;

    expect(settings.error).toBeNull();
    expect(settings.loaded).toBe(true);
    expect(settings.loading).toBe(false);
  });

  it.each([
    ["generic error", new Error("stale load failed")],
    ["401", apiError(401, "Unauthorized")],
    ["403", apiError(403, "Forbidden")],
  ])("ignores a stale settings %s", async (_name, staleError) => {
    let rejectFirst!: (error: unknown) => void;
    const currentResponse = {
      agent_dirs: { current: ["/current"] },
      chart_palette: "agentsview" as const,
      github_configured: true,
      host: "current.example",
      port: 9090,
      read_only: false,
      require_auth: false,
      terminal: { mode: "current" },
      zoom_level: 100,
    };
    settingsService.getApiV1Settings
      .mockReturnValueOnce(
        new Promise((_, reject) => {
          rejectFirst = reject;
        }),
      )
      .mockResolvedValueOnce(currentResponse);

    const first = settings.load();
    const second = settings.load();
    await second;
    rejectFirst(staleError);
    await first;

    expect(settings.agentDirs).toEqual({ current: ["/current"] });
    expect(settings.readOnly).toBe(false);
    expect(settings.error).toBeNull();
    expect(settings.needsAuth).toBe(false);
    expect(settings.loading).toBe(false);
    expect(settings.loaded).toBe(true);
  });

  it("keeps the current load pending when a stale load finishes", async () => {
    let rejectFirst!: (error: unknown) => void;
    let finishSecond!: (value: Record<string, unknown>) => void;
    settingsService.getApiV1Settings
      .mockReturnValueOnce(
        new Promise((_, reject) => {
          rejectFirst = reject;
        }),
      )
      .mockReturnValueOnce(
        new Promise((resolve) => {
          finishSecond = resolve;
        }),
      );

    const first = settings.load();
    const second = settings.load();
    rejectFirst(new Error("stale load failed"));
    await first;

    expect(settings.loading).toBe(true);
    expect(settings.loaded).toBe(false);
    expect(settings.error).toBeNull();
    expect(settings.needsAuth).toBe(false);
    expect(settingsService.putApiV1Settings).not.toHaveBeenCalled();

    ui.setZoomLevel(120);
    expect(settingsService.putApiV1Settings).not.toHaveBeenCalled();
    finishSecond({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    await second;

    expect(settings.loading).toBe(false);
    expect(settings.loaded).toBe(true);
    expect(settingsService.putApiV1Settings).not.toHaveBeenCalled();
  });

  it("does not let a pending load replace a newer user zoom", async () => {
    settings.readOnly = true;
    let finish!: (value: Record<string, unknown>) => void;
    settingsService.getApiV1Settings.mockReturnValue(
      new Promise((resolve) => {
        finish = resolve;
      }),
    );

    const loading = settings.load();
    ui.setZoomLevel(150);
    finish({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: true,
      require_auth: false,
      terminal: { mode: "auto" },
      zoom_level: 120,
    });
    await loading;

    expect(ui.zoomLevel).toBe(150);
    expect(settingsService.putApiV1Settings).not.toHaveBeenCalled();
  });
});

describe("SettingsStore zoom default", () => {
  const response = {
    agent_dirs: {},
    chart_palette: "agentsview",
    github_configured: false,
    host: "127.0.0.1",
    port: 8080,
    read_only: false,
    require_auth: false,
    terminal: { mode: "auto" },
    zoom_level: 120,
  };

  it("uses the configured default without storing a local override", async () => {
    settingsService.getApiV1Settings.mockResolvedValue(response);
    await settings.load();
    expect(ui.zoomLevel).toBe(120);
    expect(localStorage.getItem("agentsview-zoom-level")).toBeNull();
    expect(settingsService.putApiV1Settings).not.toHaveBeenCalled();
  });

  it.each([100, 150])("keeps local %i when settings reload", async (level) => {
    settingsService.getApiV1Settings.mockResolvedValue(response);
    await settings.load();
    ui.setZoomLevel(level);
    await settings.load();
    expect(ui.zoomLevel).toBe(level);
    expect(localStorage.getItem("agentsview-zoom-level")).toBe(String(level));
    expect(settingsService.putApiV1Settings).not.toHaveBeenCalled();
  });
});

describe("SettingsStore chart palette", () => {
  it("loads the server chart palette", async () => {
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "matplotlib",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });

    await settings.load();

    expect(settings.chartPalette).toBe("matplotlib");
  });

  it("keeps the confirmed palette when saving fails", async () => {
    settings.chartPalette = "agentsview";
    settingsService.putApiV1Settings.mockRejectedValue(new Error("save failed"));

    await settings.save({ chart_palette: "matplotlib" });

    expect(settings.chartPalette).toBe("agentsview");
    expect(settings.saveError).toBe("save failed");
  });
});

describe("SettingsStore session providers", () => {
  const response = {
    agent_dirs: {},
    session_providers: [
      {
        id: "claude",
        display_name: "Claude Code",
        dirs: ["/sessions/claude"],
      },
      {
        id: "gemini",
        display_name: "Gemini",
        dirs: ["/sessions/gemini"],
      },
    ],
    disabled_agents: ["gemini"],
    chart_palette: "agentsview",
    github_configured: false,
    host: "127.0.0.1",
    port: 8080,
    read_only: false,
    require_auth: false,
    terminal: { mode: "auto" },
  };

  it("loads ordered provider metadata and disabled state", async () => {
    settingsService.getApiV1Settings.mockResolvedValue(response);

    await settings.load();

    expect(settings.sessionProviders).toEqual(response.session_providers);
    expect(settings.disabledAgents).toEqual(["gemini"]);
  });

  it("returns true and replaces confirmed state after a successful save", async () => {
    settings.disabledAgents = ["gemini"];
    settingsService.putApiV1Settings.mockResolvedValue({
      ...response,
      disabled_agents: [],
    });

    const saved = await settings.save({ disabled_agents: [] });

    expect(saved).toBe(true);
    expect(settings.disabledAgents).toEqual([]);
  });

  it("returns false without replacing confirmed state after a failed save", async () => {
    settings.disabledAgents = ["gemini"];
    settingsService.putApiV1Settings.mockRejectedValue(new Error("save failed"));

    const saved = await settings.save({ disabled_agents: [] });

    expect(saved).toBe(false);
    expect(settings.disabledAgents).toEqual(["gemini"]);
    expect(settings.saveError).toBe("save failed");
  });

  it("serializes saves and keeps saving true until the queue drains", async () => {
    let finishFirst!: (value: typeof response) => void;
    settingsService.putApiV1Settings
      .mockReturnValueOnce(
        new Promise((resolve) => {
          finishFirst = resolve;
        }),
      )
      .mockResolvedValueOnce({
        ...response,
        disabled_agents: [],
        chart_palette: "matplotlib",
      });

    const first = settings.save({ disabled_agents: [] });
    const second = settings.save({ chart_palette: "matplotlib" });

    expect(settingsService.putApiV1Settings).toHaveBeenCalledTimes(1);
    expect(settings.saving).toBe(true);

    finishFirst({ ...response, disabled_agents: [] });
    await first;
    expect(settingsService.putApiV1Settings).toHaveBeenCalledTimes(2);
    expect(settings.saving).toBe(true);

    await second;
    expect(settings.saving).toBe(false);
    expect(settings.disabledAgents).toEqual([]);
    expect(settings.chartPalette).toBe("matplotlib");
  });

  it("serializes custom server-backed mutations with settings saves", async () => {
    let finishCustom!: () => void;
    const custom = settings.runMutation(
      () =>
        new Promise<void>((resolve) => {
          finishCustom = resolve;
        }),
    );
    settingsService.putApiV1Settings.mockResolvedValue({
      ...response,
      disabled_agents: [],
    });

    const save = settings.save({ disabled_agents: [] });

    expect(settingsService.putApiV1Settings).not.toHaveBeenCalled();
    expect(settings.saving).toBe(true);

    finishCustom();
    await custom;
    expect(settingsService.putApiV1Settings).toHaveBeenCalledTimes(1);
    expect(settings.saving).toBe(true);

    await save;
    expect(settings.saving).toBe(false);
  });

  it("continues queued settings saves after a custom mutation fails", async () => {
    let failCustom!: (error: Error) => void;
    const custom = settings.runMutation(
      () =>
        new Promise<void>((_, reject) => {
          failCustom = reject;
        }),
    );
    settingsService.putApiV1Settings.mockResolvedValue({
      ...response,
      disabled_agents: [],
    });

    const save = settings.save({ disabled_agents: [] });

    expect(settingsService.putApiV1Settings).not.toHaveBeenCalled();

    failCustom(new Error("custom failed"));
    await expect(custom).rejects.toThrow("custom failed");
    expect(settingsService.putApiV1Settings).toHaveBeenCalledTimes(1);

    await save;
    expect(settings.saving).toBe(false);
  });
});

describe("SettingsStore.load auth handling", () => {
  it("prompts for a token on 401 responses", async () => {
    settingsService.getApiV1Settings.mockRejectedValue(apiError(401, "Unauthorized"));

    await settings.load();

    expect(settings.needsAuth).toBe(true);
    expect(settings.error).toBeNull();
  });

  it("surfaces an actionable hint on a bare 403", async () => {
    settingsService.getApiV1Settings.mockRejectedValue(apiError(403, "Forbidden"));

    await settings.load();

    expect(settings.needsAuth).toBe(false);
    expect(settings.error).toContain("--public-url");
  });

  it("preserves a descriptive 403 body from the server", async () => {
    const detail =
      'Forbidden: request Host "127.0.0.1:18080" is not in the ' +
      "allowed set [127.0.0.1:8080 localhost:8080]. restart with " +
      "--public-url http://127.0.0.1:18080.";
    settingsService.getApiV1Settings.mockRejectedValue(apiError(403, detail));

    await settings.load();

    expect(settings.needsAuth).toBe(false);
    expect(settings.error).toBe(detail);
  });
});
