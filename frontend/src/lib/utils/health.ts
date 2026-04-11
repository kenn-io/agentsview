import { getAuthToken } from "../api/client.js";

const DEBOUNCE_MS = 5_000;
const TIMEOUT_MS = 3_000;

/**
 * Set up a visibilitychange listener that pings the backend when the
 * page becomes visible. If the backend is unreachable (network error,
 * timeout, or 5xx), the page reloads automatically.
 *
 * 4xx responses (401/403) are treated as proof the backend is alive
 * and do not trigger a reload — auth recovery is handled elsewhere.
 *
 * The base URL is resolved lazily on each check so it stays current
 * if the connection target changes at runtime.
 *
 * Returns a cleanup function that removes the listener.
 */
export function setupVisibilityHealthCheck(
  getBaseUrl: () => string,
): () => void {
  let lastCheck = 0;

  function onVisibilityChange() {
    if (document.visibilityState !== "visible") return;
    const now = Date.now();
    if (now - lastCheck < DEBOUNCE_MS) return;
    lastCheck = now;

    const init: RequestInit = {
      signal: AbortSignal.timeout(TIMEOUT_MS),
    };
    const token = getAuthToken();
    if (token) {
      init.headers = { Authorization: `Bearer ${token}` };
    }

    fetch(`${getBaseUrl()}/version`, init)
      .then((res) => {
        if (res.status >= 500) throw new Error(`HTTP ${res.status}`);
      })
      .catch(() => {
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
