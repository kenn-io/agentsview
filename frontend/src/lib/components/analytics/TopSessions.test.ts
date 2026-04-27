// @vitest-environment jsdom
import {
  describe,
  it,
  expect,
  vi,
  afterEach,
  beforeEach,
  type MockInstance,
} from "vitest";
import { mount, unmount, tick } from "svelte";
// @ts-ignore
import TopSessions from "./TopSessions.svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { router } from "../../stores/router.svelte.js";

describe("TopSessions", () => {
  let cacheSpy: MockInstance;
  let navSpy: MockInstance;

  beforeEach(() => {
    cacheSpy = vi
      .spyOn(sessions, "invalidateFilterCaches")
      .mockImplementation(() => {});
    navSpy = vi
      .spyOn(router, "navigateToSession")
      .mockImplementation(() => {});
  });

  let savedLoading: typeof analytics.loading;
  let savedErrors: typeof analytics.errors;

  beforeEach(() => {
    savedLoading = { ...analytics.loading };
    savedErrors = { ...analytics.errors };
  });

  afterEach(() => {
    cacheSpy.mockRestore();
    navSpy.mockRestore();
    analytics.includeOneShot = false;
    analytics.topSessions = null;
    // @ts-ignore
    analytics.loading = savedLoading;
    // @ts-ignore
    analytics.errors = savedErrors;
    sessions.filters.includeOneShot = false;
    window.history.replaceState(null, "", "/");
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
          output_tokens: 0,
          duration_min: 5,
        },
      ],
    };
    // @ts-ignore — loading is reactive state
    analytics.loading = {
      ...analytics.loading,
      topSessions: false,
    };
    // @ts-ignore
    analytics.errors = {
      ...analytics.errors,
      topSessions: null,
    };

    return mount(TopSessions, { target: document.body });
  }

  function clickRow() {
    const row = document.querySelector(".session-row");
    expect(row).toBeTruthy();
    row!.dispatchEvent(
      new MouseEvent("click", { bubbles: true }),
    );
  }

  it("sets filter and navigates when analytics includeOneShot is enabled", async () => {
    analytics.includeOneShot = true;
    sessions.filters.includeOneShot = false;
    const component = mountWithData();
    await tick();

    clickRow();
    await tick();

    expect(sessions.filters.includeOneShot).toBe(true);
    expect(cacheSpy).toHaveBeenCalledOnce();
    expect(navSpy).toHaveBeenCalledWith("sess-1");

    unmount(component);
  });

  it("skips invalidation but still navigates when filter already set", async () => {
    analytics.includeOneShot = true;
    sessions.filters.includeOneShot = true;
    const component = mountWithData();
    await tick();

    clickRow();
    await tick();

    expect(cacheSpy).not.toHaveBeenCalled();
    expect(navSpy).toHaveBeenCalledWith("sess-1");

    unmount(component);
  });

  it("navigates without setting filter when analytics includeOneShot is off", async () => {
    analytics.includeOneShot = false;
    const component = mountWithData();
    await tick();

    clickRow();
    await tick();

    expect(sessions.filters.includeOneShot).toBe(false);
    expect(cacheSpy).not.toHaveBeenCalled();
    expect(navSpy).toHaveBeenCalledWith("sess-1");

    unmount(component);
  });
});
