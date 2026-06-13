import {
  ApiError as GeneratedApiError,
  SettingsService,
  type SettingsResponse,
  type SettingsUpdateRequest,
  type TerminalResponse,
} from "../api/generated/index";
import {
  configureGeneratedClient,
  generatedErrorMessage,
  setAuthToken,
  isRemoteConnection,
} from "../api/runtime.js";

type TerminalConfig = TerminalResponse & {
  mode: "auto" | "custom" | "clipboard";
};

interface AppSettings extends Omit<SettingsResponse, "terminal" | "agent_dirs"> {
  agent_dirs: Record<string, string[]>;
  terminal: TerminalConfig;
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
  agentDirs: Record<string, string[]> = $state({});
  githubConfigured: boolean = $state(false);
  terminal: AppSettings["terminal"] = $state({
    mode: "auto",
  });
  host: string = $state("");
  port: number = $state(0);
  authToken: string = $state("");
  requireAuth: boolean = $state(false);
  readOnly: boolean = $state(false);
  loaded: boolean = $state(false);
  loading: boolean = $state(false);
  saving: boolean = $state(false);
  error: string | null = $state(null);
  /** True when the API returned 401, indicating the user needs
   *  to provide an auth token before the app can load. */
  needsAuth: boolean = $state(false);

  async load() {
    this.loading = true;
    this.loaded = false;
    this.error = null;
    this.needsAuth = false;
    try {
      configureGeneratedClient();
      const data =
        await SettingsService.getApiV1Settings() as unknown as AppSettings;
      this.agentDirs = data.agent_dirs;
      this.githubConfigured = data.github_configured;
      this.terminal = data.terminal;
      this.host = data.host;
      this.port = data.port;
      this.authToken = data.auth_token ?? "";
      this.requireAuth = data.require_auth ?? false;
      this.readOnly = data.read_only === true;
      // When the server returns an auth token (localhost only), persist
      // it so the client stays authenticated after remote access is
      // toggled on (which starts requiring auth for all requests).
      if (data.auth_token && !isRemoteConnection()) {
        setAuthToken(data.auth_token);
      }
    } catch (e) {
      if (e instanceof GeneratedApiError && e.status === 401) {
        this.needsAuth = true;
      } else if (e instanceof GeneratedApiError && e.status === 403) {
        this.error = forbiddenMessage(generatedErrorMessage(e));
      } else {
        this.error =
          e instanceof Error ? e.message : "Failed to load settings";
      }
    } finally {
      this.loading = false;
      this.loaded = true;
    }
  }

  async save(patch: Partial<AppSettings>) {
    this.saving = true;
    this.error = null;
    try {
      configureGeneratedClient();
      const data =
        await SettingsService.putApiV1Settings({
          requestBody: patch as SettingsUpdateRequest,
        }) as unknown as AppSettings;
      this.agentDirs = data.agent_dirs;
      this.githubConfigured = data.github_configured;
      this.terminal = data.terminal;
      this.host = data.host;
      this.port = data.port;
      this.authToken = data.auth_token ?? "";
      this.requireAuth = data.require_auth ?? false;
      this.readOnly = data.read_only === true;
      if (data.auth_token && !isRemoteConnection()) {
        setAuthToken(data.auth_token);
      }
    } catch (e) {
      this.error =
        e instanceof Error ? e.message : "Failed to save settings";
    } finally {
      this.saving = false;
    }
  }
}

export const settings = new SettingsStore();
