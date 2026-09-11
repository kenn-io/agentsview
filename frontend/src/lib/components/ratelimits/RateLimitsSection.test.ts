// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { ServiceRateLimitWindow } from "../../api/generated/index";

const rateLimitsServiceMocks = vi.hoisted(() => ({
  getApiV1RateLimitsCurrent: vi.fn(),
  getApiV1RateLimitsHistory: vi.fn(),
}));

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: (o?: { signal?: AbortSignal }) => Promise<unknown>) => request()),
  };
});
vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/generated/index")>();
  return { ...orig, RateLimitsService: rateLimitsServiceMocks };
});

const { rateLimits } = await import("../../stores/ratelimits.svelte.js");
const { default: RateLimitsSection } = await import("./RateLimitsSection.svelte");

// snapshot builds a fixture with a limitName distinct from limitId, so a
// card that fell back to rendering limitId would be caught.
function snapshot(overrides: Partial<ServiceRateLimitWindow> = {}): ServiceRateLimitWindow {
  return {
    vendor: "codex", machine: "laptop", limitId: "codex", limitName: "GPT-5.3-Codex-Spark",
    planType: "pro", windowKind: "primary", usedPercent: 95, windowMinutes: 10080,
    creditsHas: true, creditsUnlimited: false, observedAt: "2026-09-09T10:00:00Z", ...overrides,
  };
}

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) {
    void unmount(component);
    component = undefined;
  }
  vi.clearAllMocks();
  rateLimits.current = [];
  rateLimits.history = {};
  document.body.innerHTML = "";
});

async function mountSection(from = "2026-09-01", to = "2026-09-09") {
  component = mount(RateLimitsSection, { target: document.body, props: { from, to } });
  await tick();
  await tick();
  await tick();
}

// A card's header must show the vendor-reported limit_name, not fall
// back to limit_id, and Codex snapshots (no account identity) must group
// into one account per machine within the vendor -- two machines render
// as two separate account groups.
describe("RateLimitsSection", () => {
  it("shows each card's limit_name and groups snapshots by vendor then account", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([
      snapshot({ machine: "laptop" }),
      snapshot({ machine: "desktop" }),
    ]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);
    await mountSection();

    const header = document.querySelector(".limit-id")?.textContent;
    expect(header).toContain("GPT-5.3-Codex-Spark");
    expect(header).not.toContain("[codex]");
    expect(document.querySelectorAll(".account-group")).toHaveLength(2);
    const accountTitles = [...document.querySelectorAll(".account-title")].map((el) => el.textContent);
    expect(accountTitles).toEqual(expect.arrayContaining(["laptop", "desktop"]));
  });

  // No windowMinutes: fall back to a window-kind label, not "— limit".
  it("falls back to a window-kind label when windowMinutes is unknown", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([
      snapshot({ windowMinutes: undefined, windowKind: "primary" }),
    ]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);
    await mountSection();
    expect(document.querySelector(".limit-id")?.textContent).toContain("Session limit");
  });
});
