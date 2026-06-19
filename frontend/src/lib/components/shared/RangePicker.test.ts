import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/svelte";

import RangePicker from "./RangePicker.svelte";
import type { RangeSelection } from "./rangeSelection.js";

const relative30: RangeSelection = { mode: "relative", days: 30 };

function setup(selection: RangeSelection = relative30) {
  const onSelect = vi.fn();
  render(RangePicker, { selection, onSelect });
  return { onSelect };
}

async function openPanel() {
  // Before opening, the trigger is the only button present.
  await fireEvent.click(screen.getAllByRole("button")[0]!);
}

beforeEach(() => {
  vi.useRealTimers();
});

describe("RangePicker", () => {
  it("shows the current selection on the trigger", () => {
    setup();
    expect(screen.getByRole("button", { name: /Last 30 days/ })).toBeTruthy();
  });

  it("opens to the tab matching the selection mode", async () => {
    setup();
    await openPanel();
    for (const t of ["Relative", "Calendar", "Custom"]) {
      expect(screen.getByRole("tab", { name: t })).toBeTruthy();
    }
    expect(
      screen.getByRole("tab", { name: "Relative" }).getAttribute("aria-selected"),
    ).toBe("true");
  });

  it("emits a relative selection when a preset is clicked", async () => {
    const { onSelect } = setup();
    await openPanel();
    await fireEvent.click(screen.getByRole("button", { name: "7d" }));
    expect(onSelect).toHaveBeenCalledWith({ mode: "relative", days: 7 });
  });

  it("emits a calendar selection and steps the period", async () => {
    vi.setSystemTime(new Date("2026-06-17T12:00:00Z"));
    const { onSelect } = setup();
    await openPanel();
    await fireEvent.click(screen.getByRole("tab", { name: "Calendar" }));
    await fireEvent.click(screen.getByRole("button", { name: "Week" }));
    expect(onSelect).toHaveBeenLastCalledWith({
      mode: "calendar",
      unit: "week",
      anchor: "2026-06-17",
    });
    await fireEvent.click(screen.getByRole("button", { name: "Next period" }));
    expect(onSelect).toHaveBeenLastCalledWith({
      mode: "calendar",
      unit: "week",
      anchor: "2026-06-24",
    });
  });

  it("emits a custom selection when both dates are edited", async () => {
    vi.setSystemTime(new Date("2026-06-17T12:00:00Z"));
    const { onSelect } = setup();
    await openPanel();
    await fireEvent.click(screen.getByRole("tab", { name: "Custom" }));
    const inputs = screen.getAllByDisplayValue(/2026-/);
    const from = inputs[0] as HTMLInputElement;
    await fireEvent.input(from, { target: { value: "2026-01-01" } });
    await fireEvent.change(from, { target: { value: "2026-01-01" } });
    expect(onSelect).toHaveBeenCalledWith(
      expect.objectContaining({ mode: "custom", from: "2026-01-01" }),
    );
  });

  it("labels a calendar week selection on the trigger", () => {
    setup({ mode: "calendar", unit: "week", anchor: "2026-06-17" });
    expect(screen.getByRole("button", { name: /Week of Jun 15/ })).toBeTruthy();
  });

  it("syncs the Custom tab to a preset chosen while open", async () => {
    vi.setSystemTime(new Date("2026-06-17T12:00:00Z"));
    setup({ mode: "custom", from: "2020-01-01", to: "2020-01-31" });
    await openPanel();
    await fireEvent.click(screen.getByRole("tab", { name: "Relative" }));
    await fireEvent.click(screen.getByRole("button", { name: "7d" }));
    await fireEvent.click(screen.getByRole("tab", { name: "Custom" }));
    // 7 days before 2026-06-17 is 2026-06-10; the stale 2020 seed is gone.
    const from = screen.getAllByDisplayValue(/2026-/)[0] as HTMLInputElement;
    expect(from.value).toBe("2026-06-10");
  });

  it("normalizes a reversed custom range before emitting", async () => {
    vi.setSystemTime(new Date("2026-06-17T12:00:00Z"));
    const { onSelect } = setup({
      mode: "custom",
      from: "2026-06-10",
      to: "2026-06-20",
    });
    await openPanel();
    const from = screen.getAllByDisplayValue(/2026-/)[0] as HTMLInputElement;
    await fireEvent.input(from, { target: { value: "2026-06-25" } });
    await fireEvent.change(from, { target: { value: "2026-06-25" } });
    expect(onSelect).toHaveBeenLastCalledWith({
      mode: "custom",
      from: "2026-06-20",
      to: "2026-06-25",
    });
  });
});
