import { expect, test, type Page, type TestInfo } from "@playwright/test";

const SIDEBAR_WIDTH_KEY = "agentsview-sidebar-width";
const REVIEW_LABEL = "Open Code Review";
const LONG_LABEL = "Open Code Review With a Very Long Custom Label";
const REGISTRY_LABEL = "Opencode";

interface SidebarFixtureRow {
  id: string;
  parent_session_id: null;
  relationship_type: null;
  project: string;
  machine: string;
  agent: string;
  agent_label?: string | null;
  entrypoint?: string | null;
  display_name: string;
  first_message: string;
  started_at: string;
  ended_at: string;
  created_at: string;
  message_count: number;
  user_message_count: number;
  is_automated: boolean;
  is_teammate: boolean;
}

interface SidebarFixtureResponse {
  sessions: SidebarFixtureRow[];
  next_cursor: null;
  total: number;
}

const now = "2026-09-08T12:00:00Z";
const fixtureRows: SidebarFixtureRow[] = [
  {
    id: "opencode-session",
    parent_session_id: null,
    relationship_type: null,
    project: "fixture-project",
    machine: "local",
    agent: "opencode",
    display_name: "OpenCode session",
    first_message: "OpenCode session",
    started_at: now,
    ended_at: now,
    created_at: now,
    message_count: 3,
    user_message_count: 2,
    is_automated: false,
    is_teammate: false,
  },
  {
    id: "open-code-review-session",
    parent_session_id: null,
    relationship_type: null,
    project: "fixture-project",
    machine: "ci-runner-east",
    agent: "opencode",
    agent_label: REVIEW_LABEL,
    entrypoint: "sdk-cli",
    display_name: "Review session with a long title for ellipsis checks",
    first_message: "Review session with a long title for ellipsis checks",
    started_at: now,
    ended_at: now,
    created_at: now,
    message_count: 4,
    user_message_count: 3,
    is_automated: false,
    is_teammate: false,
  },
  {
    id: "long-label-session",
    parent_session_id: null,
    relationship_type: null,
    project: "fixture-project",
    machine: "ci-runner-west",
    agent: "opencode",
    agent_label: LONG_LABEL,
    entrypoint: "sdk-python-experimental-runner",
    display_name: "Long label session with metadata",
    first_message: "Long label session with metadata",
    started_at: now,
    ended_at: now,
    created_at: now,
    message_count: 5,
    user_message_count: 4,
    is_automated: false,
    is_teammate: false,
  },
];

function sidebarResponse(): SidebarFixtureResponse {
  return {
    sessions: fixtureRows,
    next_cursor: null,
    total: fixtureRows.length,
  };
}

async function installSyntheticRoutes(page: Page) {
  await page.route("**/api/v1/**", async (route) => {
    const pathname = new URL(route.request().url()).pathname;

    if (pathname.endsWith("/sessions/sidebar-index")) {
      await route.fulfill({ json: sidebarResponse() });
      return;
    }
    if (pathname.endsWith("/sessions")) {
      await route.fulfill({
        json: { sessions: fixtureRows, next_cursor: null, total: fixtureRows.length },
      });
      return;
    }
    if (pathname.endsWith("/agents")) {
      await route.fulfill({
        json: { agents: [{ name: "opencode", session_count: fixtureRows.length }] },
      });
      return;
    }
    if (pathname.endsWith("/machines")) {
      await route.fulfill({ json: { machines: ["ci-runner-east", "ci-runner-west"] } });
      return;
    }
    if (pathname.endsWith("/projects")) {
      await route.fulfill({
        json: { projects: [{ name: "fixture-project", session_count: fixtureRows.length }] },
      });
      return;
    }
    if (pathname.endsWith("/stats")) {
      await route.fulfill({
        json: {
          session_count: fixtureRows.length,
          message_count: 12,
          user_message_count: 9,
          assistant_message_count: 3,
          tool_call_count: 0,
          project_count: 1,
          machine_count: 2,
          agent_count: 1,
          earliest_session: now,
        },
      });
      return;
    }
    if (pathname.endsWith("/sync/status")) {
      await route.fulfill({ json: { last_sync: null, stats: null } });
      return;
    }
    if (pathname.endsWith("/version")) {
      await route.fulfill({ json: { version: "test", commit: "test", read_only: false } });
      return;
    }
    if (pathname.endsWith("/update/check")) {
      await route.fulfill({ json: { update_available: false, current_version: "test" } });
      return;
    }
    if (pathname.endsWith("/settings")) {
      await route.fulfill({ json: {} });
      return;
    }

    await route.fulfill({ json: {} });
  });
}

async function openSessions(page: Page, viewportWidth: number, sidebarWidth: number) {
  await page.setViewportSize({ width: viewportWidth, height: 900 });
  await page.addInitScript(
    ({ key, value }) => localStorage.setItem(key, String(value)),
    { key: SIDEBAR_WIDTH_KEY, value: sidebarWidth },
  );
  await page.goto("/");
  await expect(page.locator(".session-item").first()).toBeVisible();
  await page.evaluate(() => document.fonts.ready);
}

