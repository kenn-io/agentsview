export const MIN_REMOTE_API_VERSION = 28;

export function supportsRemoteVersion(apiVersion: number): boolean {
  return Number.isInteger(apiVersion) && apiVersion >= MIN_REMOTE_API_VERSION;
}
