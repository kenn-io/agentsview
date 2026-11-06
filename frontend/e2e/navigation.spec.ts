import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

test.describe("Navigation", () => {
  let sp: SessionsPage;

  test.beforeEach(async ({ page }) => {
    sp = new SessionsPage(page);
    await sp.goto();
  });

  test.afterEach(async ({ page }) => {
    await page.unrouteAll({ behavior: "wait" });
  });

  test("keyboard ] navigates to next session", async () => {
    await sp.sessionItems.first().click();
    await expect(sp.sessionItems.first()).toHaveClass(/active/);

    await sp.pressNextSessionShortcut();
    await expect(sp.sessionItems.nth(1)).toHaveClass(/active/);
  });

  test("keyboard [ navigates to previous session", async () => {
    await sp.sessionItems.nth(1).click();
    await expect(sp.sessionItems.nth(1)).toHaveClass(/active/);

    await sp.pressPreviousSessionShortcut();
    await expect(sp.sessionItems.first()).toHaveClass(/active/);
  });

  test("analytics page shows when no session selected", async () => {
    await expect(sp.analyticsPage).toBeVisible();
    await expect(sp.analyticsToolbar).toBeVisible();
    await expect(sp.exportBtn).toContainText("Export CSV");
  });

  test("search result for an unlisted session keeps the breadcrumb project (#1190)", async ({
    page,
  }) => {
    // The target stays off the sidebar index page, as with a paginated
    // or filtered index in production. Search still finds it; opening
    // the result hydrates it by ID, and the index reload that entering
    // the sessions route triggers must not drop the hydrated row after
    // the detail fetch lands.
    const sessionId = "test-session-duration-subagent-1";
    await page.goto("/usage");
    await page.route("**/api/v1/sessions/sidebar-index**", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      body.sessions = body.sessions.filter((s: { id: string }) => s.id !== sessionId);
      await new Promise((resolve) => setTimeout(resolve, 500));
      await route.fulfill({ response, json: body });
    });

    await page.keyboard.press("ControlOrMeta+k");
    const input = page.locator(".palette-input");
    await expect(input).toBeVisible();
    await input.fill("synchronous DB read");

    const result = page
      .locator(".palette-item", {
        hasText: "synchronous DB read",
      })
      .first();
    await expect(result).toBeVisible();
    const indexReloaded = page.waitForResponse("**/api/v1/sessions/sidebar-index**");
    await result.click();

    await expect(page).toHaveURL(new RegExp(`/sessions/${sessionId}`));
    await indexReloaded;
    await expect(page.locator(".breadcrumb-current")).toHaveText(/project-duration/);
  });

  test("subagent cost rollup", async ({ page }, testInfo) => {
    await page.route("**/api/v1/sessions/*/usage**", async (route) => {
      const rollup = new URL(route.request().url()).searchParams.get("rollup");
      const complete = route.request().url().includes("test-session-mixed-content-7");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          session_id: "test-session-mixed-content-7",
          agent: "claude",
          project: "project",
          total_output_tokens: 0,
          peak_context_tokens: 0,
          has_token_data: false,
          cost: { microdollars: 1_000_000 },
          has_cost: true,
          models: [],
          unpriced_models: [],
          breakdown_count: 0,
          breakdown: [],
          server_running: true,
          ...(rollup === "true"
            ? {
                has_rollup_cost: complete,
                ...(complete ? { rollup_cost: { microdollars: 3_000_000 } } : {}),
                rollup_subagent_count: 1,
              }
            : {}),
        }),
      });
    });

    const selected = page.locator('.session-item[data-session-id="test-session-mixed-content-7"]');
    await selected.click();
    for (const width of [1280, 768, 400]) {
      await page.setViewportSize({ width, height: 800 });
      await expect(page.locator(".cost-badge")).toContainText("Total");
      await expect(page.locator(".cost-badge")).toContainText("$3.00");
      await expect(page.locator(".cost-badge")).toHaveAttribute(
        "title",
        "Total cost including 1 subagent",
      );
      await page.screenshot({
        path: testInfo.outputPath(`subagent-cost-rollup-${width}.png`),
      });
    }

    await page.setViewportSize({ width: 1280, height: 800 });
    const fallback = page
      .locator('.session-item:visible:not([data-session-id="test-session-mixed-content-7"])')
      .first();
    await fallback.click();
    await expect(page.locator(".cost-badge")).toContainText("$1.00");
    await expect(page.locator(".cost-badge")).not.toContainText("Total");
  });

  test("Shift+J and Shift+K navigate visible user prompts", async ({ page }, testInfo) => {
    const sessionId = "test-session-mixed-content-7";
    const session = page.locator(`.session-item[data-session-id="${sessionId}"]`);
    await expect(session).toHaveCount(1);
    await session.click();
    await expect(session).toHaveClass(/active/);
    await expect(sp.scroller).toHaveAttribute("data-messages-session-id", sessionId);
    await expect(sp.messageRows).toHaveCount(6);

    const users = sp.messageRows.filter({
      has: page.locator(".message.is-user"),
    });
    await users.first().click();
    await expect(users.first()).toHaveClass(/selected/);

    const assistants = sp.messageRows.filter({
      has: page.locator(".message:not(.is-user)"),
    });
    await page.keyboard.press("j");
    await expect(assistants.first()).toHaveClass(/selected/);
    await users.first().click();

    await page.keyboard.press("Shift+J");
    await expect(users.nth(1)).toHaveClass(/selected/);
    await page.keyboard.press("Shift+K");
    await expect(users.first()).toHaveClass(/selected/);

    await users.nth(1).click();
    await sp.toggleSortOrder();
    await expect(users.nth(1)).toHaveClass(/selected/);
    await page.keyboard.press("Shift+J");
    await expect(users.nth(2)).toHaveClass(/selected/);
    await users.nth(1).click();
    await page.keyboard.press("Shift+K");
    await expect(users.nth(0)).toHaveClass(/selected/);

    await page.keyboard.press("?");
    await expect(page.getByText("Next user prompt")).toBeVisible();
    await expect(page.getByText("Previous user prompt")).toBeVisible();

    const shortcutsModal = page.getByRole("dialog", {
      name: "Keyboard shortcuts",
    });
    for (const width of [1280, 768, 400]) {
      await page.setViewportSize({ width, height: 800 });
      const box = await shortcutsModal.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(width);
      await page.screenshot({
        path: testInfo.outputPath(`shortcuts-${width}.png`),
      });
    }
  });
});

