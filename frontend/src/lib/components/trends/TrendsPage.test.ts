// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { trends } from "../../stores/trends.svelte.js";
import type { TrendsTermsResponse } from "../../api/types.js";

const mocks = vi.hoisted(() => ({
  getApiV1TrendsTerms: vi.fn(),
}));

vi.mock("../../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
}));

vi.mock("../../api/generated/index", () => ({
  TrendsService: {
    getApiV1TrendsTerms: mocks.getApiV1TrendsTerms,
  },
}));

// @ts-ignore
import TrendsPage from "./TrendsPage.svelte";

function makeResponse(
  from = "2024-01-01",
  to = "2024-01-31",
): TrendsTermsResponse {
  return {
    granularity: "week",
    from,
    to,
    message_count: 0,
    buckets: [],
    series: [],
  };
}

async function flushPromises() {
  await Promise.resolve();
  await tick();
}

describe("TrendsPage", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    mocks.getApiV1TrendsTerms.mockImplementation((params) =>
      Promise.resolve(makeResponse(params.from, params.to)),
    );
    trends.from = "2024-01-01";
    trends.to = "2024-01-31";
    trends.granularity = "week";
    trends.termText = "seam";
    trends.response = null;
    trends.loading.terms = false;
    trends.errors.terms = null;
    window.history.replaceState(null, "", "/trends");
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    document.body.innerHTML = "";
    window.history.replaceState(null, "", "/");
    vi.unstubAllGlobals();
  });

  it("refreshes with the changed date value", async () => {
    component = mount(TrendsPage, { target: document.body });
    await flushPromises();

    const fromInput = document.querySelector<HTMLInputElement>(
      'input[type="date"]',
    );
    expect(fromInput).not.toBeNull();

    fromInput!.value = "2024-01-10";
    fromInput!.dispatchEvent(new Event("change", { bubbles: true }));
    await flushPromises();

    expect(mocks.getApiV1TrendsTerms).toHaveBeenLastCalledWith(
      expect.objectContaining({ from: "2024-01-10" }),
    );
    expect(window.location.search).toContain("from=2024-01-10");
  });

  it("shows the terms entry format hint", async () => {
    component = mount(TrendsPage, { target: document.body });
    await flushPromises();

    expect(document.body.textContent).toContain("one per line");
  });

  it("shows chart loading status while trends are computing", async () => {
    let resolveFetch:
      | ((response: TrendsTermsResponse) => void)
      | undefined;
    mocks.getApiV1TrendsTerms.mockReturnValueOnce(
      new Promise<TrendsTermsResponse>((resolve) => {
        resolveFetch = resolve;
      }),
    );

    component = mount(TrendsPage, { target: document.body });
    await tick();

    const status = document.querySelector<HTMLElement>(
      '[role="status"]',
    );
    expect(status).not.toBeNull();
    expect(status!.textContent).toContain("Computing trends");

    resolveFetch!(makeResponse());
    await flushPromises();

    expect(document.body.textContent).not.toContain(
      "Computing trends",
    );
  });

  it("surfaces explicit refresh errors after initial data loads", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    component = mount(TrendsPage, { target: document.body });
    await flushPromises();

    mocks.getApiV1TrendsTerms.mockRejectedValueOnce(
      new Error("at least one trend term is required"),
    );
    const textarea = document.querySelector<HTMLTextAreaElement>(
      "#trend-terms",
    );
    expect(textarea).not.toBeNull();
    textarea!.value = "";
    textarea!.dispatchEvent(new Event("input", { bubbles: true }));

    const refreshButton = Array.from(
      document.querySelectorAll<HTMLButtonElement>("button"),
    ).find((button) => button.textContent?.trim() === "Refresh");
    expect(refreshButton).not.toBeNull();
    refreshButton!.click();
    await flushPromises();

    expect(document.body.textContent).toContain(
      "at least one trend term is required",
    );
    warn.mockRestore();
  });

  it("toggles normalized term totals", async () => {
    mocks.getApiV1TrendsTerms.mockResolvedValueOnce({
      granularity: "week",
      from: "2024-01-01",
      to: "2024-01-31",
      message_count: 20,
      buckets: [{ date: "2024-01-01", message_count: 20 }],
      series: [
        {
          term: "seam",
          variants: ["seam"],
          total: 2,
          points: [{ date: "2024-01-01", count: 2 }],
        },
      ],
    });
    component = mount(TrendsPage, { target: document.body });
    await flushPromises();

    expect(document.body.textContent).toContain("Count");
    expect(document.body.textContent).toContain("2");

    const checkbox = document.querySelector<HTMLInputElement>(
      'input[type="checkbox"]',
    );
    expect(checkbox).not.toBeNull();
    checkbox!.click();
    await tick();

    expect(document.body.textContent).toContain("Per 1k messages");
    expect(document.body.textContent).toContain("100");
  });

  it("labels normalization by number of messages", async () => {
    component = mount(TrendsPage, { target: document.body });
    await flushPromises();

    expect(document.body.textContent).toContain(
      "Normalize by number of messages",
    );
  });

  it("shows a y-axis metric label", async () => {
    mocks.getApiV1TrendsTerms.mockResolvedValueOnce({
      granularity: "week",
      from: "2024-01-01",
      to: "2024-01-31",
      message_count: 20,
      buckets: [{ date: "2024-01-01", message_count: 20 }],
      series: [
        {
          term: "seam",
          variants: ["seam"],
          total: 2,
          points: [{ date: "2024-01-01", count: 2 }],
        },
      ],
    });
    component = mount(TrendsPage, { target: document.body });
    await flushPromises();

    expect(document.body.textContent).toContain("Occurrences");
  });

  it("uses the dedicated trends palette for seven terms", async () => {
    mocks.getApiV1TrendsTerms.mockResolvedValueOnce({
      granularity: "week",
      from: "2024-01-01",
      to: "2024-01-31",
      message_count: 70,
      buckets: [{ date: "2024-01-01", message_count: 70 }],
      series: Array.from({ length: 7 }, (_, i) => ({
        term: `term-${i + 1}`,
        variants: [`term-${i + 1}`],
        total: i + 1,
        points: [{ date: "2024-01-01", count: i + 1 }],
      })),
    });
    component = mount(TrendsPage, { target: document.body });
    await flushPromises();

    const swatches = Array.from(
      document.querySelectorAll<HTMLElement>(".swatch"),
    );
    const styles = swatches.map((el) => el.getAttribute("style") ?? "");
    expect(styles.slice(0, 7)).toEqual([
      "background: var(--trend-blue);",
      "background: var(--trend-gold);",
      "background: var(--trend-purple);",
      "background: var(--trend-green);",
      "background: var(--trend-magenta);",
      "background: var(--trend-slate);",
      "background: var(--trend-red);",
    ]);
  });
});
