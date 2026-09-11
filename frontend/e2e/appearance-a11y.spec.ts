import { test, expect, type Locator, type Page } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

function readZoom(page: Page): Promise<string> {
  return page.evaluate(() => document.documentElement.style.getPropertyValue("zoom"));
}

async function isolateServerZoom(page: Page): Promise<void> {
  let settingsBody: Record<string, unknown> | undefined;
  await page.route("**/api/v1/settings", async (route) => {
    if (route.request().method() === "GET") {
      const response = await route.fetch();
      settingsBody = await response.json();
      delete settingsBody.zoom_level;
      await route.fulfill({
        response,
        body: JSON.stringify(settingsBody),
      });
      return;
    }
    if (route.request().method() === "PUT") {
      if (!settingsBody) {
        const response = await page.request.get("/api/v1/settings");
        settingsBody = await response.json();
        delete settingsBody.zoom_level;
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(settingsBody),
      });
      return;
    }
    await route.continue();
  });
}

test.beforeEach(async ({ page }) => {
  await isolateServerZoom(page);
});

function luminance([r, g, b]: [number, number, number]): number {
  const [rr, gg, bb] = [r, g, b].map((channel) => {
    const value = channel / 255;
    return value <= 0.03928 ? value / 12.92 : Math.pow((value + 0.055) / 1.055, 2.4);
  });
  return 0.2126 * rr! + 0.7152 * gg! + 0.0722 * bb!;
}

function contrastRatio(
  foreground: [number, number, number],
  background: [number, number, number],
): number {
  const lighter = Math.max(luminance(foreground), luminance(background));
  const darker = Math.min(luminance(foreground), luminance(background));
  return (lighter + 0.05) / (darker + 0.05);
}

