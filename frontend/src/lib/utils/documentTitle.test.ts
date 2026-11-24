import { describe, expect, it } from "vite-plus/test";
import type { Route } from "../stores/router.svelte.js";
import { documentTitle } from "./documentTitle.js";

const base = { route: "sessions" as const, needsAuth: false };

describe("documentTitle", () => {
  it.each<[Route, string]>([
    ["sessions", "Sessions · Agentic HQ"],
    ["usage", "Usage · Agentic HQ"],
    ["token-usage", "Usage · Agentic HQ"],
    ["activity", "Activity · Agentic HQ"],
    ["recall", "Recall · Agentic HQ"],
    ["quality", "Quality · Agentic HQ"],
    ["friction", "Friction Log · Agentic HQ"],
    ["pinned", "Pinned · Agentic HQ"],
    ["trash", "Trash · Agentic HQ"],
    ["recent-edits", "Recent Edits · Agentic HQ"],
    ["data", "Data · Agentic HQ"],
    ["settings", "Settings · Agentic HQ"],
  ])("names the %s route", (route, expected) => {
    expect(documentTitle({ route, needsAuth: false, sessionId: null })).toBe(expected);
  });

  it.each([
    ["foobar", "Session (bf9cf968) · Agentic HQ"],
    ["é", "Session (1e9de8c1) · Agentic HQ"],
    ["hostA~abc", "Session (8e9ac634) · Agentic HQ"],
    ["hostB~abc", "Session (4376abb5) · Agentic HQ"],
    ["codebuff:project-a:2026-01-01T00:00:00.000Z", "Session (11ff51a8) · Agentic HQ"],
    ["codebuff:project-a:2026-01-02T00:00:00.000Z", "Session (b0f40315) · Agentic HQ"],
  ])("fingerprints the complete route session ID %s", (sessionId, expected) => {
    expect(documentTitle({ ...base, sessionId })).toBe(expected);
  });

  it("names the sign-in screen while leaving settings accessible", () => {
    expect(documentTitle({ route: "activity", needsAuth: true, sessionId: null })).toBe(
      "Authentication required · Agentic HQ",
    );
    expect(documentTitle({ route: "settings", needsAuth: true, sessionId: null })).toBe(
      "Settings · Agentic HQ",
    );
    expect(documentTitle({ route: "activity", needsAuth: false, sessionId: null })).toBe(
      "Activity · Agentic HQ",
    );
  });
});
