// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";

const mocks = vi.hoisted(() => ({ getLimits: vi.fn() }));
vi.mock("../../api/generated/index", () => ({
  UsageService: { getApiV1UsageLimits: mocks.getLimits },
}));

import UsageLimitsPanel from "./UsageLimitsPanel.svelte";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  mocks.getLimits.mockReset();
  document.body.innerHTML = "";
});

async function settle() {
  await Promise.resolve();
  await tick();
}

describe("UsageLimitsPanel", () => {
  it("shows each reported window with a meter", async () => {
    mocks.getLimits.mockResolvedValue({
      limits: [
        {
          agent: "codex",
          account_key: "a",
          limit_id: "codex",
          plan_type: "pro",
          session_id: "codex:s1",
          observed_at: new Date().toISOString(),
          primary: { used_percent: 12, window_minutes: 10080 },
        },
      ],
    });
    component = mount(UsageLimitsPanel, { target: document.body });
    await settle();

    expect(document.querySelector(".chart-title")?.textContent).toBe("Plan limits");
    expect(document.querySelector(".group-title")?.textContent).toBe("Codex · pro");
    const meter = document.querySelector('[role="meter"]');
    expect(meter?.getAttribute("aria-valuenow")).toBe("12");
    expect(meter?.getAttribute("aria-label")).toBe("Weekly: 12% used");
  });

  it("stays hidden when nothing was recorded or the read fails", async () => {
    mocks.getLimits.mockResolvedValue({ limits: [] });
    component = mount(UsageLimitsPanel, { target: document.body });
    await settle();
    expect(document.querySelector(".limits-panel")).toBeNull();
    unmount(component);

    mocks.getLimits.mockRejectedValue(new Error("404"));
    component = mount(UsageLimitsPanel, { target: document.body });
    await settle();
    expect(document.querySelector(".limits-panel")).toBeNull();
  });
});
