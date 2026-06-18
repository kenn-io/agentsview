import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/svelte";

const store = vi.hoisted(() => ({
  activity: {
    preset: "day" as "day" | "week" | "month" | "custom",
    setPreset: vi.fn(),
    load: vi.fn(),
  },
}));
vi.mock("../../stores/activity.svelte.js", () => store);

import RangeControl from "./RangeControl.svelte";

beforeEach(() => {
  store.activity.preset = "day";
  store.activity.setPreset.mockReset();
  store.activity.load.mockReset();
});

describe("RangeControl", () => {
  it("renders Day/Week/Month/Custom segments", () => {
    render(RangeControl);
    for (const label of ["Day", "Week", "Month", "Custom"]) {
      expect(screen.getByRole("button", { name: label })).toBeTruthy();
    }
  });
  it("selecting Week sets the preset and reloads", async () => {
    render(RangeControl);
    await fireEvent.click(screen.getByRole("button", { name: "Week" }));
    expect(store.activity.setPreset).toHaveBeenCalledWith("week");
    expect(store.activity.load).toHaveBeenCalled();
  });
  it("does not expose a 15m bucket control", () => {
    render(RangeControl);
    expect(screen.queryByText(/15m/i)).toBeNull();
  });
});
