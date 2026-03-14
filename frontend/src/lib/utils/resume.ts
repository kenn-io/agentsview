/** Agent types that support CLI session resumption. */
const RESUME_AGENTS: Record<
  string,
  (sessionId: string) => string
> = Object.create(null);
RESUME_AGENTS["claude"] = (id) =>
  `claude --resume ${shellQuote(id)}`;
RESUME_AGENTS["codex"] = (id) =>
  `codex resume ${shellQuote(id)}`;
RESUME_AGENTS["copilot"] = (id) =>
  `copilot --resume=${shellQuote(id)}`;
RESUME_AGENTS["gemini"] = (id) =>
  `gemini --resume ${shellQuote(id)}`;
RESUME_AGENTS["opencode"] = (id) =>
  `opencode --session ${shellQuote(id)}`;
RESUME_AGENTS["amp"] = (id) =>
  `amp --resume ${shellQuote(id)}`;

/** Flags available for Claude Code resume. */
export interface ClaudeResumeFlags {
  skipPermissions?: boolean;
  forkSession?: boolean;
  print?: boolean;
}

/**
 * POSIX-safe shell quoting using single quotes.
 * Any embedded single quotes are escaped as '"'"'.
 * Skips quoting for IDs that are purely alphanumeric + hyphens.
 */
function shellQuote(s: string): string {
  if (/^[a-zA-Z0-9_][\w-]*$/.test(s)) return s;
  return "'" + s.replace(/'/g, "'\"'\"'") + "'";
}

/**
 * Strip the agent-type prefix from a compound session ID, but only
 * when the prefix matches a known agent. Raw IDs that happen to
 * contain ":" are left untouched.
 */
function stripIdPrefix(id: string, agent?: string): string {
  if (agent) {
    const prefix = agent + ":";
    if (id.startsWith(prefix)) return id.slice(prefix.length);
  }
  return id;
}

/**
 * Returns true if the given agent supports CLI session resumption.
 */
export function supportsResume(agent: string): boolean {
  return Object.hasOwn(RESUME_AGENTS, agent);
}

/**
 * Build a CLI command to resume the given session in a terminal.
 *
 * @param agent - The agent type (e.g. "claude", "codex", "gemini")
 * @param sessionId - The session ID (may include agent prefix)
 * @param flags - Optional Claude-specific resume flags
 * @returns The shell command string, or null if the agent is not supported
 */
export function buildResumeCommand(
  agent: string,
  sessionId: string,
  flags?: ClaudeResumeFlags,
): string | null {
  const builder = RESUME_AGENTS[agent];
  if (!builder) return null;

  const rawId = stripIdPrefix(sessionId, agent);
  let cmd = builder(rawId);

  if (agent === "claude" && flags) {
    if (flags.skipPermissions)
      cmd += " --dangerously-skip-permissions";
    if (flags.forkSession) cmd += " --fork-session";
    if (flags.print) cmd += " --print";
  }

  return cmd;
}