async function measureRow(page: Page, sessionId: string) {
  return page.locator(`[data-session-id="${sessionId}"]`).evaluate((row) => {
    const rect = row.getBoundingClientRect();
    const meta = row.querySelector<HTMLElement>(".side-meta")!;
    const agent = row.querySelector<HTMLElement>(".agent-tag")!;
    const name = row.querySelector<HTMLElement>(".session-name")!;
    const controls = [...row.querySelectorAll<HTMLElement>("button, a")];
    return {
      row: {
        width: rect.width,
        height: rect.height,
        top: rect.top,
        bottom: rect.bottom,
        right: rect.right,
      },
      meta: { width: meta.getBoundingClientRect().width, right: meta.getBoundingClientRect().right },
      agent: {
        width: agent.getBoundingClientRect().width,
        right: agent.getBoundingClientRect().right,
        clientWidth: agent.clientWidth,
        scrollWidth: agent.scrollWidth,
      },
      entrypoint: (() => {
        const element = row.querySelector<HTMLElement>(".entrypoint-tag");
        if (!element) return null;
        const rect = element.getBoundingClientRect();
        return { top: rect.top, bottom: rect.bottom, height: rect.height };
      })(),
      name: { width: name.getBoundingClientRect().width, scrollWidth: name.scrollWidth },
      controls: controls.map((control) => {
        const controlRect = control.getBoundingClientRect();
        return { left: controlRect.left, right: controlRect.right, width: controlRect.width };
      }),
    };
  });
}

async function prefixFitsBeforeEllipsis(page: Page, sessionId: string, prefixLength: number) {
  return page.locator(`[data-session-id="${sessionId}"] .agent-tag`).evaluate(
    (agent, length) => {
      const textNode = agent.firstChild;
      if (!textNode || textNode.nodeType !== Node.TEXT_NODE) return false;
      const range = document.createRange();
      range.setStart(textNode, 0);
      range.setEnd(textNode, length);
      const prefix = range.getBoundingClientRect();
      const tag = agent.getBoundingClientRect();
      return prefix.width > 0 && prefix.right <= tag.right + 1;
    },
    prefixLength,
  );
}

async function collectRenderFindings(page: Page) {
  return page.evaluate(() => {
    const sidebar = document.querySelector<HTMLElement>("#session-sidebar");
    if (!sidebar) return ["missing #session-sidebar"];
    const sidebarRect = sidebar.getBoundingClientRect();
    const violations: string[] = [];
    for (const row of sidebar.querySelectorAll<HTMLElement>(".session-item")) {
      const rowRect = row.getBoundingClientRect();
      const name = row.querySelector<HTMLElement>(".session-name");
      const meta = row.querySelector<HTMLElement>(".side-meta");
      if (rowRect.right > sidebarRect.right + 1) violations.push("row escaped sidebar");
      if (name && name.getBoundingClientRect().right > rowRect.right + 1) {
        violations.push("title escaped row");
      }
      if (meta && meta.getBoundingClientRect().right > rowRect.right + 1) {
        violations.push("metadata escaped row");
      }
      for (const element of row.querySelectorAll<HTMLElement>(
        ".session-name, .side-meta, .side-meta > *, button, a",
      )) {
        const elementRect = element.getBoundingClientRect();
        if (
          elementRect.right > rowRect.right + 1 ||
          elementRect.left < rowRect.left - 1
        ) {
          violations.push("element escaped row horizontally");
        }
        if (
          elementRect.bottom > rowRect.bottom + 1 ||
          elementRect.top < rowRect.top - 1
        ) {
          violations.push("element escaped row vertically");
        }
      }
    }
    return violations;
  });
}

async function capture(page: Page, testInfo: TestInfo, name: string) {
  const screenshotPath = testInfo.outputPath(`${name}.png`);
  await page.locator("#session-sidebar").screenshot({ path: screenshotPath });
  return screenshotPath;
}

