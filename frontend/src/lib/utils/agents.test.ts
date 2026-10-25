import { describe, it, expect } from "vite-plus/test";
import {
  KNOWN_AGENTS,
  agentColor,
  agentForeground,
  agentLabel,
  entrypointBadge,
} from "./agents.js";

describe("agentColor", () => {
  it("falls back to blue for unknown agents", () => {
    expect(agentColor("unknown")).toBe("var(--accent-blue)");
    expect(agentColor("")).toBe("var(--accent-blue)");
  });
});

describe("agentForeground", () => {
  it("returns the matching foreground token for every known agent fill", () => {
    for (const agent of KNOWN_AGENTS) {
      const color = agentColor(agent.name);
      const token = color.match(/^var\(--accent-([a-z]+)\)$/)?.[1];
      expect(token, `${agent.name} color token`).toBeTruthy();
      expect(agentForeground(agent.name)).toBe(`var(--accent-${token}-foreground)`);
    }
  });

  it("uses the accent foreground for unknown fallback agents", () => {
    expect(agentForeground("unknown")).toBe("var(--accent-blue-foreground)");
    expect(agentForeground("")).toBe("var(--accent-blue-foreground)");
  });

  it("uses non-blue accent foregrounds for non-blue agent fills", () => {
    expect(agentForeground("codex")).toBe("var(--accent-green-foreground)");
    expect(agentForeground("opencode")).toBe("var(--accent-purple-foreground)");
    expect(agentForeground("opencodereview")).toBe("var(--accent-indigo-foreground)");
  });
});

describe("entrypointBadge", () => {
  it("shows non-default entrypoints and hides the default cli value", () => {
    expect(entrypointBadge("sdk-cli")).toBe("sdk-cli");
    expect(entrypointBadge(" sdk-cli ")).toBe("sdk-cli");
    expect(entrypointBadge("cli")).toBeNull();
    expect(entrypointBadge(" cli ")).toBeNull();
    expect(entrypointBadge("")).toBeNull();
    expect(entrypointBadge("   ")).toBeNull();
    expect(entrypointBadge(null)).toBeNull();
    expect(entrypointBadge(undefined)).toBeNull();
  });

  it("names the codex-cloud origins", () => {
    expect(entrypointBadge("codex-web")).toBe("Codex web");
    expect(entrypointBadge("codex-cloud")).toBe("Codex cloud");
    expect(entrypointBadge("dot")).toBe("dot");
  });
});

describe("agentLabel", () => {
  it("uses a non-empty raw session override and ignores whitespace-only values", () => {
    expect(agentLabel("claude", "triage")).toBe("triage");
    expect(agentLabel("claude", " triage ")).toBe(" triage ");
    expect(agentLabel("claude", "   ")).toBe("Claude");
  });
  it("returns explicit labels for hyphenated agents", () => {
    expect(agentLabel("vscode-copilot")).toBe("VS Code Copilot");
    expect(agentLabel("visualstudio-copilot")).toBe("Visual Studio Copilot");
    expect(agentLabel("openhands")).toBe("OpenHands");
    expect(agentLabel("devin")).toBe("Devin");
    expect(agentLabel("openclaw")).toBe("OpenClaw");
    expect(agentLabel("qclaw")).toBe("QClaw");
    expect(agentLabel("iflow")).toBe("iFlow");
    expect(agentLabel("junie")).toBe("Junie");
    expect(agentLabel("kimi-work")).toBe("Kimi Work");
    expect(agentLabel("workbuddy")).toBe("WorkBuddy");
    expect(agentLabel("codebuddy")).toBe("CodeBuddy");
    expect(agentLabel("piebald")).toBe("Piebald");
    expect(agentLabel("fx")).toBe("fx");
    expect(agentLabel("zed")).toBe("Zed");
    expect(agentLabel("qwen")).toBe("Qwen Code");
    expect(agentLabel("qwenpaw")).toBe("QwenPaw");
    expect(agentLabel("deepseek-tui")).toBe("DeepSeek TUI");
    expect(agentLabel("deepseek-harness")).toBe("DeepSeek Harness");
    expect(agentLabel("prime-agent")).toBe("Prime Agent");
    expect(agentLabel("stepcode")).toBe("StepCode");
    expect(agentLabel("qoder")).toBe("Qoder");
    expect(agentLabel("roocode")).toBe("RooCode");
    expect(agentLabel("omnigent")).toBe("Omnigent");
    expect(agentLabel("traex")).toBe("TraeX");
    expect(agentLabel("augure-code")).toBe("Augure Code");
    expect(agentLabel("augure-desktop")).toBe("Augure Desktop");
    expect(agentLabel("opencodereview")).toBe("Open Code Review");
    expect(agentLabel("crush")).toBe("Charm Crush");
    expect(agentLabel("muse")).toBe("Muse Code");
  });

  it("capitalizes simple agent names", () => {
    expect(agentLabel("claude")).toBe("Claude");
    expect(agentLabel("gemini")).toBe("Gemini");
  });
});
