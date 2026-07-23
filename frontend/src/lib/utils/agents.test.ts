import { describe, it, expect } from "vite-plus/test";
import {
  KNOWN_AGENTS,
  agentColor,
  agentForeground,
  agentLabel,
  entrypointBadge,
} from "./agents.js";

describe("KNOWN_AGENTS", () => {
  it("contains all expected agents", () => {
    const names = KNOWN_AGENTS.map((a) => a.name);
    expect(names).toEqual([
      "claude",
      "cowork",
      "codex",
      "copilot",
      "devin",
      "gemini",
      "opencode",
      "kilo",
      "kilo-legacy",
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
      "qoder",
      "piebald",
      "antigravity",
      "antigravity-cli",
      "vibe",
      "posit-assistant",
      "roocode",
      "omnigent",
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
    expect(agentColor("devin")).toBe(
      "var(--accent-red)",
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
    expect(agentColor("roocode")).toBe(
      "var(--accent-rose)",
    );
    expect(agentColor("omnigent")).toBe(
      "var(--accent-teal)",
    );
  });

  it("falls back to blue for unknown agents", () => {
    expect(agentColor("unknown")).toBe(
      "var(--accent-blue)",
    );
    expect(agentColor("")).toBe("var(--accent-blue)");
  });
});

describe("agentForeground", () => {
  it("returns the matching foreground token for every known agent fill", () => {
    for (const agent of KNOWN_AGENTS) {
      const color = agentColor(agent.name);
      const token = color.match(/^var\(--accent-([a-z]+)\)$/)?.[1];
      expect(token, `${agent.name} color token`).toBeTruthy();
      expect(agentForeground(agent.name)).toBe(
        `var(--accent-${token}-foreground)`,
      );
    }
  });

  it("uses the accent foreground for unknown fallback agents", () => {
    expect(agentForeground("unknown")).toBe(
      "var(--accent-blue-foreground)",
    );
    expect(agentForeground("")).toBe("var(--accent-blue-foreground)");
  });

  it("uses non-blue accent foregrounds for non-blue agent fills", () => {
    expect(agentForeground("codex")).toBe("var(--accent-green-foreground)");
    expect(agentForeground("opencode")).toBe(
      "var(--accent-purple-foreground)",
    );
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
});

describe("agentLabel", () => {
	it("uses a non-empty raw session override and ignores whitespace-only values", () => {
		expect(agentLabel("claude", "triage")).toBe("triage");
		expect(agentLabel("claude", " triage ")).toBe(" triage ");
		expect(agentLabel("claude", "   ")).toBe("Claude");
	});
  it("returns explicit labels for hyphenated agents", () => {
    expect(agentLabel("vscode-copilot")).toBe(
      "VS Code Copilot",
    );
    expect(agentLabel("visualstudio-copilot")).toBe(
      "Visual Studio Copilot",
    );
    expect(agentLabel("openhands")).toBe("OpenHands");
    expect(agentLabel("devin")).toBe("Devin");
    expect(agentLabel("openclaw")).toBe("OpenClaw");
    expect(agentLabel("qclaw")).toBe("QClaw");
    expect(agentLabel("iflow")).toBe("iFlow");
    expect(agentLabel("workbuddy")).toBe("WorkBuddy");
    expect(agentLabel("piebald")).toBe("Piebald");
    expect(agentLabel("zed")).toBe("Zed");
    expect(agentLabel("qwen")).toBe("Qwen Code");
    expect(agentLabel("qwenpaw")).toBe("QwenPaw");
    expect(agentLabel("deepseek-tui")).toBe("DeepSeek TUI");
    expect(agentLabel("qoder")).toBe("Qoder");
    expect(agentLabel("roocode")).toBe("RooCode");
    expect(agentLabel("omnigent")).toBe("Omnigent");
  });

  it("capitalizes simple agent names", () => {
    expect(agentLabel("claude")).toBe("Claude");
    expect(agentLabel("gemini")).toBe("Gemini");
  });
});
