import { BREAKPOINTS } from "@kenn-io/kit-ui";

export const SIDEBAR_WIDTH_KEY = "agentsview-sidebar-width";
export const SIDEBAR_WIDTH_DEFAULT = 260;
export const SIDEBAR_WIDTH_MIN = 220;
export const SIDEBAR_WIDTH_STORAGE_MAX = 520;
export const SIDEBAR_CONTENT_MIN = 480;
export const VITALS_WIDTH_KEY = "agentsview-vitals-width";
export const VITALS_WIDTH_DEFAULT = 320;
export const VITALS_WIDTH_MIN = 280;
export const VITALS_WIDTH_STORAGE_MAX = 560;
// Desktop starts one pixel past kit-ui's medium breakpoint so JS layout
// logic agrees with the (max-width: 760px) CSS rules and ui.isMobileViewport.
export const SIDEBAR_DESKTOP_BREAKPOINT = BREAKPOINTS.medium + 1;

function clampStoredPaneWidth(
  value: unknown,
  fallback: number,
  minWidth: number,
  maxWidth: number,
): number {
  const numericValue = typeof value === "string" && value.trim() !== "" ? Number(value) : value;

  if (typeof numericValue !== "number" || !Number.isFinite(numericValue)) {
    return fallback;
  }

  return Math.min(maxWidth, Math.max(minWidth, numericValue));
}

export function clampStoredSidebarWidth(value: unknown): number {
  return clampStoredPaneWidth(
    value,
    SIDEBAR_WIDTH_DEFAULT,
    SIDEBAR_WIDTH_MIN,
    SIDEBAR_WIDTH_STORAGE_MAX,
  );
}

export function clampStoredVitalsWidth(value: unknown): number {
  return clampStoredPaneWidth(
    value,
    VITALS_WIDTH_DEFAULT,
    VITALS_WIDTH_MIN,
    VITALS_WIDTH_STORAGE_MAX,
  );
}

export function isDesktopSidebarLayout(viewportWidth: number): boolean {
  return viewportWidth >= SIDEBAR_DESKTOP_BREAKPOINT;
}

function clampPaneWidthForLayout(
  desiredWidth: number,
  layoutWidth: number,
  minWidth: number,
  maxWidth: number,
): number {
  const layoutMaxWidth = Math.max(minWidth, Math.min(maxWidth, layoutWidth - SIDEBAR_CONTENT_MIN));

  return Math.min(layoutMaxWidth, Math.max(minWidth, desiredWidth));
}

export function clampSidebarWidthForLayout(desiredWidth: number, layoutWidth: number): number {
  return clampPaneWidthForLayout(
    desiredWidth,
    layoutWidth,
    SIDEBAR_WIDTH_MIN,
    SIDEBAR_WIDTH_STORAGE_MAX,
  );
}

export function clampVitalsWidthForLayout(desiredWidth: number, layoutWidth: number): number {
  return clampPaneWidthForLayout(
    desiredWidth,
    layoutWidth,
    VITALS_WIDTH_MIN,
    VITALS_WIDTH_STORAGE_MAX,
  );
}
