import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { DaemonService, HealthService, SettingsService } from "../api/generated/index";
import { ApiError, setServerUrl } from "../api/runtime.js";

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
    DaemonService: { postApiV1DaemonRestart: vi.fn() },
    HealthService: { getApiPing: vi.fn() },
  };
});

const restartDaemon = vi.mocked(DaemonService.postApiV1DaemonRestart);
const ping = vi.mocked(HealthService.getApiPing);

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
  sessionStorage.clear();
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

  it("seeds the insight agent with the configured default", async () => {
    const { insights } = await import("./insights.svelte.js");
    insights.agent = "claude";
    insights.agentChosen = false;
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
      insight_default_agent: "codex",
    });

    await settings.load();

    expect(insights.agent).toBe("codex");
  });

  it("clears the insight agent choice before loading a different server", async () => {
    const { insights } = await import("./insights.svelte.js");
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      terminal: { mode: "auto" },
      insight_default_agent: "codex",
    });
    setServerUrl("https://example.com");
    await settings.load();
    insights.setAgent("kiro");
    await settings.load();
    expect(insights.agent).toBe("kiro");

    let finishLoad!: (value: unknown) => void;
    settingsService.getApiV1Settings.mockReturnValueOnce(
      new Promise((resolve) => {
        finishLoad = resolve;
      }),
    );
    setServerUrl("");
    const load = settings.load();
    expect(insights.agent).toBe("claude");
    expect(insights.agentChosen).toBe(false);
    finishLoad({
      agent_dirs: {},
      chart_palette: "agentsview",
      terminal: { mode: "auto" },
      insight_default_agent: "gemini",
    });
    await load;
    expect(insights.agent).toBe("gemini");
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

describe("SettingsStore.save", () => {
  it("reports a saved image policy when restart status is missing", async () => {
    settingsService.putApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      tool_result_images: "drop",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    expect(await settings.save({ tool_result_images: "drop" })).toBe(false);
    expect(settings.toolResultImages).toBe("drop");
    expect(settings.saveError).toMatch(/saved/i);
    expect(settings.saveError).toMatch(/update|restart/i);
  });
});

