import { describe, expect, it } from "vite-plus/test";
import { claudeCodeLink } from "./claude.js";

describe("claudeCodeLink", () => {
  it("opens Claude Code in the local session directory", () => {
    expect(claudeCodeLink("/tmp/project with spaces")).toBe(
      "claude://code/new?folder=%2Ftmp%2Fproject%20with%20spaces",
    );
  });

  it("opens Claude Code in its default directory without a path", () => {
    expect(claudeCodeLink(null)).toBe("claude://code/new");
  });
});
