// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, unmount } from "svelte";
// @ts-ignore
import RefreshControl from "./RefreshControl.svelte";
import source from "./RefreshControl.svelte?raw";

afterEach(() => {
  vi.useRealTimers();
  document.body.innerHTML = "";
});

describe("RefreshControl", () => {
  it("renders the icon button with the given label and tooltip default", () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: null,
        onRefresh: () => {},
        label: "Refresh activity",
      },
    });

    const button = document.querySelector<HTMLButtonElement>(
      'button.refresh-btn[aria-label="Refresh activity"]',
    );
    expect(button).not.toBeNull();
    // No explicit title falls back to the label.
    expect(button!.getAttribute("title")).toBe("Refresh activity");
    expect(button!.querySelector("svg")).not.toBeNull();

    const status = document.querySelector(".refresh-status span");
    expect(status?.textContent?.trim()).toBe("Not updated");

    unmount(component);
  });

  it("uses a distinct tooltip when title differs from the label", () => {
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: null,
        onRefresh: () => {},
        label: "Refresh usage data",
        title: "Refresh",
      },
    });

    const button = document.querySelector<HTMLButtonElement>(
      'button.refresh-btn[aria-label="Refresh usage data"]',
    );
    expect(button!.getAttribute("title")).toBe("Refresh");

    unmount(component);
  });

  it("formats the relative age from the last update", () => {
    vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
    vi.setSystemTime(new Date("2026-06-16T12:10:00Z"));
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: new Date("2026-06-16T12:08:00Z").getTime(),
        onRefresh: () => {},
        label: "Refresh",
      },
    });

    const status = document.querySelector(".refresh-status span");
    expect(status?.textContent?.trim()).toBe("Updated 2m ago");

    unmount(component);
  });

  it("refreshes on click", () => {
    const onRefresh = vi.fn();
    const component = mount(RefreshControl, {
      target: document.body,
      props: { lastUpdatedAt: null, onRefresh, label: "Refresh" },
    });

    document
      .querySelector<HTMLButtonElement>("button.refresh-btn")!
      .click();
    expect(onRefresh).toHaveBeenCalledOnce();

    unmount(component);
  });

  it("spins and disables the button while busy", () => {
    const onRefresh = vi.fn();
    const component = mount(RefreshControl, {
      target: document.body,
      props: { lastUpdatedAt: null, onRefresh, label: "Refresh", busy: true },
    });

    const button = document.querySelector<HTMLButtonElement>(
      "button.refresh-btn",
    );
    expect(button!.disabled).toBe(true);
    expect(button!.classList.contains("querying")).toBe(true);
    button!.click();
    expect(onRefresh).not.toHaveBeenCalled();

    unmount(component);
  });

  it("auto-refreshes on the interval without firing on mount", async () => {
    vi.useFakeTimers();
    const onRefresh = vi.fn();
    const component = mount(RefreshControl, {
      target: document.body,
      props: {
        lastUpdatedAt: null,
        onRefresh,
        label: "Refresh",
        intervalMs: 1000,
      },
    });

    expect(onRefresh).toHaveBeenCalledTimes(0);
    await vi.advanceTimersByTimeAsync(1000);
    expect(onRefresh).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1000);
    expect(onRefresh).toHaveBeenCalledTimes(2);

    unmount(component);
  });

  it("owns the bounded auto-refresh scheduler", () => {
    expect(source).toContain("createRefreshScheduler");
    expect(source).toContain("scheduler.scheduleNext()");
    expect(source).toContain("scheduler.refreshNow()");
    expect(source).toContain("DEFAULT_REFRESH_INTERVAL_MS");
    expect(source).not.toContain("setInterval");
  });

  it("ticks the age label on a one-minute timer", () => {
    expect(source).toContain("REFRESH_LABEL_INTERVAL_MS = 60 * 1000");
    expect(source).toContain("formatRefreshAge");
  });

  it("does not announce passive refresh-age ticks as live updates", () => {
    expect(source).not.toContain("aria-live");
  });

  it("keeps the timestamp beside the centered icon button", () => {
    const refreshControl =
      source.match(/\.refresh-control\s*{[^}]+}/)?.[0] ?? "";
    const refreshButton =
      source.match(/\.refresh-btn\s*{[^}]+}/)?.[0] ?? "";

    expect(refreshControl).toContain("display: inline-flex");
    expect(refreshControl).toContain("align-items: center");
    expect(refreshControl).toContain("gap: 8px");
    expect(refreshButton).toContain("width: 28px");
    expect(refreshButton).toContain("justify-content: center");
    expect(refreshButton).not.toContain("padding-right");
    expect(source).not.toContain(".refresh-btn::before");
  });
});
