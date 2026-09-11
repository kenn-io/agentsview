import {
  SettingsService,
  type SettingsResponse,
  type SessionProviderResponse,
  type TerminalResponse,
} from "../api/generated/index";
import {
  ApiError,
  generatedErrorMessage,
  setAuthToken,
  isRemoteConnection,
} from "../api/runtime.js";
import { DEFAULT_CHART_PALETTE, isChartPalette, type ChartPalette } from "../utils/chartPalette.js";
import { ui } from "./ui.svelte.js";

export type ToolResultImagesPolicy = "keep" | "drop";

type TerminalConfig = TerminalResponse;

interface AppSettings extends Omit<
  SettingsResponse,
  "terminal" | "agent_dirs" | "chart_palette" | "session_providers" | "disabled_agents"
> {
  agent_dirs: Record<string, string[]>;
  session_providers: SessionProvider[];
  disabled_agents: string[];
  agent_homes?: Record<string, string[]>;
  terminal: TerminalConfig;
  chart_palette: ChartPalette;
}

export interface SessionProvider extends Omit<SessionProviderResponse, "dirs"> {
  dirs: string[];
}

/** Build an actionable message for a 403 from the settings API. A
 *  403 means the server rejected the request origin/Host (not that a
 *  token is required), which typically happens behind SSH
 *  port-forwarding, a reverse proxy, or a remote dev environment.
 *  Newer servers return a descriptive body; for older servers that
 *  return a bare "Forbidden", supply the actionable hint ourselves. */
function forbiddenMessage(serverMessage: string): string {
  const detail = serverMessage.trim();
  if (detail && detail.toLowerCase() !== "forbidden") {
    return detail;
  }
  return (
    "Server rejected this origin. If you are reaching agentsview " +
    "through SSH port-forwarding, a reverse proxy, or a remote dev " +
    "environment, restart it with --public-url <origin> matching the " +
    "URL in your browser."
  );
}

class SettingsStore {
  private mutationQueue: Promise<void> | null = null;
  private pendingZoomSave = false;
  private zoomSaveVersion = 0;
  private loadVersion = 0;
  agentDirs: Record<string, string[]> = $state({});
  sessionProviders: SessionProvider[] = $state([]);
  disabledAgents: string[] = $state([]);
  githubConfigured: boolean = $state(false);
  terminal: AppSettings["terminal"] = $state({
    mode: "auto",
  });
  host: string = $state("");
  port: number = $state(0);
  authToken: string = $state("");
  requireAuth: boolean = $state(false);
  readOnly: boolean = $state(false);
  chartPalette: ChartPalette = $state(DEFAULT_CHART_PALETTE);
  toolResultImages: ToolResultImagesPolicy = $state("keep");
  loaded: boolean = $state(false);
  loading: boolean = $state(false);
  saving: boolean = $state(false);
  error: string | null = $state(null);
  saveError: string | null = $state(null);
  /** True when the API returned 401, indicating the user needs
   *  to provide an auth token before the app can load. */
  needsAuth: boolean = $state(false);

  constructor() {
    ui.setZoomSaveCallback((level) => {
      this.zoomSaveVersion += 1;
      if (!this.loaded || this.error) {
        this.pendingZoomSave = true;
      } else if (!this.readOnly) {
        void this.save({ zoom_level: level });
      }
    });
  }

