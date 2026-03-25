// @vitest-environment jsdom
import {
  describe,
  it,
  expect,
  vi,
  afterEach,
} from "vitest";
import { mount, unmount, tick } from "svelte";
// @ts-ignore
import TopSessions from "./TopSessions.svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { router } from "../../stores/router.svelte.js";

describe("TopSessions", () => {
  afterEach(() => {
    analytics.includeOneShot = false;
    sessions.filters.includeOneShot = false;
  });

  function mountWithData() {
    analytics.topSessions = {
      metric: "messages",
      sessions: [
        {
          id: "sess-1",
          project: "proj",
          first_message: "hello",
          message_count: 10,
          duration_min: 5,
        },
      ],
    };
    // @ts-ignore — loading is reactive state
    analytics.loading = { ...analytics.loading, topSessions: false };
    // @ts-ignore
    analytics.errors = { ...analytics.errors, topSessions: null };

    return mount(TopSessions, { target: document.body });
  }

  it("sets includeOneShot filter on click when analytics has it enabled", async () => {
    analytics.includeOneShot = true;
    const component = mountWithData();
    await tick();

    const spy = vi.spyOn(sessions, "invalidateFilterCaches");
    const navSpy = vi.spyOn(router, "navigateToSession");

    const row = document.querySelector(".session-row");
    expect(row).toBeTruthy();
    row!.dispatchEvent(
      new MouseEvent("click", { bubbles: true }),
    );
    await tick();

    expect(sessions.filters.includeOneShot).toBe(true);
    expect(spy).toHaveBeenCalledOnce();
    expect(navSpy).toHaveBeenCalledWith("sess-1");

    spy.mockRestore();
    navSpy.mockRestore();
    unmount(component);
  });

  it("skips cache invalidation when filter already set", async () => {
    analytics.includeOneShot = true;
    sessions.filters.includeOneShot = true;
    const component = mountWithData();
    await tick();

    const spy = vi.spyOn(sessions, "invalidateFilterCaches");

    const row = document.querySelector(".session-row");
    row!.dispatchEvent(
      new MouseEvent("click", { bubbles: true }),
    );
    await tick();

    expect(spy).not.toHaveBeenCalled();

    spy.mockRestore();
    unmount(component);
  });

  it("does not set filter when analytics includeOneShot is off", async () => {
    analytics.includeOneShot = false;
    const component = mountWithData();
    await tick();

    const row = document.querySelector(".session-row");
    row!.dispatchEvent(
      new MouseEvent("click", { bubbles: true }),
    );
    await tick();

    expect(sessions.filters.includeOneShot).toBe(false);

    unmount(component);
  });
});
