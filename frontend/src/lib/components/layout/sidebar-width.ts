import { BREAKPOINTS } from "@kenn-io/kit-ui";

export const SIDEBAR_WIDTH_KEY = "agentsview-sidebar-width";
export const SIDEBAR_WIDTH_DEFAULT = 260;
export const SIDEBAR_WIDTH_MIN = 220;
export const SIDEBAR_WIDTH_STORAGE_MAX = 520;
export const SIDEBAR_CONTENT_MIN = 480;
// Desktop starts one pixel past kit-ui's medium breakpoint so JS layout
// logic agrees with the (max-width: 760px) CSS rules and ui.isMobileViewport.
export const SIDEBAR_DESKTOP_BREAKPOINT = BREAKPOINTS.medium + 1;

export function clampStoredSidebarWidth(value: unknown): number {
  const numericValue =
    typeof value === "string" && value.trim() !== ""
      ? Number(value)
      : value;

  if (typeof numericValue !== "number" || !Number.isFinite(numericValue)) {
    return SIDEBAR_WIDTH_DEFAULT;
  }

  return Math.min(
    SIDEBAR_WIDTH_STORAGE_MAX,
    Math.max(SIDEBAR_WIDTH_MIN, numericValue),
  );
}

export function isDesktopSidebarLayout(viewportWidth: number): boolean {
  return viewportWidth >= SIDEBAR_DESKTOP_BREAKPOINT;
}

export function clampSidebarWidthForLayout(
  desiredWidth: number,
  layoutWidth: number,
): number {
  const layoutMaxWidth = Math.max(
    SIDEBAR_WIDTH_MIN,
    Math.min(SIDEBAR_WIDTH_STORAGE_MAX, layoutWidth - SIDEBAR_CONTENT_MIN),
  );

  return Math.min(layoutMaxWidth, Math.max(SIDEBAR_WIDTH_MIN, desiredWidth));
}
