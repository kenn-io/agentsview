import { describe, expect, it } from "vite-plus/test";
import type { Session } from "../api/types/core.js";
import { documentTitle } from "./documentTitle.js";

const base = { needsAuth: false, sessionId: null, sessionNotFound: false };

describe("documentTitle", () => {
  it("names a session using its name, first message, or project", () => {
    const session = {
      id: "source-a~codex:12345678abcdef",
      display_name: "Fix\n login",
      first_message: "First\n prompt",
      project: "project-a",
    } as Session;
    const input = {
      ...base,
      route: "sessions" as const,
      sessionId: "source-a~codex:12345678abcdef",
      session,
    };
    expect(documentTitle(input)).toBe("Fix login (source-a:12345678) · project-a · AgentsView");
    expect(documentTitle({ ...input, session: { ...session, display_name: undefined } })).toBe(
      "First prompt (source-a:12345678) · project-a · AgentsView",
    );
    expect(
      documentTitle({
        ...input,
        session: { ...session, display_name: undefined, first_message: null },
      }),
    ).toBe("project-a (source-a:12345678) · AgentsView");
    expect(documentTitle({ ...input, session: { ...session, project: "" } })).toBe(
      "Fix login (source-a:12345678) · AgentsView",
    );
    expect(
      documentTitle({
        ...input,
        session: {
          ...session,
          display_name: undefined,
          first_message:
            "<teammate-message>You are a teammate on a team. Work together. Task #2: Fix the login flow 3. Report back</teammate-message>",
        },
      }),
    ).toBe("Fix the login flow (source-a:12345678) · project-a · AgentsView");
  });

  it("names the sign-in screen while leaving settings accessible", () => {
    expect(documentTitle({ ...base, route: "activity", needsAuth: true })).toBe(
      "Authentication required · AgentsView",
    );
    expect(documentTitle({ ...base, route: "settings", needsAuth: true })).toBe(
      "Settings · AgentsView",
    );
  });

  it.each([
    ["codex:12345678abcdef", "12345678"],
    ["source-a~codex:12345678abcdef", "source-a:12345678"],
  ])("shows the short ID for %s while loading or missing", (sessionId, id) => {
    const input = { ...base, route: "sessions" as const, sessionId };
    expect(documentTitle(input)).toBe(`${id} · AgentsView`);
    expect(documentTitle({ ...input, sessionNotFound: true })).toBe(
      `Session not found (${id}) · AgentsView`,
    );
  });
});