function parseRgb(value: string): [number, number, number] {
  const match = value.match(/rgba?\((\d+),\s*(\d+),\s*(\d+)/);
  if (!match) {
    throw new Error(`Expected rgb() color, got ${value}`);
  }
  return [Number(match[1]), Number(match[2]), Number(match[3])];
}

async function elementColors(locator: Locator): Promise<{
  background: string;
  foreground: string;
}> {
  return locator.evaluate((element) => {
    const styles = getComputedStyle(element);
    return {
      background: styles.backgroundColor,
      foreground: styles.color,
    };
  });
}

function expectReadableContrast(colors: { background: string; foreground: string }) {
  expect(
    contrastRatio(parseRgb(colors.foreground), parseRgb(colors.background)),
  ).toBeGreaterThanOrEqual(4.5);
}

test.describe("Appearance accessibility", () => {
  test("keeps Quality semantic wrappers as rendered layout boxes", async ({ page }) => {
    let failSignals = false;
    await page.route("**/api/v1/analytics/signals*", (route) => {
      if (failSignals) {
        return route.fulfill({ status: 500, body: "request failed" });
      }
      return route.fulfill({
        json: {
          scored_sessions: 2,
          unscored_sessions: 0,
          grade_distribution: { A: 1, B: 1 },
          avg_health_score: 85,
          outcome_distribution: { completed: 2 },
          outcome_confidence_distribution: { high: 2 },
          tool_health: {
            total_failure_signals: 1,
            total_retries: 0,
            total_edit_churn: 0,
            sessions_with_failures: 1,
            failure_rate: 50,
          },
          context_health: {
            avg_compaction_count: 0,
            sessions_with_compaction: 0,
            mid_task_compaction_count: 0,
            sessions_with_mid_task_compaction: 0,
            sessions_with_context_data: 2,
            avg_context_pressure: 0.2,
            high_pressure_sessions: 0,
          },
          quality_health: {
            computed_sessions: 2,
            totals: {
              short_prompt_count: 2,
              unstructured_start: 1,
              missing_success_criteria_count: 0,
              missing_verification_count: 0,
              duplicate_prompt_count: 0,
              no_code_context_count: 0,
              runaway_tool_loop_count: 0,
              frustration_marker_count: 0,
            },
            sessions_with_signal: {
              short_prompt_count: 2,
              unstructured_start: 1,
              missing_success_criteria_count: 0,
              missing_verification_count: 0,
              duplicate_prompt_count: 0,
              no_code_context_count: 0,
              runaway_tool_loop_count: 0,
              frustration_marker_count: 0,
            },
          },
          trend: [],
          by_agent: [],
          by_project: [],
          calibration: {},
        },
      });
    });
    await page.route("**/api/v1/analytics/signal-sessions*", (route) =>
      route.fulfill({
        json: {
          signal: "short_prompt_count",
          sessions: [
            {
              session_id: "example-session",
              project: "agentsview",
              agent: "codex",
              date: "2026-07-10",
              is_automated: false,
              outcome: "completed",
              health_score: 90,
              health_grade: "A",
              signal_total: 1,
              reason_code: "short_prompt",
              excerpt: "Example evidence",
              message_ordinal: 7,
              failure_signals: 0,
              retries: 0,
              edit_churn: 0,
            },
          ],
        },
      }),
    );

    await page.goto("/quality");
    await expect(page.getByRole("heading", { name: "Quality Patterns" })).toBeVisible();
    await page.locator(".driver-row").first().click();
    await expect(page.locator(".evidence-panel-live")).toBeVisible();

    const contentDisplays = await page.evaluate(() => {
      const display = (selector: string) =>
        getComputedStyle(document.querySelector(selector)!).display;
      return {
        recommendation: display(".recommendation-content"),
        summary: display(".summary-card-content"),
        pattern: display(".pattern-card-content"),
        evidence: display(".evidence-panel-live"),
      };
    });
    expect(contentDisplays).toEqual({
      recommendation: "grid",
      summary: "flex",
      pattern: "flex",
      evidence: "grid",
    });

    failSignals = true;
    await page.reload();
    const alert = page.getByRole("alert");
    await expect(alert).toBeVisible();
    await expect(alert).toHaveCSS("display", "grid");
  });

  test("legacy zoom scales the UI on web without horizontal overflow", async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem("agentsview-font-scale", "130");
    });
    const sp = new SessionsPage(page);
    await page.goto("/sessions");
    await sp.sessionItems.first().waitFor({ state: "attached" });
    if (await page.evaluate(() => matchMedia("(max-width: 760px)").matches)) {
      await page.getByRole("button", { name: "Toggle sidebar", exact: true }).click();
    }
    await expect(sp.sessionItems.first()).toBeVisible();

    expect(await readZoom(page)).toBe("1.3");

    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(2);

    await sp.selectFirstSession();
    await expect(sp.messageRows.first()).toBeVisible();
  });

  test("zoom at 90% renders and scrolls the transcript", async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem("agentsview-zoom-level", "90");
    });
    const sp = new SessionsPage(page);
    await page.goto("/sessions");
    await sp.sessionItems.first().waitFor({ state: "attached" });
    if (await page.evaluate(() => matchMedia("(max-width: 760px)").matches)) {
      await page.getByRole("button", { name: "Toggle sidebar", exact: true }).click();
    }
    await expect(sp.sessionItems.first()).toBeVisible();

    expect(await readZoom(page)).toBe("0.9");

    await sp.selectFirstSession();
    await expect(sp.messageRows.first()).toBeVisible();
  });

  test("high contrast applies the root class and overrides tokens", async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem("theme", "light");
      localStorage.setItem("agentsview-high-contrast", "true");
    });
    const sp = new SessionsPage(page);
    await sp.goto();

    const hasClass = await page.evaluate(() =>
      document.documentElement.classList.contains("high-contrast"),
    );
    expect(hasClass).toBe(true);

    // Browsers may normalize #000000 to #000; compare after stripping
    // leading # and zero-padding each channel to 6 hex digits.
    const textPrimary = await page.evaluate((): string => {
      const raw = getComputedStyle(document.documentElement)
        .getPropertyValue("--text-primary")
        .trim();
      // Expand shorthand #rgb → #rrggbb before comparing.
      if (/^#[0-9a-fA-F]{3}$/.test(raw)) {
        return "#" + raw[1]!.repeat(2) + raw[2]!.repeat(2) + raw[3]!.repeat(2);
      }
      return raw;
    });
    expect(textPrimary).toBe("#000000");
  });

  test("dark high contrast keeps accent-filled controls readable", async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem("theme", "dark");
      localStorage.setItem("agentsview-high-contrast", "true");
    });
    const sp = new SessionsPage(page);
    await sp.goto();

    // Measure a real kit-ui primary/solid Button (the Import modal's
    // confirm action) rather than a synthetic element, so a kit-ui Button
    // styling regression fails this test. Computed color/background still
    // reflect the tone tokens while the button is disabled.
    await page.locator(".import-btn").click();
    const solidButton = page.locator(".kit-button--solid.kit-button--info");
    await expect(solidButton).toBeVisible();
    expectReadableContrast(await elementColors(solidButton));
    await page.keyboard.press("Escape");
    await expect(solidButton).toBeHidden();

    await sp.selectFirstSession();
    const agentBadge = page.locator(".agent-badge").first();
    await expect(agentBadge).toBeVisible();
    expectReadableContrast(await elementColors(agentBadge));

    const nonBlueAgentBadgeColors = await page.evaluate(() => {
      const badge = document.createElement("span");
      badge.className = "agent-badge";
      badge.style.background = "var(--accent-green)";
      badge.style.color = "var(--accent-green-foreground)";
      badge.textContent = "Codex";
      document.body.append(badge);
      const styles = getComputedStyle(badge);
      const result = {
        background: styles.backgroundColor,
        foreground: styles.color,
      };
      badge.remove();
      return result;
    });
    expectReadableContrast(nonBlueAgentBadgeColors);

    const userRoleIcon = page.locator(".role-icon", { hasText: "U" }).first();
    await expect(userRoleIcon).toBeVisible();
    expectReadableContrast(await elementColors(userRoleIcon));

    const assistantRoleIcon = page.locator(".role-icon", { hasText: "A" }).first();
    await expect(assistantRoleIcon).toBeVisible();
    expectReadableContrast(await elementColors(assistantRoleIcon));
  });
});

