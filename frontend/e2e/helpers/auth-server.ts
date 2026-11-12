// scripts/e2e-server.sh starts a second fixture server on the port after the
// main one when AGENTSVIEW_E2E_AUTH_PORT is set. It requires this bearer token.
export const AUTH_SERVER_TOKEN = "e2e-auth-token";

export function authServerPort(mainPort: number): number {
  return mainPort + 1;
}

export function authServerURL(baseURL: string): string {
  const url = new URL(baseURL);
  url.port = String(authServerPort(Number(url.port)));
  return url.origin;
}
