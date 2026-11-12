import { expect } from "@playwright/test";

// scripts/e2e-server.sh starts a second fixture server on the port after the
// main one when AGENTSVIEW_E2E_AUTH_PORT is set. It requires this bearer token.
export const AUTH_SERVER_TOKEN = "e2e-auth-token";

export function authServerPort(mainPort: number): number {
  return mainPort < 65535 ? mainPort + 1 : mainPort - 1;
}

export function authServerURL(baseURL: string): string {
  const url = new URL(baseURL);
  // URL drops the default port, so an empty port means 80 on http.
  url.port = String(authServerPort(Number(url.port || 80)));
  return url.origin;
}

/** Waits until the server answers, which it does with 401 without a token. */
export async function waitForAuthServer(server: string): Promise<void> {
  await expect
    .poll(
      async () => {
        try {
          return (await fetch(`${server}/api/ping`)).status;
        } catch {
          return 0;
        }
      },
      { message: "authenticated e2e server did not start", timeout: 15_000 },
    )
    .toBe(401);
}
