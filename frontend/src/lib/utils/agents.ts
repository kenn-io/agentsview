export interface AgentMeta {
  name: string;
  color: string;
  label?: string;
}

export const KNOWN_AGENTS: readonly AgentMeta[] = [
  { name: "claude", color: "var(--accent-blue)" },
  { name: "codex", color: "var(--accent-green)" },
  { name: "copilot", color: "var(--accent-amber)" },
  { name: "gemini", color: "var(--accent-rose)" },
  { name: "opencode", color: "var(--accent-purple)" },
  { name: "cursor", color: "var(--accent-black)" },
  { name: "amp", color: "var(--accent-coral)", label: "Amp" },
  { name: "vscode-copilot", color: "var(--accent-teal)" },
  { name: "pi", color: "var(--accent-indigo)", label: "Pi" },
  { name: "openclaw", color: "var(--accent-orange)" },
];

const agentColorMap = new Map(
  KNOWN_AGENTS.map((a) => [a.name, a.color]),
);

export function agentColor(agent: string): string {
  return agentColorMap.get(agent) ?? "var(--accent-blue)";
}

export function agentLabel(agent: string): string {
  const meta = KNOWN_AGENTS.find((a) => a.name === agent);
  if (meta?.label) return meta.label;
  return agent.charAt(0).toUpperCase() + agent.slice(1);
}