  async load(): Promise<void> {
    if (this.saving && this.mutationQueue) {
      await this.mutationQueue;
      return this.load();
    }
    const loadVersion = ++this.loadVersion;
    const zoomChangeVersion = ui.zoomChangeVersion;
    const zoomSaveVersion = this.zoomSaveVersion;
    const mutationActive = this.saving;
    const pendingZoomSave = this.pendingZoomSave;
    let loadedData = false;
    this.loading = true;
    this.loaded = false;
    this.error = null;
    this.saveError = null;
    this.needsAuth = false;
    try {
      const data = await SettingsService.getApiV1Settings();
      if (!isChartPalette(data.chart_palette)) {
        throw new Error(
          `Invalid chart_palette in settings response: ${String(data.chart_palette)}`,
        );
      }
      this.agentDirs = data.agent_dirs;
      this.sessionProviders = data.session_providers ?? [];
      this.disabledAgents = data.disabled_agents ?? [];
      this.githubConfigured = data.github_configured;
      this.terminal = data.terminal;
      this.host = data.host;
      this.port = data.port;
      this.authToken = data.auth_token ?? "";
      this.requireAuth = data.require_auth ?? false;
      this.readOnly = data.read_only === true;
      this.chartPalette = data.chart_palette;
      if (
        loadVersion === this.loadVersion &&
        ui.zoomChangeVersion === zoomChangeVersion &&
        this.zoomSaveVersion === zoomSaveVersion &&
        !mutationActive &&
        !pendingZoomSave
      ) {
        if (data.zoom_level !== undefined) {
          ui.applyZoomLevel(data.zoom_level);
        } else {
          ui.restoreStoredZoom();
        }
      }
      // A response without the field, including every fixture that predates
      // it, reads as the default keep policy instead of failing the load.
      this.toolResultImages = data.tool_result_images === "drop" ? "drop" : "keep";
      // When the server returns an auth token (localhost only), persist
      // it so the client stays authenticated after remote access is
      // toggled on (which starts requiring auth for all requests).
      if (data.auth_token && !isRemoteConnection()) {
        setAuthToken(data.auth_token);
      }
      loadedData = true;
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        this.needsAuth = true;
      } else if (e instanceof ApiError && e.status === 403) {
        this.error = forbiddenMessage(generatedErrorMessage(e));
      } else {
        this.error = e instanceof Error ? e.message : "Failed to load settings";
      }
    } finally {
      this.loading = false;
      this.loaded = true;
      if (
        loadedData &&
        loadVersion === this.loadVersion &&
        this.pendingZoomSave
      ) {
        this.pendingZoomSave = false;
        if (!this.readOnly) void this.save({ zoom_level: ui.zoomLevel });
      }
    }
  }

  save(patch: Partial<AppSettings>): Promise<boolean> {
    return this.runMutation(() => this.performSave(patch));
  }

  runMutation<T>(mutation: () => Promise<T>): Promise<T> {
    this.saving = true;
    const operation = this.mutationQueue ? this.mutationQueue.then(mutation) : mutation();
    const queueEnd = operation.then(
      () => undefined,
      () => undefined,
    );
    this.mutationQueue = queueEnd;
    return operation.finally(() => {
      if (this.mutationQueue === queueEnd) {
        this.mutationQueue = null;
        this.saving = false;
      }
    });
  }

  private async performSave(patch: Partial<AppSettings>): Promise<boolean> {
    this.saveError = null;
    try {
      const data = await SettingsService.putApiV1Settings(patch);
      if (!isChartPalette(data.chart_palette)) {
        throw new Error(
          `Invalid chart_palette in settings response: ${String(data.chart_palette)}`,
        );
      }
      this.agentDirs = data.agent_dirs;
      this.sessionProviders = data.session_providers ?? [];
      this.disabledAgents = data.disabled_agents ?? [];
      this.githubConfigured = data.github_configured;
      this.terminal = data.terminal;
      this.host = data.host;
      this.port = data.port;
      this.authToken = data.auth_token ?? "";
      this.requireAuth = data.require_auth ?? false;
      this.readOnly = data.read_only === true;
      this.chartPalette = data.chart_palette;
      // A response without the field, including every fixture that predates
      // it, reads as the default keep policy instead of failing the load.
      this.toolResultImages = data.tool_result_images === "drop" ? "drop" : "keep";
      if (data.auth_token && !isRemoteConnection()) {
        setAuthToken(data.auth_token);
      }
      return true;
    } catch (e) {
      this.saveError = e instanceof Error ? e.message : "Failed to save settings";
      return false;
    }
  }
}

export const settings = new SettingsStore();
