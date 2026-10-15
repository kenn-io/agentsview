import { SessionsService } from "./generated/index.js";
import type { ServiceInputTree } from "./generated/index.js";

export async function fetchSessionInputTree(
  sessionId: string,
  opts: { signal?: AbortSignal } = {},
): Promise<ServiceInputTree> {
  return SessionsService.getApiV1SessionsByIdInputTree({ id: sessionId }, { signal: opts.signal });
}
