import { describe, expect, it } from "vite-plus/test";
import { documentTitle } from "./documentTitle.js";

const base = { route: "sessions" as const, needsAuth: false };

describe("documentTitle", () => {
  it.each([
    ["foobar", "Session (bf9cf968) · AgentsView"],
    ["é", "Session (1e9de8c1) · AgentsView"],
    ["hostA~abc", "Session (8e9ac634) · AgentsView"],
    ["hostB~abc", "Session (4376abb5) · AgentsView"],
    ["codebuff:project-a:2026-01-01T00:00:00.000Z", "Session (11ff51a8) · AgentsView"],
    ["codebuff:project-a:2026-01-02T00:00:00.000Z", "Session (b0f40315) · AgentsView"],
  ])("fingerprints the complete route session ID %s", (sessionId, expected) => {
    expect(documentTitle({ ...base, sessionId })).toBe(expected);
  });

  it("names the sign-in screen while leaving settings accessible", () => {
    expect(documentTitle({ route: "activity", needsAuth: true, sessionId: null })).toBe(
      "Authentication required · AgentsView",
    );
    expect(documentTitle({ route: "settings", needsAuth: true, sessionId: null })).toBe(
      "Settings · AgentsView",
    );
    expect(documentTitle({ route: "activity", needsAuth: false, sessionId: null })).toBe(
      "Activity · AgentsView",
    );
  });
});
