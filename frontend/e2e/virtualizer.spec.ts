import { test, expect, type Locator, type Page } from "@playwright/test";

// Session list is ordered by ended_at DESC.
// We select sessions by stable properties (project + message count)
// rather than unstable indices.
const ESTIMATE_PX = 120;

const LOC = {
  sessionItem: ".session-item",
  sessionProject: ".session-project",
  sessionCount: ".session-count",
  listScroll: ".message-list-scroll",
  row: ".virtual-row",
} as const;

const SESSIONS = {
  ALPHA_5: { project: "project-alpha", count: 3, displayRows: 5 },
  ALPHA_2: { project: "project-alpha", count: 2, displayRows: 2 },
  BETA_6: { project: "project-beta", count: 3, displayRows: 5 },
  DELTA_5500: { project: "project-delta", count: 2750 },
};

function getSessionItem(page: Page, project: string, count: number) {
  return page
    .locator(LOC.sessionItem)
    .filter({
      has: page.locator(`${LOC.sessionProject}:text-is("${project}")`),
    })
    .filter({
      has: page.locator(`${LOC.sessionCount}:text-is("${count}")`),
    });
}

async function selectSession(page: Page, project: string, count: number): Promise<string> {
  const item = getSessionItem(page, project, count);
  const sessionId = await item.getAttribute("data-session-id");
  expect(sessionId).toBeTruthy();
  await item.click();
  await expect(item).toHaveClass(/active/);
  return sessionId!;
}

async function expectSessionLoaded(page: Page, sessionId: string, expectedRows?: number) {
  const messageList = page.locator(LOC.listScroll);
  await expect(messageList).toHaveAttribute("data-session-id", sessionId);
  await expect(messageList).toHaveAttribute("data-messages-session-id", sessionId);
  await expect(messageList).toHaveAttribute("data-loaded", "true");

  if (expectedRows !== undefined) {
    await expect(page.locator(LOC.row)).toHaveCount(expectedRows);
  } else {
    await expect(page.locator(LOC.row).first()).toBeVisible();
  }
}

/**
 * Wait until the virtualizer has measured items and the container
 * height no longer equals the initial estimate.
 */
async function waitForLayoutSettle(page: Page, itemCount: number) {
  const container = page.locator(`${LOC.listScroll} > div`).first();
  await expect
    .poll(async () => {
      return container.evaluate((el) => el.getBoundingClientRect().height);
    })
    .not.toBe(itemCount * ESTIMATE_PX);
  return container;
}

/**
 * Verify there are no vertical gaps (> 1px) between consecutive
 * virtual rows by checking translateY positions against heights.
 */
async function verifyNoVerticalGaps(rows: Locator) {
  expect(await maxVerticalGap(rows)).toBeLessThanOrEqual(1);
}

async function maxVerticalGap(rows: Locator) {
  const positions = await rows.evaluateAll((els) =>
    els.map((el) => {
      const style = el.style.transform;
      const match = style.match(/translateY\((\d+(?:\.\d+)?)px\)/);
      return {
        translateY: match ? parseFloat(match[1]!) : 0,
        height: el.offsetHeight,
      };
    }),
  );

  let maxGap = 0;
  for (let i = 0; i < positions.length - 1; i++) {
    const current = positions[i]!;
    const next = positions[i + 1]!;
    const expectedNextStart = current.translateY + current.height;
    maxGap = Math.max(maxGap, Math.abs(expectedNextStart - next.translateY));
  }
  return maxGap;
}

test.beforeEach(async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(LOC.sessionItem).first()).toBeVisible({ timeout: 5_000 });
});

