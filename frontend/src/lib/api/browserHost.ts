export interface BrowserHost {
  connect(): Promise<void>;
  fetch(
    path: string,
  ): Promise<{ status: number; body: string; retryAfter?: string; error?: string }>;
  close(): Promise<void>;
  disconnect(): Promise<void>;
}

export function getBrowserHost(): BrowserHost | undefined {
  const tauri = (
    window as Window & {
      __TAURI__?: {
        core: { invoke<T>(command: string, args?: Record<string, unknown>): Promise<T> };
      };
    }
  ).__TAURI__;
  if (!tauri) return;
  return {
    connect: () => tauri.core.invoke("claude_auth_connect"),
    fetch: (path) => tauri.core.invoke("claude_auth_fetch", { path }),
    close: () => tauri.core.invoke("claude_auth_close"),
    disconnect: () => tauri.core.invoke("claude_auth_disconnect"),
  };
}
