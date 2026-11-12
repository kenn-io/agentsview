import { m } from "../i18n/index.js";
import {
  DaemonService,
  HealthService,
  SettingsService,
  type SettingsResponse,
  type SessionProviderResponse,
  type TerminalResponse,
} from "../api/generated/index";
import {
  ApiError,
  generatedErrorMessage,
  setAuthToken,
  getAuthToken,
  getGeneratedBase,
  getServerUrl,
  isRemoteConnection,
  type ApiRequestOptions,
} from "../api/runtime.js";
import { DEFAULT_CHART_PALETTE, isChartPalette, type ChartPalette } from "../utils/chartPalette.js";
import { insights } from "./insights.svelte.js";
import { ui } from "./ui.svelte.js";

const RESTART_TIMEOUT_MS = 30_000;
const RESTART_POLL_MS = 1_000;
function restartTarget(): ApiRequestOptions {
  const token = getAuthToken();
  return {
    baseUrl: getGeneratedBase(),
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  };
}
/** Wait, resolving false instead when the signal aborts first. */
function delay(ms: number, signal: AbortSignal): Promise<boolean> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve(false);
    const onAbort = () => {
      clearTimeout(timer);
      resolve(false);
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve(true);
    }, ms);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

export type ToolResultImagesPolicy = "keep" | "drop" | "offload";

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
  private loadVersion = 0;
  private serverUrl = getServerUrl();
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
  /** The server can replace its own daemon process. */
  restartSupported: boolean = $state(false);
  /** A saved setting waits for a daemon restart to take effect. */
  restartRequired: boolean = $state(false);
  restarting: boolean = $state(false);
  restartFailed: boolean = $state(false);
  restartCanRetry: boolean = $state(false);
  private originalInstance = "";
  private restartUncertain = false;
  /** The server the last restart attempt was sent to, kept for rechecks. */
  private restartTargetUsed!: ApiRequestOptions;
  /** The server's reason when it refused the restart. */
  restartError: string | null = $state(null);
  /** True when the API returned 401, indicating the user needs
   *  to provide an auth token before the app can load. */
  needsAuth: boolean = $state(false);

  async load(): Promise<void> {
    const serverUrl = getServerUrl();
    if (serverUrl !== this.serverUrl) {
      this.serverUrl = serverUrl;
      insights.resetAgent();
    }
    if (this.saving && this.mutationQueue) {
      await this.mutationQueue;
      return this.load();
    }
    const loadVersion = ++this.loadVersion;
    const isCurrentLoad = () => loadVersion === this.loadVersion;
    this.loading = true;
    this.loaded = false;
    this.error = null;
    this.saveError = null;
    this.needsAuth = false;
    try {
      const data = await SettingsService.getApiV1Settings();
      if (!isCurrentLoad()) return;
      this.applySettings(data);
      ui.applyZoomDefault(data.zoom_level);
      insights.applyDefaultAgent(data.insight_default_agent);
    } catch (e) {
      if (!isCurrentLoad()) return;
      if (e instanceof ApiError && e.status === 401) {
        this.needsAuth = true;
      } else if (e instanceof ApiError && e.status === 403) {
        this.error = forbiddenMessage(generatedErrorMessage(e));
      } else {
        this.error = e instanceof Error ? e.message : m.settings_load_failed();
      }
    } finally {
      if (isCurrentLoad()) {
        this.loading = false;
        this.loaded = true;
      }
    }
  }

  /** The image policy only takes effect after a restart, so its saved response must say whether one is pending. */
  private applySaved(patch: Partial<AppSettings>, data: SettingsResponse): void {
    this.applySettings(data);
    const unknown =
      typeof data.restart_required !== "boolean" || typeof data.restart_supported !== "boolean";
    if (patch.tool_result_images !== undefined && unknown) {
      throw new Error(m.settings_restart_status_missing());
    }
  }

  private applySettings(data: SettingsResponse): void {
    if (!isChartPalette(data.chart_palette)) {
      throw new Error(`Invalid chart_palette in settings response: ${String(data.chart_palette)}`);
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
    this.restartSupported = data.restart_supported === true;
    this.restartRequired = data.restart_required === true;
    // A response without the field, including every fixture that predates
    // it, reads as the default keep policy instead of failing the load.
    this.toolResultImages =
      data.tool_result_images === "drop" || data.tool_result_images === "offload"
        ? data.tool_result_images
        : "keep";
    // When the server returns an auth token (localhost only), persist
    // it so the client stays authenticated after remote access is
    // toggled on (which starts requiring auth for all requests).
    if (data.auth_token && !isRemoteConnection()) {
      setAuthToken(data.auth_token);
    }
  }

  save(patch: Partial<AppSettings>): Promise<boolean> {
    return this.runMutation(() => this.performSave(patch));
  }

  /** Restart the daemon and reload once a new instance answers. Queued
   *  behind saves so it restarts with the settings just written. */
  restartDaemon(): Promise<void> {
    if (this.restarting) return Promise.resolve();
    this.restarting = true;
    this.restartFailed = false;
    this.restartError = null;
    this.restartCanRetry = false;
    this.restartUncertain = false;
    this.restartTargetUsed = restartTarget();
    return this.runMutation(() => this.performRestart(true, this.restartTargetUsed));
  }

  /** Check again for the same replacement without sending another restart. */
  checkConnection(): Promise<void> {
    if (this.restarting) return Promise.resolve();
    this.restarting = true;
    return this.runMutation(() => this.performRestart(false, this.restartTargetUsed));
  }

  private async waitForReplacement(
    signal: AbortSignal,
    target: ApiRequestOptions,
  ): Promise<boolean> {
    while (await delay(RESTART_POLL_MS, signal)) {
      try {
        const { instance_id: id } = await HealthService.getApiPing({ ...target, signal });
        this.restartCanRetry = this.restartUncertain && !!id && id === this.originalInstance;
        if (
          typeof id === "string" &&
          id &&
          typeof this.originalInstance === "string" &&
          this.originalInstance &&
          id !== this.originalInstance
        )
          return true;
      } catch (e) {
        this.restartCanRetry = false;
        if (e instanceof ApiError && e.status === 401) throw e;
      }
    }
    return false;
  }

  /** Record the running server's identity for this attempt, clearing any earlier one. */
  private async identify(signal: AbortSignal, target: ApiRequestOptions): Promise<void> {
    this.originalInstance = "";
    const id = (await HealthService.getApiPing({ ...target, signal })).instance_id;
    if (typeof id !== "string" || !id) throw new Error("Missing server identity");
    this.originalInstance = id;
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

  /** Restart, or with `send` off only wait again for the replacement. */
  private async performRestart(send: boolean, target: ApiRequestOptions): Promise<void> {
    const deadline = new AbortController();
    const timer = setTimeout(() => deadline.abort(), RESTART_TIMEOUT_MS);
    let navigating = false;
    try {
      if (send) {
        await this.identify(deadline.signal, target);
        try {
          await DaemonService.postApiV1DaemonRestart({ ...target, signal: deadline.signal });
        } catch (e) {
          // A lost response may still mean the server accepted the request.
          if (e instanceof ApiError) throw e;
          this.restartUncertain = true;
        }
      }
      if (await this.waitForReplacement(deadline.signal, target)) {
        navigating = true;
        window.location.reload();
        return;
      }
      this.restartFailed = true;
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        this.needsAuth = true;
      } else if (e instanceof ApiError && e.status === 501) {
        this.restartSupported = false;
      } else {
        this.restartFailed = true;
        if (!this.originalInstance) this.restartError = m.settings_restart_identify_failed();
        else if (e instanceof ApiError) this.restartError = generatedErrorMessage(e);
      }
    } finally {
      clearTimeout(timer);
      if (!navigating) this.restarting = false;
    }
  }

  private async performSave(patch: Partial<AppSettings>): Promise<boolean> {
    this.saveError = null;
    try {
      const data = await SettingsService.putApiV1Settings(patch);
      this.applySaved(patch, data);
      return true;
    } catch (e) {
      this.saveError = e instanceof Error ? e.message : m.settings_save_failed();
      return false;
    }
  }
}

export const settings = new SettingsStore();
