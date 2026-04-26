import { test, expect, type Page } from "@playwright/test";

// Spec for the Session Vital Signs panel. Replaces the old
// ActivityMinimap spec — the minimap component is gone, and
// the right column now shows the four-section vital-signs
// panel rendered by SessionVitals.svelte.
//
// The fixture session `test-session-duration-showcase` is
// seeded by cmd/testfixture and exercised by scripts/e2e-server.sh.
// It contains: a solo Read turn, a parallel turn (two Reads + one
// Task with a sub-agent), and a slow Bash turn — exactly the
// shape needed to cover all four section interactions.

const SHOWCASE = "test-session-duration-showcase";

// The conversation scrolls inside `.message-list-scroll`
// (`SessionsPage.scroller`). The plan's sketch referenced
// `.conv-body`, which does not exist in the live DOM.
const SCROLLER = ".message-list-scroll";

async function gotoShowcase(page: Page) {
  // Vitals panel defaults to closed. Open it via localStorage
  // so the panel renders on first paint.
  await page.addInitScript(() => {
    localStorage.setItem("agentsview-session-vitals", "true");
  });
  await page.goto(`/sessions/${SHOWCASE}`);
  await expect(page.locator("aside.vitals")).toBeVisible({
    timeout: 5_000,
  });
  // Wait for timing data — the Calls section renders rows once
  // the API response lands. Without this, early assertions race
  // the fetch.
  await expect(
    page.locator(".calls .call").first(),
  ).toBeVisible({ timeout: 5_000 });
}

test.describe("Session Vital Signs", () => {
  test("renders all four sections", async ({ page }) => {
    await gotoShowcase(page);

    // Section headers live inside `.v-h > span` (text-only spans),
    // not semantic <h*> headings, so we match by text on .v-h
    // instead of the plan-sketch's `getByRole("heading")`.
    const headers = page
      .locator(".v-section .v-h > span:first-child")
      .filter({ hasText: /^(Session|Time spent|Timeline|Calls)$/ });
    await expect(headers).toHaveCount(4);

    await expect(
      page.locator(".v-section .v-h", { hasText: "Session" }),
    ).toBeVisible();
    await expect(
      page.locator(".v-section .v-h", { hasText: "Time spent" }),
    ).toBeVisible();
    await expect(
      page.locator(".v-section .v-h", { hasText: "Timeline" }),
    ).toBeVisible();
    await expect(
      page.locator(".v-section .v-h", { hasText: "Calls" }),
    ).toBeVisible();
  });

  test("slowest-call link scrolls the conversation", async ({
    page,
  }) => {
    await gotoShowcase(page);

    const scroller = page.locator(SCROLLER);
    // Reset to top so the click-induced scroll is observable.
    // (Default scrollTop is already 0 for this fixture, but be
    // explicit to avoid coupling to startup state.)
    await scroller.evaluate((el) => {
      el.scrollTop = 0;
    });

    await page.locator(".stat-grid .val-link").click();

    // ui.scrollToOrdinal sets pending state; the conversation
    // scrolls once MessageList processes the request.
    await expect
      .poll(() => scroller.evaluate((el) => el.scrollTop), {
        timeout: 3_000,
      })
      .toBeGreaterThan(0);
  });

  test("Time spent row click filters siblings", async ({
    page,
  }) => {
    await gotoShowcase(page);

    // Bash exists in the showcase fixture — pick it as the
    // active filter target. (Plan sketch suggested Bash.)
    const bashRow = page
      .locator(".agg-row")
      .filter({ has: page.locator(".agg-name", { hasText: "Bash" }) });
    const taskRow = page
      .locator(".agg-row")
      .filter({ has: page.locator(".agg-name", { hasText: "Task" }) });

    await expect(bashRow).toHaveCount(1);
    await expect(taskRow).toHaveCount(1);

    await bashRow.click();

    await expect(bashRow).toHaveClass(/\bactive\b/);
    await expect(taskRow).toHaveClass(/\bdimmed\b/);

    // Filter chip lives in the Time-spent section header.
    const chip = page.locator(".filter-chip");
    await expect(chip).toBeVisible();
    await expect(chip).toContainText("Bash");

    // Clear via the × inside the chip.
    await chip.locator(".x").click();

    await expect(page.locator(".filter-chip")).toHaveCount(0);
    await expect(taskRow).not.toHaveClass(/\bdimmed\b/);
    await expect(bashRow).not.toHaveClass(/\bactive\b/);
  });

  test("clicking a slow call scrolls the conversation", async ({
    page,
  }) => {
    await gotoShowcase(page);

    const scroller = page.locator(SCROLLER);
    await scroller.evaluate((el) => {
      el.scrollTop = 0;
    });

    // The slow threshold algorithm marks only the longest call
    // when fewer than 10 measurable calls exist; in the showcase
    // that's the Task call (120s) inside the parallel group.
    // We want a `.call` body click (not the chevron), so target
    // the row's name span explicitly.
    const slowCall = page.locator(".call.slow").first();
    await expect(slowCall).toBeVisible();
    await slowCall.locator(".cn").click();

    await expect
      .poll(() => scroller.evaluate((el) => el.scrollTop), {
        timeout: 3_000,
      })
      .toBeGreaterThan(0);
  });

  test("sub-agent expands and collapses inline via chevron", async ({
    page,
  }) => {
    await gotoShowcase(page);

    // The sub-agent lives on the Task call inside the parallel
    // group (`.cgroup`). CallRow renders `button.chev` only for
    // calls with a subagent_session_id.
    const taskRow = page
      .locator(".cgroup .call")
      .filter({
        has: page.locator(".cn", { hasText: "Task" }),
      });
    await expect(taskRow).toHaveCount(1);
    const chev = taskRow.locator("button.chev");
    await expect(chev).toBeVisible();
    await expect(chev).toHaveAttribute("aria-expanded", "false");

    await chev.click();

    const saExpand = page.locator(".sa-expand");
    await expect(saExpand).toBeVisible();
    // The expanded state is mirrored on the chevron button so
    // assistive tech sees the toggle work.
    await expect(chev).toHaveAttribute("aria-expanded", "true");

    // Collapse — re-clicking the chevron tears down the panel.
    await chev.click();
    await expect(saExpand).toHaveCount(0);
    await expect(chev).toHaveAttribute("aria-expanded", "false");
  });
});