test.describe("sidebar agent names", () => {
  test.beforeEach(async ({ page }) => {
    await installSyntheticRoutes(page);
  });

  test("reporter distinction keeps the full label at 520px and a prefix at 220px", async ({ page }) => {
    await openSessions(page, 1280, 520);
    const wide = await measureRow(page, "open-code-review-session");
    expect(wide.row.width).toBeGreaterThanOrEqual(518);
    expect(wide.row.width).toBeLessThanOrEqual(520);
    expect(wide.meta.width).toBeLessThanOrEqual(wide.row.width * 0.4 + 1);
    expect(wide.agent.scrollWidth).toBeLessThanOrEqual(wide.agent.clientWidth + 1);
    console.log(`width=520px ${JSON.stringify(wide)}`);
    expect(page.locator('[data-session-id="open-code-review-session"] .agent-tag')).toHaveAttribute(
      "title",
      REVIEW_LABEL,
    );
    expect(page.locator('[data-session-id="opencode-session"] .agent-tag')).toHaveAttribute(
      "title",
      REGISTRY_LABEL,
    );

    await page.addInitScript(
      ({ key, value }) => localStorage.setItem(key, String(value)),
      { key: SIDEBAR_WIDTH_KEY, value: 220 },
    );
    await page.evaluate(
      ({ key }) => localStorage.setItem(key, "220"),
      { key: SIDEBAR_WIDTH_KEY },
    );
    await page.reload();
    await expect(page.locator(".session-item").first()).toBeVisible();
    await page.evaluate(() => document.fonts.ready);

    const narrow = await measureRow(page, "open-code-review-session");
    expect(narrow.row.width).toBeGreaterThanOrEqual(218);
    expect(narrow.row.width).toBeLessThanOrEqual(220);
    expect(narrow.meta.width).toBeLessThanOrEqual(narrow.row.width * 0.4 + 1);
    expect(await prefixFitsBeforeEllipsis(page, "open-code-review-session", "Open Code".length)).toBe(
      true,
    );
    const longLabel = await measureRow(page, "long-label-session");
    expect(longLabel.row.height).toBeCloseTo(42, 0);
    expect(longLabel.meta.width).toBeLessThanOrEqual(longLabel.row.width * 0.4 + 1);
    expect(longLabel.name.width).toBeGreaterThan(0);
    expect(longLabel.agent.scrollWidth).toBeGreaterThan(longLabel.agent.clientWidth);
    expect(longLabel.entrypoint?.bottom).toBeLessThanOrEqual(longLabel.row.bottom + 1);
    expect(longLabel.entrypoint?.top).toBeGreaterThanOrEqual(longLabel.row.top - 1);
    expect(page.locator('[data-session-id="long-label-session"] .agent-tag')).toHaveAttribute(
      "title",
      LONG_LABEL,
    );
    await page.locator('[data-session-id="long-label-session"] .agent-tag').hover();
    await expect(page.locator(".agent-tooltip")).toHaveText(LONG_LABEL);
    console.log(`width=220px ${JSON.stringify(narrow)}`);
    console.log(`width=220px long-label ${JSON.stringify(longLabel)}`);
  });

  test("metadata boundary preserves rows and controls across desktop and mobile widths", async ({ page }, testInfo) => {
    await openSessions(page, 1280, 260);
    const desktop = await measureRow(page, "open-code-review-session");
    expect(desktop.row.height).toBeCloseTo(42, 0);
    expect(desktop.name.width).toBeGreaterThan(0);
    expect(desktop.meta.width).toBeLessThanOrEqual(desktop.row.width * 0.4 + 1);
    expect(desktop.controls.every((control) => control.right <= desktop.row.right + 1)).toBe(true);
    const longDesktop = await measureRow(page, "long-label-session");
    expect(longDesktop.entrypoint?.bottom).toBeLessThanOrEqual(longDesktop.row.bottom + 1);
    const desktopLint = await collectRenderFindings(page);
    expect(desktopLint).toEqual([]);
    console.log(`render-lint width=1280px violations=${JSON.stringify(desktopLint)}`);
    console.log(`width=260px ${JSON.stringify(desktop)}`);
    await capture(page, testInfo, "sidebar-agent-names-1280");

    await page.setViewportSize({ width: 768, height: 900 });
    await page.addInitScript(
      ({ key, value }) => localStorage.setItem(key, String(value)),
      { key: SIDEBAR_WIDTH_KEY, value: 520 },
    );
    await page.evaluate(
      ({ key }) => localStorage.setItem(key, "520"),
      { key: SIDEBAR_WIDTH_KEY },
    );
    await page.reload();
    await expect(page.locator(".session-item").first()).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    const clamped = await page.locator("#session-sidebar").evaluate((sidebar) => {
      const rect = sidebar.getBoundingClientRect();
      return { viewport: "768px", width: rect.width };
    });
    expect(clamped.width).toBeGreaterThanOrEqual(220);
    expect(clamped.width).toBeLessThan(520);
    const clampedLint = await collectRenderFindings(page);
    expect(clampedLint).toEqual([]);
    console.log(`render-lint viewport=768px violations=${JSON.stringify(clampedLint)}`);
    console.log(`viewport=768px actualSidebarWidth=${clamped.width}px`);
    await capture(page, testInfo, "sidebar-agent-names-768");

    await page.setViewportSize({ width: 400, height: 900 });
    await page.reload();
    const drawer = page.locator("#session-sidebar");
    if (!(await drawer.evaluate((sidebar) => sidebar.classList.contains("open")))) {
      await page.locator("button.hamburger").click();
    }
    await expect(drawer).toHaveClass(/open/);
    await expect(page.locator(".session-item").first()).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    const drawerBox = await drawer.boundingBox();
    expect(drawerBox?.width).toBeCloseTo(280, 0);
    const mobile = await measureRow(page, "open-code-review-session");
    expect(mobile.row.height).toBeCloseTo(42, 0);
    expect(mobile.meta.width).toBeLessThanOrEqual(mobile.row.width * 0.4 + 1);
    const mobileLint = await collectRenderFindings(page);
    expect(mobileLint).toEqual([]);
    console.log(`render-lint viewport=400px drawer=280px violations=${JSON.stringify(mobileLint)}`);
    console.log(`viewport=400px drawer=280px ${JSON.stringify(mobile)}`);
    await capture(page, testInfo, "sidebar-agent-names-400");
  });
});
