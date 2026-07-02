// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import SessionsTable from "./SessionsTable.svelte";
import { m } from "../../i18n/index.js";
import { router } from "../../stores/router.svelte.js";
import type { Report } from "../../api/types.js";
import type { ActivitySessionRow } from "../../api/generated/index";

function makeRow(
  overrides: Partial<ActivitySessionRow> = {},
): ActivitySessionRow {
  return {
    session_id: "sess",
    title: "Session",
    project: "proj",
    agent: "claude",
    primary_model: "opus",
    models: ["opus"],
    agent_minutes: 10,
    cost: 1,
    output_tokens: 0,
    first_active: "2026-06-16T08:00:00Z",
    last_active: "2026-06-16T09:00:00Z",
    timing_quality: "high",
    is_automated: false,
    ...overrides,
  };
}

function makeReport(rows: ActivitySessionRow[]): Report {
  return {
    peak: { agents: 0, at: null },
    totals: {
      active_minutes: 0,
      idle_minutes: 0,
      agent_minutes: 0,
      sessions: rows.length,
      untimed_sessions: 0,
      distinct_projects: 0,
      distinct_models: 0,
      output_tokens: 0,
      cost: 0,
    },
    partial: false,
    as_of: null,
    timezone: "UTC",
    range_start: "2026-06-16T00:00:00Z",
    range_end: "2026-06-17T00:00:00Z",
    bucket_unit: "hour",
    effective_end: "2026-06-17T00:00:00Z",
    bucket_seconds: 10800,
    bucket_count: 8,
    elapsed_bucket_count: 8,
    buckets: null,
    by_project: null,
    by_model: null,
    by_agent: null,
    by_session: rows,
    intervals: null,
  } as Report;
}

// Two timed rows with distinct agent_minutes/cost orderings plus
// one untimed row, so default (minutes) and cost sorts differ and
// the untimed row's placement is observable.
function fixtureRows(): ActivitySessionRow[] {
  return [
    makeRow({
      session_id: "low-min",
      title: "Low minutes high cost",
      agent_minutes: 5,
      cost: 9,
    }),
    makeRow({
      session_id: "high-min",
      title: "High minutes low cost",
      agent_minutes: 40,
      cost: 1,
    }),
    makeRow({
      session_id: "untimed",
      title: "Untimed",
      agent_minutes: null,
      cost: 4,
      first_active: null,
      last_active: null,
    }),
  ];
}

function rowOrder(): string[] {
  return [...document.querySelectorAll(".session-row")].map(
    (el) => el.getAttribute("data-session-id") ?? "",
  );
}

function minutesCells(): string[] {
  return [...document.querySelectorAll(".session-row .col-minutes")].map(
    (el) => el.textContent?.trim() ?? "",
  );
}

