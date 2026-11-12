import { expect, test, type Page } from "@playwright/test";
import { placeholderColor, renderedContrast, useTheme } from "./helpers/contrast";
import { SessionsPage } from "./pages/sessions-page";
import { KNOWN_AGENTS, agentColor, agentLabel } from "../src/lib/utils/agents";

const THEMES = [
  ["light", false],
  ["dark", false],
  ["light", true],
  ["dark", true],
] as const;

function themeName(mode: string, highContrast: boolean) {
  return highContrast ? `${mode} high contrast` : mode;
}

// Gives every listed and fetched session the same health grade.
async function withHealthGrade(page: Page, grade: string) {
  const patch = (session: Record<string, unknown>) => ({ ...session, health_grade: grade });
  await page.route(/\/api\/v1\/sessions(\/[^/?]+)?(\?.*)?$/, async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    const json = Array.isArray(body?.sessions)
      ? { ...body, sessions: body.sessions.map(patch) }
      : body && typeof body === "object" && "agent" in body
        ? patch(body)
        : body;
    await route.fulfill({ response, json });
  });
}

// One built-in agent per distinct agent color, so every agent color is
// checked even though the fixture only records a few agents.
const AGENT_PER_COLOR = [
  ...KNOWN_AGENTS.reduce((byColor, agent) => {
    const color = agentColor(agent.name);
    if (!byColor.has(color)) byColor.set(color, agent.name);
    return byColor;
  }, new Map<string, string>()).values(),
];

// The fixture lists about a dozen sessions and the palette shows ten, so
// the colors are checked a batch at a time, one page load per batch.
function agentBatches(size = 8) {
  const batches: string[][] = [];
  for (let i = 0; i < AGENT_PER_COLOR.length; i += size) {
    batches.push(AGENT_PER_COLOR.slice(i, i + size));
  }
  return batches;
}

// Rewrites listed and fetched sessions so they cycle through `agents`, in
// the order the sessions are first listed.
async function withAgents(page: Page, agents: string[]) {
  const assigned = new Map<string, string>();
  const agentFor = (id: string) => {
    if (!assigned.has(id)) assigned.set(id, agents[assigned.size % agents.length]!);
    return assigned.get(id)!;
  };
  const patch = (session: Record<string, unknown>) => ({
    ...session,
    agent: agentFor(String(session.id)),
    agent_label: null,
  });
  await page.route(/\/api\/v1\/sessions(\/[^/?]+)?(\?.*)?$/, async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    const json = Array.isArray(body?.sessions)
      ? { ...body, sessions: body.sessions.map(patch) }
      : body && typeof body === "object" && "agent" in body
        ? patch(body)
        : body;
    await route.fulfill({ response, json });
  });
}

// Row fills fade on hover and selection; measure the settled fill.
async function withoutTransitions(page: Page) {
  await page.addStyleTag({ content: "*, *::before { transition: none !important; }" });
}