describe("SettingsStore.restartDaemon", () => {
  let reload: ReturnType<typeof vi.fn>;
  const originalLocation = window.location;

  beforeEach(() => {
    vi.useFakeTimers();
    reload = vi.fn();
    Object.defineProperty(window, "location", { value: { reload }, writable: true });
  });

  afterEach(() => {
    vi.useRealTimers();
    Object.defineProperty(window, "location", { value: originalLocation, writable: true });
  });

  it("reads the pending restart from the settings response", async () => {
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      restart_required: true,
      restart_supported: true,
      terminal: { mode: "auto" },
    });

    await settings.load();

    expect(settings.restartRequired).toBe(true);
    expect(settings.restartSupported).toBe(true);
  });

  it.each([false, true])(
    "sends restart to its original server with queued save %s",
    async (queued) => {
      const generated =
        await vi.importActual<typeof import("../api/generated/index")>("../api/generated/index");
      ping.mockImplementation((options) => generated.HealthService.getApiPing(options));
      restartDaemon.mockImplementation((options) =>
        generated.DaemonService.postApiV1DaemonRestart(options),
      );
      setServerUrl("https://first.example");
      localStorage.setItem("agentsview-auth-token::https://first.example", "first-token");
      localStorage.setItem("agentsview-auth-token::https://second.example", "second-token");
      let finishPing!: (response: Response) => void;
      const calls: { url: string; auth: string | null }[] = [];
      const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (url, options) => {
        calls.push({ url: String(url), auth: new Headers(options?.headers).get("Authorization") });
        if (calls.length === 1) return new Promise((resolve) => (finishPing = resolve));
        return new Response(
          JSON.stringify(calls.length === 2 ? { restarting: true } : { instance_id: "new" }),
          {
            status: 200,
            headers: { "Content-Type": "application/json" },
          },
        );
      });
      try {
        let finishSave!: () => void;
        const save = queued
          ? settings.runMutation(() => new Promise<void>((resolve) => (finishSave = resolve)))
          : null;
        const restart = settings.restartDaemon();
        if (save) {
          setServerUrl("https://second.example");
          finishSave();
          await save;
        }
        expect(finishPing).toBeDefined();
        setServerUrl("https://second.example");
        finishPing(
          new Response(JSON.stringify({ instance_id: "old" }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
        );
        await vi.advanceTimersByTimeAsync(1_000);
        await restart;
        expect(calls.map((call) => call.url)).toEqual([
          "https://first.example/api/ping",
          "https://first.example/api/v1/daemon/restart",
          "https://first.example/api/ping",
        ]);
        expect(calls.map((call) => call.auth)).toEqual(Array(3).fill("Bearer first-token"));
      } finally {
        fetchSpy.mockRestore();
      }
    },
  );

  it("checks the original server after switching servers during recovery", async () => {
    setServerUrl("https://first.example");
    localStorage.setItem("agentsview-auth-token::https://first.example", "first-token");
    localStorage.setItem("agentsview-auth-token::https://second.example", "second-token");
    ping.mockResolvedValue({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    restartDaemon.mockResolvedValue({ restarting: true });
    const restart = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(30_000);
    await restart;
    expect(settings.restartFailed).toBe(true);
    setServerUrl("https://second.example");
    ping.mockResolvedValue({ healthy: true, ok: true, pid: 100, instance_id: "new" });
    const check = settings.checkConnection();
    await vi.advanceTimersByTimeAsync(1_000);
    await check;
    expect(ping.mock.calls.at(-1)?.[0]).toMatchObject({
      baseUrl: "https://first.example",
      headers: { Authorization: "Bearer first-token" },
    });
  });

  it("clears restart recovery when settings are loaded for another server", async () => {
    setServerUrl("https://first.example");
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      terminal: { mode: "auto" },
      restart_required: true,
      restart_supported: true,
    });
    await settings.load();
    ping.mockResolvedValue({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    restartDaemon.mockResolvedValue({ restarting: true });
    const restart = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(30_000);
    await restart;
    expect(settings.restartFailed).toBe(true);
    setServerUrl("https://second.example");
    await settings.load();
    expect(settings.restartFailed).toBe(false);
    expect(settings.restartSupported).toBe(true);
    expect(settings.restartRequired).toBe(true);
  });

  it("queues restart behind saves and reconnects after a lost response", async () => {
    let finishSave!: () => void;
    const save = settings.runMutation(() => new Promise<void>((resolve) => (finishSave = resolve)));
    ping
      .mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "old" })
      .mockRejectedValueOnce(new TypeError("Failed to fetch"))
      .mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "old" })
      .mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "new" });
    // The old process can drop the connection before it answers.
    restartDaemon.mockRejectedValue(new TypeError("Failed to fetch"));

    const restart = settings.restartDaemon();
    void settings.restartDaemon();
    expect(settings.restarting).toBe(true);
    expect(restartDaemon).not.toHaveBeenCalled();

    finishSave();
    await save;
    await vi.advanceTimersByTimeAsync(3_000);
    await restart;

    expect(restartDaemon).toHaveBeenCalledOnce();
    expect(reload).toHaveBeenCalledOnce();
    expect(settings.restarting).toBe(true);
    expect(settings.restartFailed).toBe(false);
  });

  it("releases the settings queue when the first request stalls", async () => {
    ping.mockImplementation(
      (options) =>
        new Promise((_, reject) =>
          options?.signal?.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          ),
        ),
    );

    const restart = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(30_000);
    await restart;

    expect(restartDaemon).not.toHaveBeenCalled();
    expect(settings.saving).toBe(false);
    expect(settings.restarting).toBe(false);
    expect(settings.restartFailed).toBe(true);
    expect(settings.restartError).toContain("identify");
    ping.mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    ping.mockResolvedValue({ healthy: true, ok: true, pid: 100, instance_id: "new" });
    restartDaemon.mockResolvedValue({ restarting: true });
    const retry = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(1_000);
    await retry;
    expect(restartDaemon).toHaveBeenCalledOnce();
    expect(reload).toHaveBeenCalledOnce();
  });

  it("reports the server's reason without polling when it refuses", async () => {
    ping.mockResolvedValue({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    restartDaemon.mockRejectedValue(new ApiError(500, "restart failed"));

    await settings.restartDaemon();

    expect(ping).toHaveBeenCalledOnce();
    expect(settings.saving).toBe(false);
    expect(settings.restartFailed).toBe(true);
    expect(settings.restartError).toBe("restart failed");
  });

  it("stops offering the restart when the server cannot restart itself", async () => {
    settings.restartSupported = true;
    ping.mockResolvedValue({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    restartDaemon.mockRejectedValue(new ApiError(501, "restart unavailable"));

    await settings.restartDaemon();

    expect(settings.restartSupported).toBe(false);
    expect(settings.restartFailed).toBe(false);
    expect(settings.restartError).toBeNull();
    expect(settings.restarting).toBe(false);
  });

  it("shows recovery when the restart POST stalls", async () => {
    ping.mockResolvedValue({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    restartDaemon.mockImplementation(
      (options) =>
        new Promise((_, reject) =>
          options?.signal?.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          ),
        ),
    );
    const restart = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(30_000);
    await restart;
    expect(restartDaemon).toHaveBeenCalledOnce();
    expect(settings.restartFailed).toBe(true);
    expect(settings.restarting).toBe(false);
    expect(reload).not.toHaveBeenCalled();
  });

  it("offers retry when a lost request leaves the original server answering", async () => {
    ping.mockResolvedValue({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    restartDaemon.mockRejectedValueOnce(new TypeError("Failed to fetch"));
    const first = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(30_000);
    await first;
    expect(settings.restartCanRetry).toBe(true);
    expect(settings.restartFailed).toBe(true);
    expect(settings.restarting).toBe(false);
    expect(reload).not.toHaveBeenCalled();

    restartDaemon.mockResolvedValueOnce({ restarting: true });
    ping.mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    ping.mockResolvedValue({ healthy: true, ok: true, pid: 100, instance_id: "new" });
    const retry = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(1_000);
    await retry;
    expect(restartDaemon).toHaveBeenCalledTimes(2);
    expect(reload).toHaveBeenCalledOnce();
  });

  it("keeps recovery available when the server stops during an accepted restart", async () => {
    ping.mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    ping.mockRejectedValue(new TypeError("Failed to fetch"));
    restartDaemon.mockResolvedValue({ restarting: true });
    const restart = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(30_000);
    await restart;
    expect(settings.restartFailed).toBe(true);
    expect(settings.restartCanRetry).toBe(false);
    expect(reload).not.toHaveBeenCalled();
  });

  it.each([
    ["the original instance", { healthy: true, ok: true, pid: 100, instance_id: "old" }],
    ["an unidentified server", { healthy: true, ok: true, pid: 100 }],
    ["a non-string identity", { healthy: true, ok: true, pid: 100, instance_id: 123 }],
    ["an API error", new ApiError(500, "unavailable")],
  ])("checks again without accepting %s as the replacement", async (_, answer) => {
    ping.mockResolvedValue({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    ping.mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    restartDaemon.mockResolvedValue({ restarting: true });
    const restart = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(30_000);
    await restart;
    expect(settings.restartFailed).toBe(true);
    expect(settings.restartCanRetry).toBe(false);
    expect(reload).not.toHaveBeenCalled();

    if (answer instanceof ApiError) ping.mockRejectedValueOnce(answer);
    else ping.mockResolvedValueOnce(answer as never);
    const check = settings.checkConnection();
    await vi.advanceTimersByTimeAsync(1_000);
    expect(reload).not.toHaveBeenCalled();
    ping.mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "new" });
    await vi.advanceTimersByTimeAsync(1_000);
    await check;
    expect(reload).toHaveBeenCalledOnce();
    expect(restartDaemon).toHaveBeenCalledOnce();
  });

  it("asks for a token when the restarted daemon requires one", async () => {
    ping
      .mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "old" })
      .mockRejectedValue(new ApiError(401, "Unauthorized"));
    restartDaemon.mockResolvedValue({ restarting: true });

    const restart = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(1_000);
    await restart;

    expect(settings.needsAuth).toBe(true);
    expect(settings.restarting).toBe(false);
    expect(settings.restartFailed).toBe(false);
    expect(reload).not.toHaveBeenCalled();
  });

  it("does not ask the selected server for a token when another server rejects the restart", async () => {
    setServerUrl("https://first.example");
    ping.mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    let rejectRestart!: (error: Error) => void;
    restartDaemon.mockImplementationOnce(
      () => new Promise((_, reject) => (rejectRestart = reject)),
    );
    const restart = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(0);
    setServerUrl("https://second.example");
    rejectRestart(new ApiError(401, "Unauthorized"));
    await restart;
    expect(settings.needsAuth).toBe(false);
    expect(settings.restarting).toBe(false);
    expect(reload).not.toHaveBeenCalled();
  });

  it("does not reload the selected server when another server finishes restarting", async () => {
    setServerUrl("https://first.example");
    ping.mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    restartDaemon.mockResolvedValue({ restarting: true });
    const restart = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(0);
    setServerUrl("https://second.example");
    ping.mockResolvedValueOnce({ healthy: true, ok: true, pid: 100, instance_id: "new" });
    await vi.advanceTimersByTimeAsync(1_000);
    await restart;
    expect(reload).not.toHaveBeenCalled();
    expect(settings.restarting).toBe(false);
    expect(settings.restartFailed).toBe(false);
  });

  it("asks for a token when checking the replacement connection", async () => {
    ping.mockResolvedValue({ healthy: true, ok: true, pid: 100, instance_id: "old" });
    restartDaemon.mockResolvedValue({ restarting: true });
    const restart = settings.restartDaemon();
    await vi.advanceTimersByTimeAsync(30_000);
    await restart;
    ping.mockRejectedValueOnce(new ApiError(401, "Unauthorized"));
    const check = settings.checkConnection();
    await vi.advanceTimersByTimeAsync(1_000);
    await check;
    expect(settings.needsAuth).toBe(true);
    expect(reload).not.toHaveBeenCalled();
  });
});
