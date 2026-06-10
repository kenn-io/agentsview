import { getAuthToken, isRemoteConnection } from "../api/runtime.js";

const DEBOUNCE_MS = 5_000;
const TIMEOUT_MS = 3_000;

interface VisibilityHealthCheckOptions {
  onBackendDegraded?: (status: number) => void;
}

/**
 * Set up a visibilitychange listener that pings the backend when the
 * page becomes visible. If the backend is unreachable (network error or
 * timeout), the page reloads automatically.
 *
 * 4xx responses (401/403) are treated as proof the backend is alive
 * and do not trigger a reload — auth recovery is handled elsewhere. The same
 * is true for 5xx responses: the backend process answered, so reloading the
 * SPA is unlikely to repair a degraded dependency such as PostgreSQL.
 *
 * In desktop mode with a local sidecar, this handler is a no-op
 * because Tauri's on_window_event focus handler owns recovery.
 * When a remote server is configured, the handler stays active
 * since Tauri only probes the local sidecar, not the remote backend.
 *
 * The base URL is resolved lazily on each check so it stays current
 * if the connection target changes at runtime.
 *
 * Returns a cleanup function that removes the listener.
 */
export function setupVisibilityHealthCheck(
  getBaseUrl: () => string,
  opts: VisibilityHealthCheckOptions = {},
): () => void {
  // In desktop mode with a local sidecar, Tauri owns recovery via
  // on_window_event focus. Skip the frontend handler to avoid racing
  // with Rust's navigate. Keep it enabled when a remote server is
  // configured since Tauri only probes the local sidecar.
  const isDesktop = new URLSearchParams(window.location.search).has(
    "desktop",
  );
  if (isDesktop && !isRemoteConnection()) return () => {};

  let lastCheck = 0;

  function onVisibilityChange() {
    if (document.visibilityState !== "visible") return;
    const now = Date.now();
    if (now - lastCheck < DEBOUNCE_MS) return;
    lastCheck = now;

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), TIMEOUT_MS);
    const init: RequestInit = { signal: controller.signal };
    const token = getAuthToken();
    if (token) {
      init.headers = { Authorization: `Bearer ${token}` };
    }

    fetch(`${getBaseUrl()}/version`, init)
      .then((res) => {
        clearTimeout(timer);
        if (res.status >= 500) {
          opts.onBackendDegraded?.(res.status);
          return;
        }
      })
      .catch(() => {
        clearTimeout(timer);
        window.location.reload();
      });
  }

  document.addEventListener("visibilitychange", onVisibilityChange);
  return () =>
    document.removeEventListener(
      "visibilitychange",
      onVisibilityChange,
    );
}
