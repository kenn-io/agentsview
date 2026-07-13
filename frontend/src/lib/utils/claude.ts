/**
 * Build the Claude Code deep link for a local session's working directory.
 * Claude Code opens a new terminal session; it does not resume by session ID.
 */
export function claudeCodeLink(cwd: string | null): string {
  if (!cwd) return "claude://code/new";
  return `claude://code/new?folder=${encodeURIComponent(cwd)}`;
}
