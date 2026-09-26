// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, tick } from "svelte";
import OutcomeTotals from "./OutcomeTotals.svelte";
import type { DbStatsOutcomeStats } from "../../api/generated/index.js";

// The API has always been able to return git and GitHub outcomes, and no page
// ever asked for them, so commits and pull requests existed on the command
// line only. These tests pin what the page shows once it does ask.

async function render(props: {
  stats: DbStatsOutcomeStats | null;
  loading?: boolean;
  error?: string | null;
  includePullRequests?: boolean;
  onIncludePullRequests?: () => void;
}): Promise<HTMLElement> {
  const target = document.createElement("div");
  document.body.appendChild(target);
  mount(OutcomeTotals, {
    target,
    props: {
      loading: false,
      error: null,
      includePullRequests: false,
      onIncludePullRequests: () => {},
      ...props,
    },
  });
  await tick();
  return target;
}

function metric(target: HTMLElement, label: string): string {
  const card = [...target.querySelectorAll(".outcome-card")].find(
    (element) => element.querySelector(".outcome-label")?.textContent?.trim() === label,
  );
  return card?.querySelector(".outcome-value")?.textContent?.trim() ?? "";
}

describe("OutcomeTotals", () => {
  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("shows the commit totals the API returns", async () => {
    const target = await render({
      stats: {
        repos_active: 12,
        commits: 1254,
        loc_added: 54321,
        loc_removed: 1823,
        files_changed: 127,
      },
    });
    expect(metric(target, "Repositories")).toBe("12");
    expect(metric(target, "Commits")).toBe("1,254");
    expect(metric(target, "Lines added")).toBe("54,321");
    expect(metric(target, "Lines removed")).toBe("1,823");
    expect(metric(target, "Files changed")).toBe("127");
  });

  it("shows pull-request totals only once they were requested", async () => {
    const withoutPRs = await render({
      stats: { repos_active: 1, commits: 2, loc_added: 3, loc_removed: 4, files_changed: 5 },
    });
    expect(metric(withoutPRs, "Pull requests opened")).toBe("");
    expect(withoutPRs.querySelector(".outcome-load-prs")).not.toBeNull();

    document.body.innerHTML = "";
    const withPRs = await render({
      includePullRequests: true,
      stats: {
        repos_active: 1,
        commits: 2,
        loc_added: 3,
        loc_removed: 4,
        files_changed: 5,
        prs_opened: 25,
        prs_merged: 21,
      },
    });
    expect(metric(withPRs, "Pull requests opened")).toBe("25");
    expect(metric(withPRs, "Pull requests merged")).toBe("21");
    expect(withPRs.querySelector(".outcome-load-prs")).toBeNull();
  });

  it("names the repositories a lookup could not read", async () => {
    const target = await render({
      stats: {
        repos_active: 2,
        commits: 84,
        loc_added: 0,
        loc_removed: 0,
        files_changed: 0,
        skipped: [
          { repo: "/repos/first", op: "pr", reason: "no git remotes found" },
          { repo: "/repos/second", op: "log", reason: "signal: killed" },
        ],
      },
    });
    const partial = target.querySelector(".outcome-partial");
    expect(partial).not.toBeNull();
    expect(partial?.textContent).toContain("/repos/first");
    expect(partial?.textContent).toContain("no git remotes found");
    expect(partial?.textContent).toContain("/repos/second");
    expect(partial?.textContent).toContain("signal: killed");
  });

  it("stays silent about partial totals when nothing was skipped", async () => {
    const target = await render({
      stats: { repos_active: 2, commits: 84, loc_added: 0, loc_removed: 0, files_changed: 0 },
    });
    expect(target.querySelector(".outcome-partial")).toBeNull();
  });

  it("reports an error instead of showing zeros", async () => {
    const target = await render({ stats: null, error: "daemon unreachable" });
    expect(target.textContent).toContain("daemon unreachable");
    expect(metric(target, "Commits")).toBe("");
  });

  it("says the window has no git activity rather than showing zeros", async () => {
    const target = await render({ stats: null });
    expect(target.querySelector(".outcome-empty")).not.toBeNull();
    expect(metric(target, "Commits")).toBe("");
  });
});
