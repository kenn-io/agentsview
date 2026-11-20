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
    expect(
      documentTitle({
        ...input,
        session: { ...session, display_name: " ", first_message: null, project: " " },
      }),
    ).toBe("source-a:12345678 · AgentsView");
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

  it.each([
    [
      "source-a~codex:12345678abcdef",
      "source-b~codex:12345678abcdef",
      [
        [
          "Fix login (source-a:12345678) · project-a · AgentsView",
          "source-a:12345678 · AgentsView",
          "Session not found (source-a:12345678) · AgentsView",
        ],
        [
          "Fix login (source-b:12345678) · project-a · AgentsView",
          "source-b:12345678 · AgentsView",
          "Session not found (source-b:12345678) · AgentsView",
        ],
      ],
    ],
  ] as const)("distinguishes sessions %s and %s in every state", (first, second, expected) => {
    const titles = [first, second].map((id) => {
      const input = { ...base, route: "sessions" as const, sessionId: id };
      const session = {
        id,
        display_name: "Fix login",
        project: "project-a",
        first_message: null,
      } as Session;
      return [
        documentTitle({ ...input, session }),
        documentTitle(input),
        documentTitle({ ...input, sessionNotFound: true }),
      ];
    });
    expect(titles).toEqual(expected);
    for (let i = 0; i < 3; i++) expect(titles[0]![i]).not.toBe(titles[1]![i]);
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
    ["codex:12345678abcdef", "12345678 · AgentsView"],
    ["source-a~12345678abcdef", "source-a:12345678 · AgentsView"],
  ])("shows the breadcrumb ID for %s while loading", (sessionId, expected) => {
    expect(documentTitle({ ...base, route: "sessions", sessionId })).toBe(expected);
  });
});
