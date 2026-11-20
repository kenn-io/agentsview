import { describe, expect, it } from "vite-plus/test";
import { documentTitle } from "./documentTitle.js";

const base = { needsAuth: false, sessionId: null, sessionNotFound: false };

describe("documentTitle", () => {
  it("names the sign-in screen while leaving settings accessible", () => {
    expect(documentTitle({ ...base, route: "activity", needsAuth: true })).toBe(
      "Authentication required · AgentsView",
    );
    expect(documentTitle({ ...base, route: "settings", needsAuth: true })).toBe(
      "Settings · AgentsView",
    );
  });

  it.each([
    ["codex:01900000-0000-7000-8000-000000000001", "Sessions · 00000001 · Codex · AgentsView"],
    ["codex:01900000-0000-7000-8000-000000000002", "Sessions · 00000002 · Codex · AgentsView"],
    [
      "codex:01900000-0000-7000-8000-000000000001_11111111-1111-4111-8111-111111111111",
      "Sessions · 11111111 · Codex · AgentsView",
    ],
    [
      "codex:01900000-0000-7000-8000-000000000001_22222222-2222-4222-8222-222222222222",
      "Sessions · 22222222 · Codex · AgentsView",
    ],
    [
      "kimi:workspace:01900000-0000-7000-8000-000000000001",
      "Sessions · 00000001 · Kimi · AgentsView",
    ],
    [
      "kimi:workspace:01900000-0000-7000-8000-000000000002",
      "Sessions · 00000002 · Kimi · AgentsView",
    ],
    [
      "source-a~codex:01900000-0000-7000-8000-000000000001",
      "Sessions · 00000001 · source-a · Codex · AgentsView",
    ],
    [
      "source-b~codex:01900000-0000-7000-8000-000000000001",
      "Sessions · 00000001 · source-b · Codex · AgentsView",
    ],
    ["01900000-0000-7000-8000-000000000001", "Sessions · 00000001 · Claude · AgentsView"],
  ])("identifies %s", (sessionId, expected) => {
    expect(documentTitle({ ...base, route: "sessions", sessionId })).toBe(expected);
  });

  it("keeps the route identity when a session is missing", () => {
    expect(
      documentTitle({
        ...base,
        route: "sessions",
        sessionId: "source-a~codex:12345678abcdef",
        sessionNotFound: true,
      }),
    ).toBe("Sessions · 78abcdef · source-a · Codex · Session not found · AgentsView");
  });

  it.each([
    [
      "codex:01900000-0000-7000-8000-000000000001",
      "Sessions · 00000001 · Codex · Session not found · AgentsView",
    ],
    [
      "codex:01900000-0000-7000-8000-000000000001_11111111-1111-4111-8111-111111111111",
      "Sessions · 11111111 · Codex · Session not found · AgentsView",
    ],
    [
      "codex:01900000-0000-7000-8000-000000000001_22222222-2222-4222-8222-222222222222",
      "Sessions · 22222222 · Codex · Session not found · AgentsView",
    ],
  ])("identifies missing session %s", (sessionId, expected) => {
    expect(documentTitle({ ...base, route: "sessions", sessionId, sessionNotFound: true })).toBe(
      expected,
    );
  });
});
