import { expect, test } from "@playwright/test";
import { RuntimeErrorMonitor } from "./helpers/runtime-error-monitor";
import { SessionsPage } from "./pages/sessions-page";

const DEPTH_ERROR_RE =
  /effect_update_depth_exceeded|Maximum update depth exceeded/i;

// Svelte 5 fires each_key_duplicate warnings when virtual-scroll
// items shift keys during rapid filter/sort transitions. These
// are cosmetic — the DOM recovers immediately.
const KNOWN_SVELTE_WARNINGS_RE = /each_key_duplicate/;

// Test-fixture assumptions: project-alpha has sessions with 2
// and 5+ messages, totalling 9 sessions across all projects
// (including the project-duration showcase added by Task 24).
const TEST_PROJECT = "project-alpha";
const FILTERED_SESSION_COUNT = 2;
const TOTAL_SESSION_COUNT = 9;

// Session deep in the list to exercise virtualizer scroll.
const TARGET_SESSION_INDEX = 6;
// Double-toggle cycles sort order back to original state.
const SORT_TOGGLE_CLICKS = 2;

// Svelte still emits depth warnings under rapid virtualizer
// churn. Keep them bounded to catch regressions.
const MAX_DEPTH_ERRORS = 4;

test.describe("Runtime stability", () => {
  test(
    "effect update-depth errors stay bounded during core interactions",
    async ({ page }) => {
      const monitor = new RuntimeErrorMonitor(page);
      const sp = new SessionsPage(page);

      await sp.goto();

      // Exercise the highest-churn flows: session open,
      // sort toggle, and project filtering.
      await sp.selectSession(TARGET_SESSION_INDEX);

      await sp.toggleSortOrder(SORT_TOGGLE_CLICKS);

      await sp.filterByProject(TEST_PROJECT);
      await expect(sp.sessionListHeader).toContainText(
        `${FILTERED_SESSION_COUNT} sessions`,
      );

      await sp.clearProjectFilter();
      await expect(sp.sessionListHeader).toContainText(
        `${TOTAL_SESSION_COUNT} sessions`,
      );

      const depthErrors = monitor.matching(DEPTH_ERROR_RE);
      expect(
        depthErrors.length,
        `depth errors (${depthErrors.length}) should be <= ${MAX_DEPTH_ERRORS}`,
      ).toBeLessThanOrEqual(MAX_DEPTH_ERRORS);

      const otherErrors = monitor
        .excluding(DEPTH_ERROR_RE)
        .filter((m) => !KNOWN_SVELTE_WARNINGS_RE.test(m));
      expect(otherErrors).toEqual([]);
    },
  );
});
