import { test, expect } from "@playwright/test";

test.describe("Chromium dashboard evidence", () => {
  test.skip(
    ({ browserName }) => browserName !== "chromium",
    "the fixture evidence is recorded on Chromium",
  );

  test("root hydration precedes delayed analytics", async ({ page }) => {
    const requests: string[] = [];
    let analyticsResponded = false;
    page.on("request", (request) => {
      if (request.method() === "GET") requests.push(new URL(request.url()).pathname);
    });
    page.on("response", (response) => {
      if (response.url().includes("/api/v1/analytics/")) analyticsResponded = true;
    });
    await page.route("**/api/v1/analytics/**", async (route) => {
      await new Promise((resolve) => setTimeout(resolve, 6000));
      await route.continue();
    });
    await page.route(/\/api\/v1\/sessions\/test-session-[^/?]+(?:\?|$)/, async (route) => {
      await route.continue();
    });
    const fixture = await page.request.get("/api/v1/sessions?project=project-alpha");
    expect(fixture.ok()).toBe(true);
    const body = await fixture.json();
    expect(body.sessions).toHaveLength(3);
    expect(
      body.sessions.every((session: { project: string }) => session.project === "project-alpha"),
    ).toBe(true);
    await page.goto("/", { waitUntil: "commit" });
    await expect
      .poll(() => requests.some((path) => path.startsWith("/api/v1/sessions/test-session-")))
      .toBe(true);
    await expect(page.locator(".session-item").first()).toBeVisible();
    const respondedAtRow = analyticsResponded;
    await expect.poll(() => requests.some((path) => path.includes("/analytics/"))).toBe(true);
    const hydration = requests.findIndex((path) =>
      path.startsWith("/api/v1/sessions/test-session-"),
    );
    const analytics = requests.findIndex((path) => path.includes("/analytics/"));
    console.log(
      JSON.stringify({
        fixture: "project-alpha",
        fixtureRows: body.sessions.length,
        hydration,
        analytics,
        visible: true,
        respondedAtRow,
        requests,
      }),
    );
    expect.soft(hydration).toBeGreaterThanOrEqual(0);
    expect.soft(hydration).toBeLessThan(analytics);
    expect(respondedAtRow).toBe(false);
  });

  test("a held then failed tools read leaves siblings usable and recovers", async ({ page }) => {
    const held = Promise.withResolvers<void>();
    let toolsReads = 0;
    await page.route("**/api/v1/analytics/tools?**", async (route) => {
      toolsReads++;
      if (toolsReads > 1) return route.continue();
      await held.promise;
      await route.fulfill({ status: 503, json: { error: "tools unavailable" } });
    });
    await page.goto("/");
    const tools = page.locator(".tool-container");
    const sessionsValue = page.locator(".summary-cards .card-value").first();
    await expect(tools).toContainText("Loading tool usage...");
    await expect(sessionsValue).toHaveText(/^\d/);
    const duration = page
      .locator(".top-sessions-container")
      .getByRole("button", { name: "By Duration" });
    await duration.click();
    await expect(duration).toHaveClass(/active/);

    held.resolve();
    const retry = tools.getByRole("button", { name: "Retry" });
    await expect(retry).toBeVisible();
    await expect(sessionsValue).toHaveText(/^\d/);
    await retry.click();
    await expect(retry).toHaveCount(0);
    await expect(tools).not.toContainText("Loading tool usage...");
    expect(toolsReads).toBe(2);
  });

  test("a date change clears old values and ignores a late response", async ({ page }) => {
    const held = Promise.withResolvers<void>();
    let heldRange: { from: string; to: string } | undefined;
    let lateSettled = false;
    await page.route("**/api/v1/analytics/summary?**", async (route) => {
      const params = new URL(route.request().url()).searchParams;
      if (!heldRange) {
        const to = params.get("to")!;
        const from = new Date(`${to}T12:00:00Z`);
        from.setUTCDate(from.getUTCDate() - 29);
        heldRange = { from: from.toISOString().slice(0, 10), to };
      }
      if (params.get("from") !== heldRange.from || params.get("to") !== heldRange.to) {
        return route.continue();
      }
      await held.promise;
      await route.fulfill({ json: { total_sessions: 987654, agents: {} } }).catch(() => {});
      lateSettled = true;
    });
    await page.goto("/");
    const sessionsValue = page.locator(".summary-cards .card-value").first();
    await expect(sessionsValue).toHaveText(/^\d/);

    const pickPreset = async (label: string) => {
      const preset = page.getByRole("button", { name: label, exact: true });
      if (!(await preset.isVisible())) {
        await page.locator(".kit-date-range-picker__trigger").first().click();
      }
      await preset.click();
    };
    await pickPreset("30d");
    await expect(sessionsValue).toHaveText("-");
    await pickPreset("7d");
    await expect(sessionsValue).toHaveText(/^\d/);
    held.resolve();
    await expect.poll(() => lateSettled).toBe(true);
    await expect(sessionsValue).not.toHaveText("987,654");
  });

  for (const width of [1280, 768, 400]) {
    test(`root shell at ${width}`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 900 });
      await page.route("**/api/v1/analytics/**", () => {});
      await page.goto("/");
      await expect(page.locator(".analytics-page")).toBeVisible();
      await expect(page.locator(".analytics-toolbar")).toBeVisible();
      await page.screenshot({ path: testInfo.outputPath(`1592-2-after-${width}.png`) });
    });
  }
});
