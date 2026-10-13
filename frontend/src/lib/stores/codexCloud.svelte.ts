import { CodexCloudService, type CodexcloudHomeStatus } from "../api/generated/index";
import { ApiError, getServerUrl } from "../api/runtime.js";

class CodexCloudStatusStore {
  // Empty when the source is not configured: the server answers no rows.
  rows: CodexcloudHomeStatus[] = $state([]);
  // The server URL that rows and unsupported describe. Some disconnect
  // paths switch servers without a page reload, so a change resets both.
  private server: string | null = null;
  // Set once the server answers 404: the route never appears without a
  // restart into a newer server, so later polls are skipped.
  private unsupported = false;

  async load() {
    const server = getServerUrl();
    if (server !== this.server) {
      this.server = server;
      this.unsupported = false;
      this.rows = [];
    }
    if (this.unsupported) return;
    try {
      const rows = await CodexCloudService.getApiV1CodexCloudStatus();
      // An answer from the previous server says nothing about this one.
      if (getServerUrl() !== server) return;
      this.rows = rows;
    } catch (error) {
      if (getServerUrl() !== server) return;
      // A server without the route predates the source or does not run
      // it, so there is nothing to show. Other failures keep the last rows;
      // the status bar already reports an unreachable or degraded server.
      if (error instanceof ApiError && error.status === 404) {
        this.unsupported = true;
        this.rows = [];
        return;
      }
      console.warn("Failed to load codex-cloud status:", error);
    }
  }
}

export const codexCloudStatus = new CodexCloudStatusStore();
