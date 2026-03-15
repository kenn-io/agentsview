// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import { mount, unmount, tick } from "svelte";
// @ts-ignore
import SessionBreadcrumb from "./SessionBreadcrumb.svelte";
import type { Session } from "../../api/types.js";

function makeSession(agent: string): Session {
  return {
    id: "run:123456789abcdef",
    project: "proj-a",
    machine: "mac",
    agent,
    first_message: "hello",
    started_at: "2026-02-20T12:30:00Z",
    ended_at: "2026-02-20T12:31:00Z",
    message_count: 2,
    user_message_count: 1,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    created_at: "2026-02-20T12:30:00Z",
  };
}

describe("SessionBreadcrumb", () => {
  it("renders gemini with rose badge color", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("gemini"),
        onBack: () => {},
      },
    });

    await tick();
    const badge = document.querySelector(".agent-badge");
    expect(badge).toBeTruthy();
    expect(badge?.getAttribute("style")).toContain(
      "var(--accent-rose)",
    );

    unmount(component);
  });

  it("falls back to blue for unknown agents", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("unknown"),
        onBack: () => {},
      },
    });

    await tick();
    const badge = document.querySelector(".agent-badge");
    expect(badge?.getAttribute("style")).toContain(
      "var(--accent-blue)",
    );

    unmount(component);
  });
});