test.describe("Top bar current tab", () => {
  // Wide enough that the ten tabs never collapse into the dropdown.
  test.use({ viewport: { width: 1600, height: 900 } });

  async function tabLooks(page: import("@playwright/test").Page) {
    const tabs = page.locator(".kit-top-bar__tabs .kit-top-bar__tab");
    await expect(tabs.first()).toBeVisible();
    // Park the pointer away from the bar so no tab is hovered.
    await page.mouse.move(800, 600);
    return tabs.evaluateAll((els) =>
      els.map((el) => {
        const s = getComputedStyle(el);
        return {
          label: el.textContent?.trim() ?? "",
          look: `${s.backgroundColor}|${s.boxShadow}|${s.color}`,
        };
      }),
    );
  }

  test("marks the route's tab on a tab page", async ({ page }) => {
    await page.goto("/sessions");
    const tabs = await tabLooks(page);
    const sessions = tabs.find((t) => t.label === "Sessions");
    const others = tabs.filter((t) => t.label !== "Sessions");
    expect(others.every((t) => t.look !== sessions?.look)).toBe(true);
  });

  test("shows no tab as current on Settings", async ({ page }) => {
    await page.goto("/settings");
    const tabs = await tabLooks(page);
    expect(new Set(tabs.map((t) => t.look)).size).toBe(1);
    await expect(page.locator('[aria-current="page"]')).toHaveCount(0);

    // Leaving Settings for the tab the bar would fall back to marks it again.
    await page.getByRole("navigation", { name: "Primary navigation" })
      .getByRole("button", { name: "Sessions", exact: true })
      .click();
    await expect(page.locator('[aria-current="page"]')).toHaveText("Sessions");
  });

  test("shows a neutral collapsed menu on Settings", async ({ page }) => {
    // Ten destinations do not fit at this width, so the tabs collapse.
    await page.setViewportSize({ width: 1000, height: 700 });
    await page.goto("/settings");
    const trigger = page.locator(".kit-top-bar__nav-select .kit-select-dropdown__trigger");
    await expect(trigger).toHaveText("—");
    await expect(trigger).toHaveAccessibleName("Primary navigation");
    await expect(page.locator('[aria-current="page"]')).toHaveCount(0);

    // The open menu presents no option as selected: no check, no selected
    // colour or weight, and no aria-selected.
    await page.mouse.move(0, 0);
    await trigger.click();
    const options = page.locator(".kit-top-bar__nav-select [role=option]");
    await expect(options.first()).toBeVisible();
    await expect(page.locator(".kit-top-bar__nav-select [role=option][aria-selected=true]"))
      .toHaveCount(0);
    await expect(page.locator(".kit-top-bar__nav-select .kit-select-dropdown__check svg:visible"))
      .toHaveCount(0);
    // Move the highlight off Sessions so it is measured at rest.
    await options.last().hover();
    await expect(options.first()).not.toHaveClass(/highlighted/);
    const looks = await options.evaluateAll((els) =>
      els
        .filter((el) => !el.matches(".highlighted, :disabled"))
        .map((el) => `${getComputedStyle(el).color} ${getComputedStyle(el).fontWeight}`),
    );
    expect(looks.length).toBeGreaterThan(1);
    expect(new Set(looks).size).toBe(1);
  });
});

