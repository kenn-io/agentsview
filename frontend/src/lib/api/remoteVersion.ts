export const LIVE_SESSION_API_VERSION = 28;

export function supportsLiveSessionUpdates(apiVersion: number): boolean {
  return Number.isInteger(apiVersion) && apiVersion >= LIVE_SESSION_API_VERSION;
}
