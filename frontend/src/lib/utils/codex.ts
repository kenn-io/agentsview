import { stripIdPrefix } from "./resume.js";

/**
 * Build the URL understood by the Codex Desktop application for a local
 * Codex thread. Remote sessions cannot be opened by a local desktop app.
 */
export function codexDesktopLink(
  agent: string,
  sessionId: string,
): string | null {
  if (agent !== "codex" || sessionId.includes("~")) return null;

  const threadId = stripIdPrefix(sessionId, agent);
  if (!threadId) return null;

  return `codex://threads/${encodeURIComponent(threadId)}`;
}
