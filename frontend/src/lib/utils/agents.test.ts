import { describe, it, expect } from "vitest";
import {
  KNOWN_AGENTS,
  agentColor,
  agentLabel,
} from "./agents.js";

describe("KNOWN_AGENTS", () => {
  it("contains all expected agents", () => {
    const names = KNOWN_AGENTS.map((a) => a.name);
    expect(names).toEqual([
      "claude",
      "codex",
      "copilot",
      "gemini",
      "opencode",
      "openhands",
      "cursor",
      "amp",
      "zencoder",
      "vscode-copilot",
      "pi",
      "openclaw",
      "iflow",
      "kimi",
      "claude-ai",
      "chatgpt",
      "kiro",
      "kiro-ide",
      "cortex",
      "piebald",
    ]);
  });

  it("has a color for every agent", () => {
    for (const agent of KNOWN_AGENTS) {
      expect(agent.color).toMatch(/^var\(--accent-/);
    }
  });
});

describe("agentColor", () => {
  it("returns correct color for known agents", () => {
    expect(agentColor("claude")).toBe(
      "var(--accent-blue)",
    );
    expect(agentColor("codex")).toBe(
      "var(--accent-green)",
    );
    expect(agentColor("copilot")).toBe(
      "var(--accent-amber)",
    );
    expect(agentColor("gemini")).toBe(
      "var(--accent-rose)",
    );
    expect(agentColor("opencode")).toBe(
      "var(--accent-purple)",
    );
    expect(agentColor("openhands")).toBe(
      "var(--accent-teal)",
    );
    expect(agentColor("cursor")).toBe(
      "var(--accent-black)",
    );
    expect(agentColor("amp")).toBe(
      "var(--accent-coral)",
    );
    expect(agentColor("zencoder")).toBe(
      "var(--accent-red)",
    );
    expect(agentColor("pi")).toBe(
      "var(--accent-indigo)",
    );
    expect(agentColor("vscode-copilot")).toBe(
      "var(--accent-teal)",
    );
    expect(agentColor("piebald")).toBe(
      "var(--accent-orange)",
    );
  });

  it("falls back to blue for unknown agents", () => {
    expect(agentColor("unknown")).toBe(
      "var(--accent-blue)",
    );
    expect(agentColor("")).toBe("var(--accent-blue)");
  });
});

describe("agentLabel", () => {
  it("returns explicit labels for hyphenated agents", () => {
    expect(agentLabel("vscode-copilot")).toBe(
      "VS Code Copilot",
    );
    expect(agentLabel("openhands")).toBe("OpenHands");
    expect(agentLabel("openclaw")).toBe("OpenClaw");
    expect(agentLabel("iflow")).toBe("iFlow");
    expect(agentLabel("piebald")).toBe("Piebald");
  });

  it("capitalizes simple agent names", () => {
    expect(agentLabel("claude")).toBe("Claude");
    expect(agentLabel("gemini")).toBe("Gemini");
  });
});