test.describe("Project picker", () => {
  const projects = Array.from({ length: 60 }, (_, i) => ({
    name: `project-${String(i).padStart(2, "0")}`,
    session_count: 10,
  }));

  async function openPicker(page: import("@playwright/test").Page) {
    await page.route("**/api/v1/projects*", (route) => route.fulfill({ json: { projects } }));
    await page.goto("/sessions");
    await page.locator(".project-picker .kit-typeahead__trigger").click();
    await expect(page.locator(".kit-typeahead__option").first()).toBeVisible();
  }

  test("the open filter field sits above the collapsed navigation and search", async ({ page }) => {
    // Ten destinations do not fit at this width, so the tabs collapse.
    await page.setViewportSize({ width: 1400, height: 700 });
    await openPicker(page);
    await expect(page.locator(".kit-top-bar__nav-select")).toBeVisible();
    const hits = await page.locator(".kit-typeahead__input").evaluate((input) => {
      const r = input.getBoundingClientRect();
      return [0.1, 0.5, 0.9].map((f) => {
        const hit = document.elementFromPoint(r.left + r.width * f, r.top + r.height / 2);
        return !!hit && input.closest(".kit-typeahead")!.contains(hit);
      });
    });
    expect(hits).toEqual([true, true, true]);
  });

  for (const zoom of ["100", "150"]) {
    test(`the list shows only whole rows at ${zoom}% zoom`, async ({ page }) => {
      await page.addInitScript((value) => localStorage.setItem("agentsview-zoom-level", value), zoom);
      await page.setViewportSize({ width: 1400, height: 700 });
      await openPicker(page);
      const layout = await page.locator(".kit-typeahead__body").evaluate((body) => {
        const box = body.getBoundingClientRect();
        const rows = [...body.querySelectorAll(".kit-typeahead__option")]
          .map((row) => row.getBoundingClientRect())
          .filter((row) => row.bottom > box.top + 0.5 && row.top < box.bottom - 0.5);
        const panel = body.closest(".kit-typeahead__panel")!.getBoundingClientRect();
        return {
          rows: rows.length,
          lastCut: rows.at(-1)!.bottom - box.bottom,
          bodyInsidePanel: box.bottom <= panel.bottom + 0.5,
        };
      });
      expect(layout.rows).toBeGreaterThan(3);
      expect(layout.lastCut, JSON.stringify(layout)).toBeLessThanOrEqual(0.5);
      expect(layout.bodyInsidePanel).toBe(true);
    });
  }
});

for (const width of [320, 375]) {
  test(`search results keep titles readable and stay inside the palette at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 });
    await page.goto("/usage");
    await page.keyboard.press("ControlOrMeta+k");
    const input = page.locator(".palette-input");
    await expect(input).toBeVisible();
    await input.fill("synchronous DB read");
    const items = page.locator(".palette-item", { hasText: "synchronous DB read" });
    await expect(items.first()).toBeVisible();
    const rows = await items.evaluateAll((els) =>
      els.map((row) => {
        const box = row.getBoundingClientRect();
        const body = row.querySelector(".item-body")!.getBoundingClientRect();
        const parts = [...row.children].map((c) => c.getBoundingClientRect());
        return {
          bodyShare: body.width / box.width,
          inside: parts.every((p) => p.left >= box.left - 0.5 && p.right <= box.right + 0.5),
        };
      }),
    );
    for (const row of rows) {
      // The title and snippet keep most of the row.
      expect(row.bodyShare, JSON.stringify(row)).toBeGreaterThanOrEqual(0.6);
      expect(row.inside, JSON.stringify(row)).toBe(true);
    }
  });
}
