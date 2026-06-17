import { describe, it, expect } from "vite-plus/test";
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
      "cowork",
      "codex",
      "copilot",
      "gemini",
      "opencode",
      "openhands",
      "cursor",
      "amp",
      "zencoder",
      "zed",
      "vscode-copilot",
      "visualstudio-copilot",
      "pi",
      "qwen",
      "qwenpaw",
      "deepseek-tui",
      "openclaw",
      "qclaw",
      "iflow",
      "kimi",
      "claude-ai",
      "chatgpt",
      "kiro",
      "kiro-ide",
      "cortex",
      "workbuddy",
      "piebald",
      "antigravity",
      "antigravity-cli",
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
    expect(agentColor("zed")).toBe(
      "var(--accent-green)",
    );
    expect(agentColor("pi")).toBe(
      "var(--accent-indigo)",
    );
    expect(agentColor("qwen")).toBe(
      "var(--accent-cyan)",
    );
    expect(agentColor("qwenpaw")).toBe(
      "var(--accent-cyan)",
    );
    expect(agentColor("deepseek-tui")).toBe(
      "var(--accent-cyan)",
    );
    expect(agentColor("vscode-copilot")).toBe(
      "var(--accent-teal)",
    );
    expect(agentColor("visualstudio-copilot")).toBe(
      "var(--accent-blue)",
    );
    expect(agentColor("qclaw")).toBe(
      "var(--accent-orange)",
    );
    expect(agentColor("workbuddy")).toBe(
      "var(--accent-violet)",
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
    expect(agentLabel("visualstudio-copilot")).toBe(
      "Visual Studio Copilot",
    );
    expect(agentLabel("openhands")).toBe("OpenHands");
    expect(agentLabel("openclaw")).toBe("OpenClaw");
    expect(agentLabel("qclaw")).toBe("QClaw");
    expect(agentLabel("iflow")).toBe("iFlow");
    expect(agentLabel("workbuddy")).toBe("WorkBuddy");
    expect(agentLabel("piebald")).toBe("Piebald");
    expect(agentLabel("zed")).toBe("Zed");
    expect(agentLabel("qwen")).toBe("Qwen Code");
    expect(agentLabel("qwenpaw")).toBe("QwenPaw");
    expect(agentLabel("deepseek-tui")).toBe("DeepSeek TUI");
  });

  it("capitalizes simple agent names", () => {
    expect(agentLabel("claude")).toBe("Claude");
    expect(agentLabel("gemini")).toBe("Gemini");
  });
});
