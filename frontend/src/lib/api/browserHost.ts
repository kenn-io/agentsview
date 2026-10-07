export interface BrowserHost {
  connect(): Promise<void>;
  status(): Promise<{ connected: boolean; pending: boolean; organization: string | null }>;
  fetch(
    path: string,
  ): Promise<{ status: number; body: string; retryAfter?: string; error?: string }>;
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
    status: () => tauri.core.invoke("claude_auth_status"),
    fetch: (path) => tauri.core.invoke("claude_auth_fetch", { path }),
    disconnect: () => tauri.core.invoke("claude_auth_disconnect"),
  };
}
