export interface BrowserHost {
  connect(): Promise<void>;
  fetch(
    path: string,
  ): Promise<{ status: number; body: string; retryAfter?: string; error?: string }>;
  close(): Promise<void>;
}

export function getBrowserHost(): BrowserHost | undefined {
  const tauri = (
    window as Window & {
      __TAURI__?: {
        core: { invoke<T>(command: string, args?: Record<string, unknown>): Promise<T> };
      };
    }
  ).__TAURI__;
  if (tauri) return {
    connect: () => tauri.core.invoke("claude_auth_connect"),
    fetch: (path) => tauri.core.invoke("claude_auth_fetch", { path }),
    close: () => tauri.core.invoke("claude_auth_close"),
  };
  if (document.documentElement.dataset.agentsviewClaudeHost !== "chrome") return;
  function request<T>(method: string, path?: string): Promise<T> {
    const id = crypto.randomUUID();
    return new Promise((resolve, reject) => {
      const reply = (event: MessageEvent) => {
        if (event.source !== window || event.data?.type !== "agentsview-claude-reply" || event.data.id !== id) return;
        window.removeEventListener("message", reply);
        if (event.data.error) reject(new Error(event.data.error));
        else if (event.data.result === undefined) reject(new Error("Claude host returned no result"));
        else resolve(event.data.result);
      };
      window.addEventListener("message", reply);
      window.postMessage({ type: "agentsview-claude-request", id, method, path }, location.origin);
    });
  }
  return {
    connect: () => request("connect"),
    fetch: (path) => request("fetch", path),
    close: async () => {},
  };
}
