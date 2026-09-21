import { describe, it, expect } from "vite-plus/test";
import { buildResumeCommand, formatResumeResponseCommand, supportsResume } from "./resume.js";

describe("supportsResume", () => {
  it("returns true for supported agents", () => {
    expect(supportsResume("claude")).toBe(true);
    expect(supportsResume("codex")).toBe(true);
    expect(supportsResume("traex")).toBe(true);
    expect(supportsResume("augure-code")).toBe(true);
    expect(supportsResume("copilot")).toBe(true);
    expect(supportsResume("cursor")).toBe(true);
    expect(supportsResume("gemini")).toBe(true);
    expect(supportsResume("opencode")).toBe(true);
    expect(supportsResume("amp")).toBe(true);
    expect(supportsResume("kiro")).toBe(true);
    expect(supportsResume("pi")).toBe(true);
  });

  it("returns false for unsupported agents", () => {
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
  it.each([
    ["claude", "devbox1~claude:abc-123", "claude --resume abc-123"],
    ["claude", "devbox1~abc-123", "claude --resume abc-123"],
    ["codex", "devbox1~codex:abc-123", "codex resume abc-123"],
    ["codex", "devbox1~codex:$(whoami)", "codex resume '$(whoami)'"],
    ["claude", "devbox1~claude:it's a test", "claude --resume 'it'\"'\"'s a test'"],
    ["claude", "devbox1~claude:a~b", "claude --resume 'a~b'"],
    ["cursor", "devbox1~cursor:abc-123", null],
    ["pi", "devbox1~pi:abc-123", null],
    ["unknown", "devbox1~unknown:abc-123", null],
  ])("remote fallback %s %s is ID-only", (agent, id, command) => {
    expect(buildResumeCommand(agent, id)).toBe(command);
  });

  it.each([
    ["abc-123", "claude --resume abc-123"],
    ["claude:abc-123", "claude --resume abc-123"],
    ["devbox1:abc-123", "claude --resume 'devbox1:abc-123'"],
    ["$(whoami)", "claude --resume '$(whoami)'"],
  ])("local fallback false positive %s", (id, command) => {
    expect(buildResumeCommand("claude", id)).toBe(command);
  });

  it("preserves remote Claude flags", () => {
    expect(buildResumeCommand("claude", "devbox1~claude:abc-123", {
      skipPermissions: true, forkSession: true, print: true,
    })).toBe("claude --resume abc-123 --dangerously-skip-permissions --fork-session --print");
  });
  it("generates claude resume command", () => {
    expect(buildResumeCommand("claude", "abc-123-def")).toBe("claude --resume abc-123-def");
  });

  it("generates codex resume command", () => {
    expect(buildResumeCommand("codex", "codex:sess-1")).toBe("codex resume sess-1");
  });

  it("generates traex resume command", () => {
    expect(buildResumeCommand("traex", "traex:sess-1")).toBe("traex resume sess-1");
    expect(buildResumeCommand("traex", "traex:run-1", { model: "gpt-5-codex" })).toBe(
      "traex resume run-1 -m gpt-5-codex",
    );
  });

  it("generates augure-code resume command with the vendor CLI name", () => {
    expect(buildResumeCommand("augure-code", "augure-code:sess-1")).toBe(
      "augure resume sess-1",
    );
    expect(buildResumeCommand("augure-code", "augure-code:run-1", { model: "ossington-5" })).toBe(
      "augure resume run-1 -m ossington-5",
    );
  });

  it("pins Claude and Codex models with shell quoting", () => {
    expect(buildResumeCommand("claude", "run-1", { model: "claude sonnet" })).toBe(
      "claude --resume run-1 --model 'claude sonnet'",
    );
    expect(buildResumeCommand("codex", "codex:run-1", { model: "o3-mini" })).toBe(
      "codex resume run-1 -m o3-mini",
    );
    expect(buildResumeCommand("claude", "run-1", { model: "x'$(command)" })).toBe(
      "claude --resume run-1 --model 'x'\"'\"'$(command)'",
    );
    expect(buildResumeCommand("codex", "codex:run-1", { model: "x'$(command)" })).toBe(
      "codex resume run-1 -m 'x'\"'\"'$(command)'",
    );
  });

  it("leaves unsupported and no-model commands unchanged", () => {
    expect(buildResumeCommand("gemini", "run-1", { model: "model-bearing-non-target" })).toBe(
      "gemini --resume run-1",
    );
    expect(buildResumeCommand("claude", "run-1", { model: "" })).toBe("claude --resume run-1");
  });

  it("generates gemini resume command", () => {
    expect(buildResumeCommand("gemini", "gemini:sess-2")).toBe("gemini --resume sess-2");
  });

  it("returns null for cursor (server-only resume)", () => {
    expect(buildResumeCommand("cursor", "cursor:chat-7")).toBeNull();
  });

  it("generates opencode resume command", () => {
    expect(buildResumeCommand("opencode", "opencode:s3")).toBe("opencode --session s3");
  });

  it("generates amp resume command", () => {
    expect(buildResumeCommand("amp", "amp:t-1")).toBe("amp --resume t-1");
  });

  it("generates kiro resume command", () => {
    expect(
      buildResumeCommand("kiro", "kiro:session-1"),
    ).toBe("kiro-cli chat --resume-id session-1");
  });

  it("returns null for pi (server-only resume)", () => {
    expect(buildResumeCommand("pi", "pi:session-1")).toBeNull();
  });

  it("strips agent prefix from compound IDs", () => {
    expect(buildResumeCommand("codex", "codex:my-session-id")).toBe("codex resume my-session-id");
  });

  it("handles plain IDs without prefix", () => {
    expect(buildResumeCommand("claude", "550e8400-e29b-41d4-a716-446655440000")).toBe(
      "claude --resume 550e8400-e29b-41d4-a716-446655440000",
    );
  });

  it("returns null for unsupported agents", () => {
    expect(buildResumeCommand("unknown", "id")).toBeNull();
  });

  it("generates copilot resume command", () => {
    expect(buildResumeCommand("copilot", "copilot:a108ddbe-acdb-42f4-a35e-6c2938bf038b")).toBe(
      "copilot --resume=a108ddbe-acdb-42f4-a35e-6c2938bf038b",
    );
  });

  describe("claude flags", () => {
    const id = "test-session";

    it("adds --dangerously-skip-permissions", () => {
      expect(
        buildResumeCommand("claude", id, {
          skipPermissions: true,
        }),
      ).toBe("claude --resume test-session --dangerously-skip-permissions");
    });

    it("adds --fork-session", () => {
      expect(
        buildResumeCommand("claude", id, {
          forkSession: true,
        }),
      ).toBe("claude --resume test-session --fork-session");
    });

    it("adds --print", () => {
      expect(buildResumeCommand("claude", id, { print: true })).toBe(
        "claude --resume test-session --print",
      );
    });

    it("combines multiple flags", () => {
      expect(
        buildResumeCommand("claude", id, {
          skipPermissions: true,
          forkSession: true,
          print: true,
        }),
      ).toBe("claude --resume test-session --dangerously-skip-permissions --fork-session --print");
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
    const cmd = buildResumeCommand("claude", "id with spaces");
    expect(cmd).toBe("claude --resume 'id with spaces'");
  });

  it("escapes single quotes in IDs using POSIX quoting", () => {
    const cmd = buildResumeCommand("claude", "it's a test");
    expect(cmd).toBe("claude --resume 'it'\"'\"'s a test'");
  });

  it("quotes shell metacharacters safely", () => {
    const cmd = buildResumeCommand("codex", "codex:$(whoami)");
    expect(cmd).toBe("codex resume '$(whoami)'");
  });

  it("quotes backtick injection attempts", () => {
    const cmd = buildResumeCommand("gemini", "gemini:`rm -rf /`");
    expect(cmd).toBe("gemini --resume '`rm -rf /`'");
  });

  it("quotes $VAR expansion attempts", () => {
    const cmd = buildResumeCommand("amp", "amp:$HOME/evil");
    expect(cmd).toBe("amp --resume '$HOME/evil'");
  });
});

describe("formatResumeResponseCommand", () => {
  it.each([
    ["claude", "cd '/remote/project' && claude --resume abc-123", "cd '/remote/project' && claude --resume abc-123"],
    ["kiro", "cd '/remote/project' && kiro-cli chat --resume-id abc-123", "cd '/remote/project' && kiro-cli chat --resume-id abc-123"],
    ["pi", "cd '/project' && pi --session session-1", "cd '/project' && pi --session session-1"],
    ["cursor", "cursor agent --resume abc-123", "cd '/remote/project' && cursor agent --resume abc-123"],
    ["codex", "codex resume abc-123", "codex resume abc-123"],
  ])("remote cwd formatting for %s", (agent, command, expected) => {
    expect(formatResumeResponseCommand(agent, { command, cwd: "/remote/project" })).toBe(expected);
  });
  it("keeps non-cursor backend commands unchanged", () => {
    expect(
      formatResumeResponseCommand("claude", {
        command: "claude --resume sess-1",
        cwd: "/tmp/project",
      }),
    ).toBe("claude --resume sess-1");
  });

  it("prepends cwd for cursor clipboard copy", () => {
    expect(
      formatResumeResponseCommand("cursor", {
        command: "cursor agent --resume chat-7 --workspace '/tmp/project'",
        cwd: "/tmp/project/frontend",
      }),
    ).toBe(
      "cd '/tmp/project/frontend' && " + "cursor agent --resume chat-7 --workspace '/tmp/project'",
    );
  });

  it("quotes cursor cwd when needed", () => {
    expect(
      formatResumeResponseCommand("cursor", {
        command: "cursor agent --resume chat-7 --workspace '/tmp/project dir'",
        cwd: "/tmp/project dir/frontend",
      }),
    ).toBe(
      "cd '/tmp/project dir/frontend' && " +
        "cursor agent --resume chat-7 --workspace '/tmp/project dir'",
    );
  });

  it("returns bare cursor command when cwd is unavailable", () => {
    expect(
      formatResumeResponseCommand("cursor", {
        command: "cursor agent --resume chat-7",
      }),
    ).toBe("cursor agent --resume chat-7");
  });

  it("returns null for missing backend command", () => {
    expect(formatResumeResponseCommand("cursor", null)).toBeNull();
  });
});
