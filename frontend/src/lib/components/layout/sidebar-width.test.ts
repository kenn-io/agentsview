import { describe, expect, it } from "vite-plus/test";
import {
  SIDEBAR_CONTENT_MIN,
  SIDEBAR_DESKTOP_BREAKPOINT,
  SIDEBAR_WIDTH_DEFAULT,
  SIDEBAR_WIDTH_KEY,
  SIDEBAR_WIDTH_MIN,
  SIDEBAR_WIDTH_STORAGE_MAX,
  clampSidebarWidthForLayout,
  clampStoredSidebarWidth,
  isDesktopSidebarLayout,
} from "./sidebar-width.js";

describe("sidebar width helpers", () => {
  it("exports the expected sidebar width constants", () => {
    expect(SIDEBAR_WIDTH_KEY).toBe("agentsview-sidebar-width");
    expect(SIDEBAR_WIDTH_DEFAULT).toBe(260);
    expect(SIDEBAR_WIDTH_MIN).toBe(220);
    expect(SIDEBAR_WIDTH_STORAGE_MAX).toBe(520);
    expect(SIDEBAR_CONTENT_MIN).toBe(480);
    // One pixel past kit-ui BREAKPOINTS.medium (760), pairing with the
    // (max-width: 760px) CSS rules.
    expect(SIDEBAR_DESKTOP_BREAKPOINT).toBe(761);
  });

  it("falls back to the default for invalid stored values", () => {
    expect(clampStoredSidebarWidth(undefined)).toBe(SIDEBAR_WIDTH_DEFAULT);
    expect(clampStoredSidebarWidth(null)).toBe(SIDEBAR_WIDTH_DEFAULT);
    expect(clampStoredSidebarWidth("not-a-number")).toBe(
      SIDEBAR_WIDTH_DEFAULT,
    );
    expect(clampStoredSidebarWidth(Number.NaN)).toBe(SIDEBAR_WIDTH_DEFAULT);
    expect(clampStoredSidebarWidth(Number.POSITIVE_INFINITY)).toBe(
      SIDEBAR_WIDTH_DEFAULT,
    );
  });

  it("clamps stored values to the supported minimum and maximum", () => {
    expect(clampStoredSidebarWidth(100)).toBe(SIDEBAR_WIDTH_MIN);
    expect(clampStoredSidebarWidth(260)).toBe(260);
    expect(clampStoredSidebarWidth(999)).toBe(SIDEBAR_WIDTH_STORAGE_MAX);
  });

  it("accepts persisted numeric strings from localStorage", () => {
    expect(clampStoredSidebarWidth("260")).toBe(260);
    expect(clampStoredSidebarWidth("300")).toBe(300);
    expect(clampStoredSidebarWidth("999")).toBe(SIDEBAR_WIDTH_STORAGE_MAX);
  });

  it("treats widths past the medium breakpoint as desktop layout", () => {
    expect(isDesktopSidebarLayout(760)).toBe(false);
    expect(isDesktopSidebarLayout(761)).toBe(true);
  });

  it("never clamps the layout width below the sidebar minimum", () => {
    expect(clampSidebarWidthForLayout(180, 650)).toBe(SIDEBAR_WIDTH_MIN);
    expect(clampSidebarWidthForLayout(520, 650)).toBe(SIDEBAR_WIDTH_MIN);
  });

  it("limits sidebar width by the available layout width", () => {
    expect(clampSidebarWidthForLayout(520, 700)).toBe(220);
    expect(clampSidebarWidthForLayout(520, 900)).toBe(420);
  });
});