describe("SessionsTable", () => {
  afterEach(() => {
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("defaults to agent-minutes desc with untimed rows last", async () => {
    const report = makeReport(fixtureRows());
    const c = mount(SessionsTable, {
      target: document.body,
      props: { report },
    });
    await tick();

    expect(rowOrder()).toEqual(["high-min", "low-min", "untimed"]);

    unmount(c);
  });

  it("renders an em dash for an untimed row's minutes cell", async () => {
    const report = makeReport(fixtureRows());
    const c = mount(SessionsTable, {
      target: document.body,
      props: { report },
    });
    await tick();

    // Third row (untimed) shows the placeholder; timed rows show numbers.
    const cells = minutesCells();
    expect(cells[2]).toBe("—");
    expect(cells[0]).not.toBe("—");

    unmount(c);
  });

  it("sorts the untimed row by its real cost when the Cost header is clicked", async () => {
    const report = makeReport(fixtureRows());
    const c = mount(SessionsTable, {
      target: document.body,
      props: { report },
    });
    await tick();

    const costHeader = document.querySelector(
      "th.sort-cost button",
    ) as HTMLElement | null;
    expect(costHeader).toBeTruthy();
    costHeader!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await tick();

    // Cost desc over the full set: low-min (9), untimed (4), high-min (1).
    // The untimed row carries real cost, so it participates in the cost
    // sort instead of being pinned last.
    expect(rowOrder()).toEqual(["low-min", "untimed", "high-min"]);

    unmount(c);
  });

  it("intercepts a plain left-click on a session link for SPA navigation", async () => {
    const navSpy = vi
      .spyOn(router, "navigateToSession")
      .mockImplementation(() => {});
    const report = makeReport(fixtureRows());
    const c = mount(SessionsTable, {
      target: document.body,
      props: { report },
    });
    await tick();

    // Default order is high-min first, so the first link is its row.
    const link = document.querySelector(
      ".session-row .session-link",
    ) as HTMLAnchorElement | null;
    expect(link).toBeTruthy();

    // Real browser clicks are cancelable; preventDefault is a no-op
    // (and defaultPrevented stays false) on a non-cancelable event.
    const click = new MouseEvent("click", {
      bubbles: true,
      cancelable: true,
      button: 0,
    });
    link!.dispatchEvent(click);
    await tick();

    expect(navSpy).toHaveBeenCalledWith("high-min");
    expect(click.defaultPrevented).toBe(true);

    unmount(c);
  });

  it("restricts rows to the active session ids when filtered", async () => {
    const report = makeReport(fixtureRows());
    const c = mount(SessionsTable, {
      target: document.body,
      props: { report, filterIds: ["high-min"], filterLabel: "06:00–09:00" },
    });
    await tick();

    expect(rowOrder()).toEqual(["high-min"]);
    expect(document.querySelector(".count")?.textContent).toContain("1 total");

    unmount(c);
  });

  it("shows a dismissible filter badge that calls onClearFilter", async () => {
    const onClearFilter = vi.fn();
    const report = makeReport(fixtureRows());
    const c = mount(SessionsTable, {
      target: document.body,
      props: {
        report,
        filterIds: ["high-min"],
        filterLabel: "06:00–09:00",
        onClearFilter,
      },
    });
    await tick();

    const badge = document.querySelector(
      ".filter-badge",
    ) as HTMLButtonElement | null;
    expect(badge).toBeTruthy();
    expect(badge!.textContent).toContain("06:00–09:00");
    badge!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await tick();
    expect(onClearFilter).toHaveBeenCalledTimes(1);

    unmount(c);
  });

  it("shows a filter-aware empty message for a slot with no matches", async () => {
    const report = makeReport(fixtureRows());
    // An empty (but non-null) id list is an active filter that matches nothing,
    // e.g. an idle slot was clicked. The badge stays so it can be cleared.
    const c = mount(SessionsTable, {
      target: document.body,
      props: { report, filterIds: [], filterLabel: "12:00–15:00" },
    });
    await tick();

    expect(document.querySelectorAll(".session-row").length).toBe(0);
    expect(document.querySelector(".empty")?.textContent).toContain(
      m.activity_no_sessions_selected_slot(),
    );
    expect(document.querySelector(".filter-badge")).toBeTruthy();

    unmount(c);
  });

  it("flags only automated sessions with an Auto badge", async () => {
    const report = makeReport([
      makeRow({ session_id: "human", title: "Human", is_automated: false }),
      makeRow({ session_id: "robot", title: "Robot", is_automated: true }),
    ]);
    const c = mount(SessionsTable, {
      target: document.body,
      props: { report },
    });
    await tick();

    expect(document.querySelectorAll(".auto-badge").length).toBe(1);
    const robotRow = document.querySelector(
      '.session-row[data-session-id="robot"]',
    );
    const humanRow = document.querySelector(
      '.session-row[data-session-id="human"]',
    );
    expect(robotRow?.querySelector(".auto-badge")).toBeTruthy();
    expect(humanRow?.querySelector(".auto-badge")).toBeNull();

    unmount(c);
  });

  it("renders no filter badge when no slot filter is active", async () => {
    const report = makeReport(fixtureRows());
    const c = mount(SessionsTable, {
      target: document.body,
      props: { report },
    });
    await tick();

    expect(document.querySelector(".filter-badge")).toBeNull();

    unmount(c);
  });
});