async function openAppearance(page: Page, url: string) {
  await page.goto(url);
  await expect(page.getByRole("heading", { name: "Appearance", exact: true })).toBeVisible();
  await expect(page.getByTitle("Zoom", { exact: true })).toBeVisible();
}

async function expectZoom(page: Page, label: string, css: string, stored: string) {
  await expect(page.getByTitle("Zoom", { exact: true })).toHaveText(`Zoom ${label}`);
  await expect.poll(() => readZoom(page)).toBe(css);
  expect(await page.evaluate(() => localStorage.getItem("agentsview-zoom-level"))).toBe(stored);
  expect(await page.evaluate(() => localStorage.getItem("agentsview-font-scale"))).toBeNull();
}

for (const desktop of [false, true]) {
  test.describe(`Shared zoom in ${desktop ? "desktop-marked browser" : "browser"}`, () => {
    const url = desktop ? "/settings?desktop" : "/settings";

    for (const state of [
      { canonical: null, legacy: null, label: "100%", css: "1", stored: "100" },
      { canonical: null, legacy: "120", label: "120%", css: "1.2", stored: "120" },
      { canonical: "100", legacy: "130", label: "130%", css: "1.3", stored: "130" },
      { canonical: "150", legacy: "120", label: "150%", css: "1.5", stored: "150" },
      { canonical: "bad", legacy: "90", label: "90%", css: "0.9", stored: "90" },
      { canonical: "133", legacy: "150", label: "100%", css: "1", stored: "100" },
    ]) {
      test(`restores canonical ${state.canonical} and legacy ${state.legacy} through reload`, async ({ page }) => {
        await page.addInitScript(({ canonical, legacy }) => {
          if (sessionStorage.getItem("zoom-seeded")) return;
          if (canonical !== null) localStorage.setItem("agentsview-zoom-level", canonical);
          if (legacy !== null) localStorage.setItem("agentsview-font-scale", legacy);
          sessionStorage.setItem("zoom-seeded", "true");
        }, state);
        await openAppearance(page, url);
        await expectZoom(page, state.label, state.css, state.stored);
        await page.reload();
        await expectZoom(page, state.label, state.css, state.stored);
      });
    }

    for (const width of [1280, 768, 400]) {
      test(`selects shared zoom and reloads at width ${width}`, async ({ page }) => {
        await page.setViewportSize({ width, height: 900 });
        await openAppearance(page, url);
        await expect(page.getByText("Text size", { exact: true })).toHaveCount(0);
        await expect(page.getByText("Desktop zoom", { exact: true })).toHaveCount(0);
        await page.getByTitle("Zoom", { exact: true }).click();
        await expect(page.getByRole("option")).toHaveText([
          "67%", "75%", "80%", "90%", "100%", "110%", "120%", "125%", "130%", "150%", "175%", "200%",
        ]);
        await page.getByRole("option", { name: "120%", exact: true }).click();
        await expectZoom(page, "120%", "1.2", "120");
        await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(2);
        await page.reload();
        await expectZoom(page, "120%", "1.2", "120");
        await page.getByTitle("Zoom", { exact: true }).click();
        await expect(page.getByRole("option", { name: "120%", exact: true })).toHaveAttribute("aria-selected", "true");
        await page.getByRole("combobox", { name: "Zoom", exact: true }).fill("133");
        await expect(page.getByText("No matches", { exact: true })).toBeVisible();
        await page.keyboard.press("Enter");
        expect(await readZoom(page)).toBe("1.2");
        await page.keyboard.press("Escape");
      });
    }

    test("selects the native sink only with both desktop and bridge markers", async ({ page }) => {
      await page.addInitScript(() => {
        localStorage.setItem("agentsview-zoom-level", "150");
        localStorage.setItem("agentsview-font-scale", "120");
        const calls: number[] = [];
        Object.assign(window, {
          zoomCalls: calls,
          __TAURI__: { webviewWindow: { getCurrentWebviewWindow: () => ({ setZoom: async (factor: number) => { calls.push(factor); } }) } },
        });
      });
      await openAppearance(page, url);
      await expectZoom(page, "150%", desktop ? "1" : "1.5", "150");
      expect(await page.evaluate(() => (window as Window & { zoomCalls?: number[] }).zoomCalls)).toEqual(desktop ? [1.5] : []);
      await page.getByTitle("Zoom", { exact: true }).click();
      await page.getByRole("option", { name: "120%", exact: true }).click();
      await expectZoom(page, "120%", desktop ? "1" : "1.2", "120");
      expect(await page.evaluate(() => (window as Window & { zoomCalls?: number[] }).zoomCalls)).toEqual(desktop ? [1.5, 1.2] : []);
    });
  });
}

