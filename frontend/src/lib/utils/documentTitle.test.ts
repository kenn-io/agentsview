import { describe, expect, it } from "vite-plus/test";
import type { Session } from "../api/types/core.js";
import type { Route } from "../stores/router.svelte.js";
import { documentTitle } from "./documentTitle.js";

const base = { needsAuth: false, sessionId: null, sessionNotFound: false };

describe("documentTitle", () => {
  it.each<[Route, string]>([
    ["sessions", "Sessions"],
    ["usage", "Usage"],
    ["token-usage", "Usage"],
    ["activity", "Activity"],
    ["recall", "Recall"],
    ["quality", "Quality"],
    ["friction", "Friction Log"],
    ["pinned", "Pinned"],
    ["trash", "Trash"],
    ["recent-edits", "Recent Edits"],
    ["data", "Data"],
    ["settings", "Settings"],
  ])("names the %s screen", (route, label) => {
    expect(documentTitle({ ...base, route })).toBe(`${label} · AgentsView`);
  });

  it("names a session using its name, first message, or project", () => {
    const session = {
      id: "host:12345678abcdef",
      display_name: "Fix\n login",
      first_message: "First\n prompt",
      project: "project-a",
    } as Session;
    const input = {
      ...base,
      route: "sessions" as const,
      sessionId: "host:12345678abcdef",
      session,
    };
    expect(documentTitle(input)).toBe("Fix login (12345678) · project-a · AgentsView");
    expect(documentTitle({ ...input, session: { ...session, display_name: undefined } })).toBe(
      "First prompt (12345678) · project-a · AgentsView",
    );
    expect(
      documentTitle({
        ...input,
        session: { ...session, display_name: undefined, first_message: null },
      }),
    ).toBe("project-a (12345678) · AgentsView");
    expect(
      documentTitle({
        ...input,
        session: { ...session, display_name: " ", first_message: null, project: " " },
      }),
    ).toBe("12345678 · AgentsView");
    expect(documentTitle({ ...input, session: { ...session, project: "" } })).toBe(
      "Fix login (12345678) · AgentsView",
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
    ).toBe("Fix the login flow (12345678) · project-a · AgentsView");
    expect(documentTitle({ ...input, sessionId: "host:other" })).toBe("other · AgentsView");
    expect(documentTitle({ ...input, sessionId: "host:other", sessionNotFound: true })).toBe(
      "Session not found (other) · AgentsView",
    );
  });

  it("names the sign-in screen while leaving settings accessible", () => {
    expect(documentTitle({ ...base, route: "activity", needsAuth: true })).toBe(
      "Authentication required · AgentsView",
    );
    expect(documentTitle({ ...base, route: "settings", needsAuth: true })).toBe(
      "Settings · AgentsView",
    );
  });
});
