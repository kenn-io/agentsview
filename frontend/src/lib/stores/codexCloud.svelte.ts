import { CodexCloudService, type CodexcloudHomeStatus } from "../api/generated/index";
import { ApiError } from "../api/runtime.js";

class CodexCloudStatusStore {
  // Empty when the source is not configured: the server answers no rows.
  rows: CodexcloudHomeStatus[] = $state([]);
  // Set once the server answers 404: the route never appears without a
  // restart into a newer server, so later polls are skipped.
  private unsupported = false;

  async load() {
    if (this.unsupported) return;
    try {
      this.rows = await CodexCloudService.getApiV1CodexCloudStatus();
    } catch (error) {
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
