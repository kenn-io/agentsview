import { stripOwningIdPrefix } from "./resume.js";

/**
 * Build the URL understood by the Codex Desktop application for a local
 * Codex thread. Remote sessions cannot be opened by a local desktop app.
 */
export function codexDesktopLink(agent: string, sessionId: string): string | null {
  if (agent !== "codex" || sessionId.includes("~")) return null;

  // The thread ID comes from the ID's own prefix, not the display agent: a
  // session remapped onto codex still belongs to its original provider.
  const threadId = stripOwningIdPrefix(sessionId, agent);
  if (!threadId) return null;

  return `codex://threads/${encodeURIComponent(threadId)}`;
}
