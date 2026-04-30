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
    sessions.filters.termination = "";
    window.history.replaceState(null, "", "/");
    // Clear any orphaned DOM left behind by tests that fail
    // before their unmount call.
    document.body.innerHTML = "";
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

  describe("status column", () => {
    function mountWithFourStates() {
      analytics.topSessions = {
        metric: "messages",
        sessions: [
          {
            id: "s1",
            project: "p",
            first_message: "clean session",
            message_count: 1,
            output_tokens: 0,
            duration_min: 0,
            termination_status: "clean",
          },
          {
            id: "s2",
            project: "p",
            first_message: "tcp session",
            message_count: 1,
            output_tokens: 0,
            duration_min: 0,
            termination_status: "tool_call_pending",
          },
          {
            id: "s3",
            project: "p",
            first_message: "trunc session",
            message_count: 1,
            output_tokens: 0,
            duration_min: 0,
            termination_status: "truncated",
          },
          {
            id: "s4",
            project: "p",
            first_message: "unknown session",
            message_count: 1,
            output_tokens: 0,
            duration_min: 0,
            termination_status: null,
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

    it("renders distinct status indicators per termination_status", async () => {
      const component = mountWithFourStates();
      await tick();

      // Two flagged statuses (tool_call_pending + truncated)
      // resolve to "unclean" once past the active window —
      // the sessions in this fixture have no timestamps, so
      // age comparisons fail and they fall straight into the
      // unclean tier.
      expect(
        document.querySelectorAll(".status-dot--unclean").length,
      ).toBe(2);

      // The clean and NULL sessions resolve to "quiet" — a
      // transparent dot is still rendered so the layout column
      // stays consistent.
      expect(
        document.querySelectorAll(".status-dot--quiet").length,
      ).toBe(2);

      unmount(component);
    });

    it("shows the unclean count pill and triggers termination filter on click", async () => {
      const setSpy = vi
        .spyOn(sessions, "setTerminationFilter")
        .mockImplementation(() => {});

      const component = mountWithFourStates();
      await tick();

      const pill = document.querySelector(
        ".status-count-pill",
      ) as HTMLButtonElement | null;
      expect(pill).toBeTruthy();
      expect(pill!.textContent?.trim()).toBe("2 unclean");

      pill!.dispatchEvent(
        new MouseEvent("click", { bubbles: true }),
      );
      await tick();

      expect(setSpy).toHaveBeenCalledWith("unclean");

      setSpy.mockRestore();
      unmount(component);
    });

    it("hides the count pill when no rows are unclean", async () => {
      analytics.topSessions = {
        metric: "messages",
        sessions: [
          {
            id: "s1",
            project: "p",
            first_message: "clean only",
            message_count: 1,
            output_tokens: 0,
            duration_min: 0,
            termination_status: "clean",
          },
        ],
      };
      // @ts-ignore
      analytics.loading = {
        ...analytics.loading,
        topSessions: false,
      };
      // @ts-ignore
      analytics.errors = {
        ...analytics.errors,
        topSessions: null,
      };

      const component = mount(TopSessions, {
        target: document.body,
      });
      await tick();

      expect(
        document.querySelector(".status-count-pill"),
      ).toBeNull();

      unmount(component);
    });
  });
});
