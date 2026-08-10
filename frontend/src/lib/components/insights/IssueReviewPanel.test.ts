// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { IssueReviewResponse } from "../../api/types.js";

const mocks = vi.hoisted(() => ({ getIssueReview: vi.fn() }));

vi.mock("../../api/generated/index.js", async (importOriginal) => {
  const original = await importOriginal<typeof import("../../api/generated/index.js")>();
  return {
    ...original,
    AnalyticsService: {
      getApiV1AnalyticsIssueReview: mocks.getIssueReview,
    },
  };
});

// @ts-ignore
import IssueReviewPanel from "./IssueReviewPanel.svelte";

const FILTER_STORAGE_KEY = "agentsview.issue-review.filters.v1";
const SAVED_VIEWS_STORAGE_KEY = "agentsview.issue-review.saved-views.v1";

function filters(overrides: Record<string, string> = {}) {
  return {
    sessionId: "",
    folder: "",
    category: "",
    tool: "",
    source: "",
    outcome: "",
    severity: "",
    confidence: "",
    status: "",
    recommendationType: "",
    minOccurrences: "2",
    minSessions: "2",
    minProjects: "0",
    minWastedMs: "0",
    sort: "impact",
    ...overrides,
  };
}

const response: IssueReviewResponse = {
  generated_at: "2026-08-10T00:00:00Z",
  scanned_sessions: 0,
  scanned_messages: 0,
  scanned_tool_calls: 0,
  analyzed_messages: 0,
  analyzed_tool_calls: 0,
  duplicate_messages: 0,
  duplicate_tool_calls: 0,
  scanned_telemetry: 0,
  telemetry_status: "available",
  total_findings: 0,
  truncated: false,
  findings: [],
  facets: {
    category: [],
    tool: [],
    source: [],
    severity: [
      { value: "high", label: "High", count: 2 },
      { value: "medium", label: "Medium", count: 1 },
    ],
    confidence: [],
    status: [],
    recommendation_type: [],
    session: [],
    folder: [],
    outcome: [],
  },
};

let component: ReturnType<typeof mount> | undefined;

async function settle() {
  await tick();
  await Promise.resolve();
  await tick();
}

async function mountPanel() {
  component = mount(IssueReviewPanel, { target: document.body });
  await settle();
}

function button(label: string): HTMLButtonElement {
  const match = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
    (item) => item.textContent?.trim() === label,
  );
  expect(match).toBeDefined();
  return match!;
}

async function choose(label: string, optionLabel: string) {
  const trigger = document.querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`);
  expect(trigger).not.toBeNull();
  trigger!.click();
  await tick();
  const option = [...document.querySelectorAll<HTMLElement>(".kit-typeahead__option")].find(
    (item) => item.textContent?.trim() === optionLabel,
  );
  expect(option).toBeDefined();
  option!.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
  await settle();
}

async function nameView(name: string) {
  const input = document.querySelector<HTMLInputElement>('input[aria-label="View name"]');
  expect(input).not.toBeNull();
  input!.value = name;
  input!.dispatchEvent(new InputEvent("input", { bubbles: true, data: name }));
  await settle();
}

beforeEach(() => {
  localStorage.clear();
  mocks.getIssueReview.mockReset().mockResolvedValue(response);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  document.body.innerHTML = "";
  localStorage.clear();
  vi.restoreAllMocks();
});

describe("IssueReviewPanel saved views", () => {
  it("saves, restores, and applies a named filter view", async () => {
    await mountPanel();
    await choose("Severity", "High (2)");
    await nameView("Critical");
    button("Save view").click();
    await settle();

    expect(JSON.parse(localStorage.getItem(SAVED_VIEWS_STORAGE_KEY)!)).toEqual([
      { name: "Critical", filters: filters({ severity: "high" }) },
    ]);

    await unmount(component!);
    component = undefined;
    document.body.innerHTML = "";
    localStorage.removeItem(FILTER_STORAGE_KEY);
    mocks.getIssueReview.mockClear();
    await mountPanel();
    await choose("Saved view", "Critical");

    expect(mocks.getIssueReview).toHaveBeenLastCalledWith(
      expect.objectContaining({ severity: "high" }),
    );
  });

  it("overwrites and deletes a selected view", async () => {
    await mountPanel();
    await choose("Severity", "High (2)");
    await nameView("Triage");
    button("Save view").click();
    await settle();

    await choose("Severity", "Medium (1)");
    button("Save view").click();
    await settle();

    expect(JSON.parse(localStorage.getItem(SAVED_VIEWS_STORAGE_KEY)!)).toEqual([
      { name: "Triage", filters: filters({ severity: "medium" }) },
    ]);

    button("Delete view").click();
    await settle();
    expect(JSON.parse(localStorage.getItem(SAVED_VIEWS_STORAGE_KEY)!)).toEqual([]);
  });

  it("removes invalid saved-view storage", async () => {
    localStorage.setItem(SAVED_VIEWS_STORAGE_KEY, "{}");
    await mountPanel();

    expect(localStorage.getItem(SAVED_VIEWS_STORAGE_KEY)).toBeNull();
    expect(button("Delete view").disabled).toBe(true);
  });

  it("caps restored and newly saved views at 50", async () => {
    localStorage.setItem(
      SAVED_VIEWS_STORAGE_KEY,
      JSON.stringify(
        Array.from({ length: 51 }, (_, index) => ({
          name: `View ${index + 1}`,
          filters: filters(),
        })),
      ),
    );
    await mountPanel();
    expect(JSON.parse(localStorage.getItem(SAVED_VIEWS_STORAGE_KEY)!)).toHaveLength(50);

    const trigger = document.querySelector<HTMLButtonElement>('button[aria-label="Saved view"]');
    trigger!.click();
    await tick();
    expect(
      [...document.querySelectorAll(".kit-typeahead__option")].some(
        (item) => item.textContent?.trim() === "View 51",
      ),
    ).toBe(false);
    document
      .querySelector<HTMLInputElement>('input[aria-label="Saved view"]')!
      .dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await settle();

    await nameView("New view");
    button("Save view").click();
    await settle();
    const saved = JSON.parse(localStorage.getItem(SAVED_VIEWS_STORAGE_KEY)!);
    expect(saved).toHaveLength(50);
    expect(saved[0].name).toBe("View 2");
    expect(saved.at(-1).name).toBe("New view");
  });
});