for (const [mode, highContrast] of THEMES) {
  test.describe(`text contrast in ${themeName(mode, highContrast)}`, () => {
    test.beforeEach(async ({ page }) => {
      await useTheme(page, mode, highContrast);
      await page.setViewportSize({ width: 1400, height: 900 });
    });

    test("settings nav descriptions stay readable on resting and selected rows", async ({
      page,
    }) => {
      await page.goto("/settings");
      const summaries = page.locator(".kit-settings__nav-summary");
      await expect(summaries.first()).toBeVisible();
      const count = await summaries.count();
      expect(count).toBeGreaterThan(1);
      let sawSelected = false;
      for (let i = 0; i < count; i++) {
        const summary = summaries.nth(i);
        if (await summary.evaluate((el) => !!el.closest("[aria-current]"))) sawSelected = true;
        const contrast = await renderedContrast(summary);
        expect(
          contrast.ratio,
          `nav summary ${i} ${JSON.stringify(contrast)}`,
        ).toBeGreaterThanOrEqual(4.5);
      }
      expect(sawSelected).toBe(true);
    });

    test("settings placeholders stay readable", async ({ page, browserName }) => {
      // The placeholder colour is read over CDP, which only Chromium has.
      test.skip(browserName !== "chromium", "placeholder colour is read over CDP");
      await page.goto("/settings");
      await page.locator(".kit-settings__nav").getByText("Session providers").first().click();
      const inputs = page.locator(
        ".kit-settings__panel input.kit-text-input__control[placeholder]:enabled",
      );
      await expect(inputs.first()).toBeVisible();
      const input = inputs.first();
      const contrast = await renderedContrast(input, {
        foreground: await placeholderColor(page, input),
      });
      expect(contrast.ratio, JSON.stringify(contrast)).toBeGreaterThanOrEqual(4.5);
    });

    test("treemap tile values and details stay readable on solid tiles", async ({ page }) => {
      await page.goto("/usage");
      const tiles = page.locator(".treemap-container .tile:not(.patterned)");
      await expect(tiles.first()).toBeVisible();
      const texts = page.locator(
        ".treemap-container .tile:not(.patterned) :is(.tile-label, .tile-label-sm, .tile-value, .tile-meta)",
      );
      const count = await texts.count();
      expect(count).toBeGreaterThan(0);
      for (let i = 0; i < count; i++) {
        const text = texts.nth(i);
        const paint = await text.evaluate((el) => {
          const tile = el.closest(".tile")!;
          const style = getComputedStyle(el);
          return {
            fill: style.fill,
            opacity: Number(style.fillOpacity),
            tile: getComputedStyle(tile.querySelector("rect")!).fill,
          };
        });
        const contrast = await renderedContrast(text, {
          foreground: paint.fill,
          background: paint.tile,
          opacity: paint.opacity,
        });
        expect(contrast.ratio, `tile text ${i} ${JSON.stringify(contrast)}`).toBeGreaterThanOrEqual(
          4.5,
        );
      }
    });

    test("unclean status dots read as marks and the unclean count stays readable", async ({
      page,
    }) => {
      await page.goto("/sessions");
      const dots = page.locator(".kit-status-dot--unclean");
      await expect(dots.first()).toBeVisible();
      const dotCount = await dots.count();
      for (let i = 0; i < Math.min(dotCount, 6); i++) {
        const dot = dots.nth(i);
        const fill = await dot.evaluate((el) => getComputedStyle(el).backgroundColor);
        const contrast = await renderedContrast(dot.locator(".."), { foreground: fill });
        expect(
          contrast.ratio,
          `unclean dot ${i} ${JSON.stringify(contrast)}`,
        ).toBeGreaterThanOrEqual(3);
      }
      const pill = page.locator(".status-count-pill");
      await expect(pill).toBeVisible();
      const resting = await renderedContrast(pill);
      expect(resting.ratio, JSON.stringify(resting)).toBeGreaterThanOrEqual(4.5);
      await pill.hover();
      await expect
        .poll(async () => (await renderedContrast(pill)).background)
        .not.toBe(resting.background);
      const hovered = await renderedContrast(pill);
      expect(hovered.ratio, JSON.stringify(hovered)).toBeGreaterThanOrEqual(4.5);
    });

    test("find overview ticks read as marks on their track", async ({ page }) => {
      const sp = new SessionsPage(page);
      await sp.goto();
      await sp.selectFirstSession();
      const modifier = await page.evaluate(() =>
        /Mac/.test(navigator.platform) ? "Meta" : "Control",
      );
      await page.keyboard.press(`${modifier}+f`);
      await page.locator(".kit-find-bar__input").fill("e");
      const rail = page.locator(".find-overview-rail");
      await expect(rail.locator(".tick.current")).toHaveCount(1);
      const track = await rail.locator(".track").evaluate((el) => getComputedStyle(el).fill);
      const ticks = await rail
        .locator(".tick")
        .evaluateAll((els) => [...new Set(els.map((el) => getComputedStyle(el).fill))]);
      expect(ticks.length).toBeGreaterThanOrEqual(2);
      for (const tick of ticks) {
        const contrast = await renderedContrast(rail, { foreground: tick, background: track });
        expect(contrast.ratio, JSON.stringify(contrast)).toBeGreaterThanOrEqual(3);
      }
    });

    test("the folded Other series reads as a mark in high contrast", async ({ page }) => {
      // Normal modes keep it a quiet tail behind the named series.
      test.skip(!highContrast, "high contrast only");
      await page.goto("/usage");
      const chart = page.locator(".treemap-container").first();
      await expect(chart).toBeVisible();
      const other = await chart.evaluate((el) => {
        const probe = document.createElement("span");
        probe.style.color = "var(--usage-series-other)";
        el.appendChild(probe);
        const color = getComputedStyle(probe).color;
        probe.remove();
        return color;
      });
      const contrast = await renderedContrast(chart, { foreground: other });
      expect(contrast.ratio, JSON.stringify(contrast)).toBeGreaterThanOrEqual(3);
    });

    test("agent names stay readable in the sidebar and session header", async ({ page }) => {
      const header = page.locator(".session-breadcrumb .agent-badge:not(.entrypoint-badge)");
      const seen = new Set<string>();
      for (const agents of agentBatches()) {
        await page.unrouteAll({ behavior: "wait" });
        await withAgents(page, agents);
        const sp = new SessionsPage(page);
        await sp.goto();
        await withoutTransitions(page);
        const count = await sp.sessionItems.count();
        for (let i = 0; i < count; i++) {
          const row = sp.sessionItems.nth(i);
          const name = row.locator(".agent-tag");
          if ((await name.count()) === 0) continue;
          const label = (await name.textContent())!;
          if (seen.has(label)) continue;
          seen.add(label);
          await page.mouse.move(0, 0);
          const resting = await renderedContrast(name);
          await row.hover();
          const hovered = await renderedContrast(name);
          await row.click();
          await expect(row).toHaveClass(/\bactive\b/);
          const selected = await renderedContrast(name);
          for (const [state, contrast] of Object.entries({ resting, hovered, selected })) {
            expect(
              contrast.ratio,
              `sidebar ${label} ${state} ${JSON.stringify(contrast)}`,
            ).toBeGreaterThanOrEqual(4.5);
          }
          await expect(header).toHaveText(label);
          const inHeader = await renderedContrast(header);
          expect(
            inHeader.ratio,
            `header ${label} ${JSON.stringify(inHeader)}`,
          ).toBeGreaterThanOrEqual(4.5);
        }
      }
      expect([...seen].sort()).toEqual(AGENT_PER_COLOR.map((agent) => agentLabel(agent)).sort());
      await page.unrouteAll({ behavior: "ignoreErrors" });
    });

    test("agent names stay readable in the search palette", async ({ page }) => {
      const seen = new Set<string>();
      for (const agents of agentBatches()) {
        await page.unrouteAll({ behavior: "wait" });
        await withAgents(page, agents);
        const sp = new SessionsPage(page);
        await sp.goto();
        await withoutTransitions(page);
        await page.keyboard.press("ControlOrMeta+k");
        const items = page.locator(".palette-item");
        await expect(items.first()).toBeVisible();
        const count = await items.count();
        for (let i = 0; i < count; i++) {
          const name = items.nth(i).locator(".item-agent");
          const label = (await name.textContent())!;
          seen.add(label);
          if (i > 0) {
            const resting = await renderedContrast(name);
            expect(
              resting.ratio,
              `palette ${label} resting ${JSON.stringify(resting)}`,
            ).toBeGreaterThanOrEqual(4.5);
          }
          await items.nth(i).hover();
          await expect(items.nth(i)).toHaveClass(/\bselected\b/);
          const selected = await renderedContrast(name);
          expect(
            selected.ratio,
            `palette ${label} selected ${JSON.stringify(selected)}`,
          ).toBeGreaterThanOrEqual(4.5);
        }
        await page.keyboard.press("Escape");
      }
      expect([...seen].sort()).toEqual(AGENT_PER_COLOR.map((agent) => agentLabel(agent)).sort());
      await page.unrouteAll({ behavior: "ignoreErrors" });
    });

    for (const grade of ["A", "B", "C", "F"]) {
      test(`the ${grade} health grade stays readable at rest and on hover`, async ({ page }) => {
        await withHealthGrade(page, grade);
        const sp = new SessionsPage(page);
        await sp.goto();
        await sp.selectFirstSession();
        const badge = page.locator(".session-breadcrumb .grade-badge");
        await expect(badge).toHaveText(grade);
        // Usage adds controls before the grade and moves it horizontally.
        // Wait for that render so the pointer stays on the badge after hover.
        await expect(
          page.locator(".session-breadcrumb .usage-breakdown-trigger").first(),
        ).toBeVisible();
        const resting = await renderedContrast(badge);
        expect(resting.ratio, JSON.stringify(resting)).toBeGreaterThanOrEqual(4.5);
        await badge.hover();
        await expect
          .poll(async () => (await renderedContrast(badge)).background)
          .not.toBe(resting.background);
        const hovered = await renderedContrast(badge);
        expect(hovered.ratio, JSON.stringify(hovered)).toBeGreaterThanOrEqual(4.5);
      });
    }
  });
}
