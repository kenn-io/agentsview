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
    ["codex:01900000-0000-7000-8000-000000000001", "Sessions · Codex · 000000000001 · AgentsView"],
    ["codex:01900000-0000-7000-8000-000000000002", "Sessions · Codex · 000000000002 · AgentsView"],
    [
      "codex:01900000-0000-7000-8000-000000000001_11111111-1111-4111-8111-111111111111",
      "Sessions · Codex · 111111111111 · AgentsView",
    ],
    [
      "codex:01900000-0000-7000-8000-000000000001_22222222-2222-4222-8222-222222222222",
      "Sessions · Codex · 222222222222 · AgentsView",
    ],
    [
      "kimi:workspace:01900000-0000-7000-8000-000000000001",
      "Sessions · Kimi · 000000000001 · AgentsView",
    ],
    [
      "kimi:workspace:01900000-0000-7000-8000-000000000002",
      "Sessions · Kimi · 000000000002 · AgentsView",
    ],
    [
      "source-a~codex:01900000-0000-7000-8000-000000000001",
      "Sessions · source-a · Codex · 000000000001 · AgentsView",
    ],
    [
      "source-b~codex:01900000-0000-7000-8000-000000000001",
      "Sessions · source-b · Codex · 000000000001 · AgentsView",
    ],
    ["01900000-0000-7000-8000-000000000001", "Sessions · Claude · 000000000001 · AgentsView"],
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
    ).toBe("Sessions · Session not found · source-a · Codex · 345678abcdef · AgentsView");
  });
});