test("Shared zoom synchronizes Appearance, desktop status bar, and shortcuts", async ({ page }) => {
  await openAppearance(page, "/settings?desktop");
  await page.getByTitle("Zoom", { exact: true }).click();
  await page.getByRole("option", { name: "120%", exact: true }).click();
  await expect(page.locator(".zoom-level")).toHaveText("120%");
  await page.locator(".zoom-controls").getByTitle(/Zoom in/).click();
  await expectZoom(page, "125%", "1.25", "125");
  await page.keyboard.press("ControlOrMeta+-");
  await expectZoom(page, "120%", "1.2", "120");
  await page.keyboard.press("ControlOrMeta+=");
  await expectZoom(page, "125%", "1.25", "125");
  await page.keyboard.press("ControlOrMeta+0");
  await expectZoom(page, "100%", "1", "100");
  await expect(page.locator(".zoom-level")).toHaveText("100%");
  await page.reload();
  await expectZoom(page, "100%", "1", "100");
});

test("Shared zoom keeps desktop controls and shortcut interception out of the browser", async ({ page }) => {
  await openAppearance(page, "/settings");
  await page.getByTitle("Zoom", { exact: true }).click();
  await page.getByRole("option", { name: "120%", exact: true }).click();
  await expect(page.locator(".zoom-controls")).toHaveCount(0);
  const prevented = await page.evaluate(() => {
    const event = new KeyboardEvent("keydown", { key: "0", ctrlKey: true, metaKey: true, bubbles: true, cancelable: true });
    document.dispatchEvent(event);
    return event.defaultPrevented;
  });
  expect(prevented).toBe(false);
  await expectZoom(page, "120%", "1.2", "120");
});
