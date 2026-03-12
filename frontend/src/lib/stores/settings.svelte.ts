import {
  getSettings,
  updateSettings,
  type AppSettings,
  ApiError,
  setAuthToken,
  isRemoteConnection,
} from "../api/client.js";

class SettingsStore {
  agentDirs: Record<string, string[]> = $state({});
  githubConfigured: boolean = $state(false);
  terminal: AppSettings["terminal"] = $state({
    mode: "auto",
  });
  host: string = $state("");
  port: number = $state(0);
  authToken: string = $state("");
  remoteAccess: boolean = $state(false);
  loading: boolean = $state(false);
  saving: boolean = $state(false);
  error: string | null = $state(null);
  /** True when the API returned 401/403, indicating the user needs
   *  to provide an auth token before the app can load. */
  needsAuth: boolean = $state(false);

  async load() {
    this.loading = true;
    this.error = null;
    this.needsAuth = false;
    try {
      const data = await getSettings();
      this.agentDirs = data.agent_dirs;
      this.githubConfigured = data.github_configured;
      this.terminal = data.terminal;
      this.host = data.host;
      this.port = data.port;
      this.authToken = data.auth_token ?? "";
      this.remoteAccess = data.remote_access ?? false;
      // When the server returns an auth token (localhost only), persist
      // it so the client stays authenticated after remote access is
      // toggled on (which starts requiring auth for all requests).
      if (data.auth_token && !isRemoteConnection()) {
        setAuthToken(data.auth_token);
      }
    } catch (e) {
      if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
        this.needsAuth = true;
      } else {
        this.error =
          e instanceof Error ? e.message : "Failed to load settings";
      }
    } finally {
      this.loading = false;
    }
  }

  async save(patch: Partial<AppSettings>) {
    this.saving = true;
    this.error = null;
    try {
      const data = await updateSettings(patch);
      this.agentDirs = data.agent_dirs;
      this.githubConfigured = data.github_configured;
      this.terminal = data.terminal;
      this.host = data.host;
      this.port = data.port;
      this.authToken = data.auth_token ?? "";
      this.remoteAccess = data.remote_access ?? false;
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