test.describe("Virtualizer measurement", () => {
  test("items are measured to actual DOM height", async ({ page }) => {
    const { project, count, displayRows } = SESSIONS.ALPHA_5;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid, displayRows);

    const rows = page.locator(LOC.row);
    const container = await waitForLayoutSettle(page, displayRows);

    const totalHeight = await container.evaluate((el) => el.getBoundingClientRect().height);
    expect(totalHeight).toBeGreaterThan(0);

    // Rows should be measured to actual DOM height. Individual rows
    // may coincidentally match the estimate, but not all of them.
    const rowCount = await rows.count();
    let nonEstimateCount = 0;
    for (let i = 0; i < rowCount; i++) {
      const h = await rows.nth(i).evaluate((el) => el.getBoundingClientRect().height);
      expect(h).toBeGreaterThan(0);
      if (h !== ESTIMATE_PX) nonEstimateCount++;
    }
    expect(nonEstimateCount).toBeGreaterThan(0);
  });

  test("no gaps between consecutive virtual rows", async ({ page }) => {
    const { project, count, displayRows } = SESSIONS.ALPHA_5;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid, displayRows);

    const rows = page.locator(LOC.row);
    await waitForLayoutSettle(page, displayRows);
    await verifyNoVerticalGaps(rows);
  });

  test("total container height matches sum of items", async ({ page }) => {
    const { project, count, displayRows } = SESSIONS.ALPHA_5;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid, displayRows);

    const rows = page.locator(LOC.row);
    const container = await waitForLayoutSettle(page, displayRows);

    const sumOfHeights = await rows.evaluateAll((els) =>
      els.reduce((sum, el) => sum + el.offsetHeight, 0),
    );

    const totalHeight = await container.evaluate((el) => el.getBoundingClientRect().height);

    // With overscan=5 and only 5 items, all should be in DOM
    expect(Math.abs(totalHeight - sumOfHeights)).toBeLessThanOrEqual(5);
  });

  test("measurements update after session switch", async ({ page }) => {
    const sessionA = SESSIONS.ALPHA_5;
    const sidA = await selectSession(page, sessionA.project, sessionA.count);
    await expectSessionLoaded(page, sidA, sessionA.displayRows);

    const container = await waitForLayoutSettle(page, sessionA.displayRows);

    const heightA = await container.evaluate((el) => el.getBoundingClientRect().height);
    expect(heightA).toBeGreaterThan(0);

    const sessionB = SESSIONS.ALPHA_2;
    const sidB = await selectSession(page, sessionB.project, sessionB.count);
    await expectSessionLoaded(page, sidB, sessionB.displayRows);

    await waitForLayoutSettle(page, sessionB.displayRows);

    const heightB = await container.evaluate((el) => el.getBoundingClientRect().height);
    expect(heightB).toBeGreaterThan(0);

    // Heights should differ (different message counts)
    expect(heightA).not.toBe(heightB);
  });

  test("message rows stay adjacent across deep sort toggles", async ({ page }) => {
    const { project, count } = SESSIONS.DELTA_5500;
    const sid = await selectSession(page, project, count);
    await expectSessionLoaded(page, sid);

    const rows = page.locator(LOC.row);
    const scroller = page.locator(LOC.listScroll);
    const sortButton = page.getByLabel("Toggle sort order");
    await page.addStyleTag({
      content: `
        .message.is-user { height: 320px; overflow: hidden; }
        .message:not(.is-user) { height: 80px; overflow: hidden; }
      `,
    });
    await scroller.evaluate((element) => {
      element.scrollTop = element.scrollHeight * 0.65;
    });
    await expect.poll(() => scroller.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
    await expect.poll(() => maxVerticalGap(rows)).toBeLessThanOrEqual(1);

    await sortButton.click();
    await expect(rows.first()).toBeVisible({ timeout: 5_000 });
    await expect.poll(() => maxVerticalGap(rows)).toBeLessThanOrEqual(1);

    await sortButton.click();
    await expect(rows.first()).toBeVisible({ timeout: 5_000 });
    await expect.poll(() => maxVerticalGap(rows)).toBeLessThanOrEqual(1);
  });

  test("session switch without explicit row count wait", async ({ page }) => {
    const sessionA = SESSIONS.ALPHA_2;
    const sidA = await selectSession(page, sessionA.project, sessionA.count);
    await expectSessionLoaded(page, sidA, sessionA.displayRows);

    const sessionB = SESSIONS.ALPHA_5;
    const messageList = page.locator(LOC.listScroll);
    const prevId = await messageList.getAttribute("data-messages-session-id");

    const sidB = await selectSession(page, sessionB.project, sessionB.count);

    // Prove the messages store actually transitioned away from
    // the previous session before asserting the final loaded
    // state.
    await expect(messageList).not.toHaveAttribute("data-messages-session-id", prevId!);

    await expectSessionLoaded(page, sidB, sessionB.displayRows);
  });
});
