// @vitest-environment jsdom
import { describe, it, expect, afterEach } from "vitest";
import { mount, unmount, tick } from "svelte";
// @ts-ignore
import TerminationBanner from "./TerminationBanner.svelte";
import type { Session } from "../../api/types.js";

function makeSession(
  termination_status: string | null | undefined,
): Session {
  return {
    id: "host~abc-123",
    project: "proj-a",
    machine: "host",
    agent: "claude",
    first_message: "hello",
    started_at: "2026-02-20T12:30:00Z",
    ended_at: "2026-02-20T12:31:00Z",
    message_count: 2,
    user_message_count: 1,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    is_automated: false,
    created_at: "2026-02-20T12:30:00Z",
    termination_status,
  };
}

afterEach(() => {
  document.body.innerHTML = "";
});

describe("TerminationBanner", () => {
  it("shows the unclean banner when termination_status is tool_call_pending", async () => {
    const component = mount(TerminationBanner, {
      target: document.body,
      props: {
        session: makeSession("tool_call_pending"),
      },
    });

    await tick();
    const banner = document.querySelector(".termination-banner");
    expect(banner).toBeTruthy();
    expect(banner?.getAttribute("data-status")).toBe(
      "tool_call_pending",
    );
    const normalized = (banner?.textContent ?? "")
      .replace(/\s+/g, " ")
      .trim();
    expect(normalized).toContain(
      "tool call that never received a response",
    );

    unmount(component);
  });

  it("shows the truncated banner when termination_status is truncated", async () => {
    const component = mount(TerminationBanner, {
      target: document.body,
      props: { session: makeSession("truncated") },
    });

    await tick();
    const banner = document.querySelector(".termination-banner");
    expect(banner).toBeTruthy();
    expect(banner?.getAttribute("data-status")).toBe("truncated");
    const normalized = (banner?.textContent ?? "")
      .replace(/\s+/g, " ")
      .trim();
    expect(normalized).toContain("ends mid-write");

    unmount(component);
  });

  it("renders nothing for clean sessions", async () => {
    const component = mount(TerminationBanner, {
      target: document.body,
      props: { session: makeSession("clean") },
    });

    await tick();
    expect(document.querySelector(".termination-banner")).toBeNull();

    unmount(component);
  });

  it("renders nothing when termination_status is null", async () => {
    const component = mount(TerminationBanner, {
      target: document.body,
      props: { session: makeSession(null) },
    });

    await tick();
    expect(document.querySelector(".termination-banner")).toBeNull();

    unmount(component);
  });

  it("renders nothing when session is null", async () => {
    const component = mount(TerminationBanner, {
      target: document.body,
      props: { session: null },
    });

    await tick();
    expect(document.querySelector(".termination-banner")).toBeNull();

    unmount(component);
  });
});
