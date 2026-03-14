import { describe, it, expect } from "vitest";
import {
  buildResumeCommand,
  supportsResume,
} from "./resume.js";

describe("supportsResume", () => {
  it("returns true for supported agents", () => {
    expect(supportsResume("claude")).toBe(true);
    expect(supportsResume("codex")).toBe(true);
    expect(supportsResume("copilot")).toBe(true);
    expect(supportsResume("gemini")).toBe(true);
    expect(supportsResume("opencode")).toBe(true);
    expect(supportsResume("amp")).toBe(true);
  });

  it("returns false for unsupported agents", () => {
    expect(supportsResume("cursor")).toBe(false);
    expect(supportsResume("vscode-copilot")).toBe(false);
    expect(supportsResume("unknown")).toBe(false);
  });

  it("returns false for prototype properties", () => {
    expect(supportsResume("toString")).toBe(false);
    expect(supportsResume("constructor")).toBe(false);
    expect(supportsResume("hasOwnProperty")).toBe(false);
  });
});

describe("buildResumeCommand", () => {
  it("generates claude resume command", () => {
    expect(
      buildResumeCommand("claude", "abc-123-def"),
    ).toBe("claude --resume abc-123-def");
  });

  it("generates codex resume command", () => {
    expect(
      buildResumeCommand("codex", "codex:sess-1"),
    ).toBe("codex resume sess-1");
  });

  it("generates gemini resume command", () => {
    expect(
      buildResumeCommand("gemini", "gemini:sess-2"),
    ).toBe("gemini --resume sess-2");
  });

  it("generates opencode resume command", () => {
    expect(
      buildResumeCommand("opencode", "opencode:s3"),
    ).toBe("opencode --session s3");
  });

  it("generates amp resume command", () => {
    expect(
      buildResumeCommand("amp", "amp:t-1"),
    ).toBe("amp --resume t-1");
  });

  it("strips agent prefix from compound IDs", () => {
    expect(
      buildResumeCommand("codex", "codex:my-session-id"),
    ).toBe("codex resume my-session-id");
  });

  it("handles plain IDs without prefix", () => {
    expect(
      buildResumeCommand("claude", "550e8400-e29b-41d4-a716-446655440000"),
    ).toBe("claude --resume 550e8400-e29b-41d4-a716-446655440000");
  });

  it("returns null for unsupported agents", () => {
    expect(buildResumeCommand("cursor", "id")).toBeNull();
    expect(buildResumeCommand("unknown", "id")).toBeNull();
  });

  it("generates copilot resume command", () => {
    expect(
      buildResumeCommand("copilot", "copilot:a108ddbe-acdb-42f4-a35e-6c2938bf038b"),
    ).toBe("copilot --resume=a108ddbe-acdb-42f4-a35e-6c2938bf038b");
  });

  describe("claude flags", () => {
    const id = "test-session";

    it("adds --dangerously-skip-permissions", () => {
      expect(
        buildResumeCommand("claude", id, {
          skipPermissions: true,
        }),
      ).toBe(
        "claude --resume test-session --dangerously-skip-permissions",
      );
    });

    it("adds --fork-session", () => {
      expect(
        buildResumeCommand("claude", id, {
          forkSession: true,
        }),
      ).toBe("claude --resume test-session --fork-session");
    });

    it("adds --print", () => {
      expect(
        buildResumeCommand("claude", id, { print: true }),
      ).toBe("claude --resume test-session --print");
    });

    it("combines multiple flags", () => {
      expect(
        buildResumeCommand("claude", id, {
          skipPermissions: true,
          forkSession: true,
          print: true,
        }),
      ).toBe(
        "claude --resume test-session --dangerously-skip-permissions --fork-session --print",
      );
    });

    it("ignores flags for non-claude agents", () => {
      expect(
        buildResumeCommand("codex", "codex:s1", {
          skipPermissions: true,
        }),
      ).toBe("codex resume s1");
    });
  });

  it("single-quotes IDs with special characters", () => {
    const cmd = buildResumeCommand(
      "claude",
      "id with spaces",
    );
    expect(cmd).toBe("claude --resume 'id with spaces'");
  });

  it("escapes single quotes in IDs using POSIX quoting", () => {
    const cmd = buildResumeCommand(
      "claude",
      "it's a test",
    );
    expect(cmd).toBe(
      "claude --resume 'it'\"'\"'s a test'",
    );
  });

  it("quotes shell metacharacters safely", () => {
    const cmd = buildResumeCommand(
      "codex",
      "codex:$(whoami)",
    );
    expect(cmd).toBe("codex resume '$(whoami)'");
  });

  it("quotes backtick injection attempts", () => {
    const cmd = buildResumeCommand(
      "gemini",
      "gemini:`rm -rf /`",
    );
    expect(cmd).toBe("gemini --resume '`rm -rf /`'");
  });

  it("quotes $VAR expansion attempts", () => {
    const cmd = buildResumeCommand(
      "amp",
      "amp:$HOME/evil",
    );
    expect(cmd).toBe("amp --resume '$HOME/evil'");
  });
});
