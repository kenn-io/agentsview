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
  { name: "openhands", color: "var(--accent-teal)", label: "OpenHands" },
  { name: "cursor", color: "var(--accent-black)" },
  { name: "amp", color: "var(--accent-coral)", label: "Amp" },
  { name: "zencoder", color: "var(--accent-red)", label: "Zencoder" },
  {
    name: "vscode-copilot",
    color: "var(--accent-teal)",
    label: "VS Code Copilot",
  },
  { name: "pi", color: "var(--accent-indigo)", label: "Pi" },
  {
    name: "openclaw",
    color: "var(--accent-orange)",
    label: "OpenClaw",
  },
  { name: "iflow", color: "var(--accent-sky)", label: "iFlow" },
  { name: "kimi", color: "var(--accent-pink)", label: "Kimi" },
  { name: "claude-ai", color: "var(--accent-violet)", label: "Claude.ai" },
  { name: "chatgpt", color: "var(--accent-lime)", label: "ChatGPT" },
  { name: "kiro", color: "var(--accent-lime)", label: "Kiro" },
  { name: "kiro-ide", color: "var(--accent-lime)", label: "Kiro IDE" },
  { name: "cortex", color: "var(--accent-cyan)", label: "Cortex Code" },
  { name: "piebald", color: "var(--accent-orange)", label: "Piebald" },
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
