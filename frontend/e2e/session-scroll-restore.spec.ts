import { test, expect, type Locator, type Page, type Route } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";
import { waitForStableValue } from "./helpers/virtual-list-helpers";

const READ_SESSION = "test-session-large-200";
const OTHER_SESSION = "test-session-medium-100";
const READ_SESSION_MESSAGES = 200;

/** Rows from one to nine paragraphs, so real heights differ from the
 *  list's per-row estimate the way real transcripts do. */
function readSessionMessage(ordinal: number) {
  const paragraphs = Array.from(
    { length: 1 + ((ordinal * 7) % 9) },
    (_, n) =>
      `Paragraph ${n} of row ${ordinal} wraps across the message width so this row is taller or shorter than its neighbours.`,
  );
  const content = [`Scroll anchor row ${ordinal}.`, ...paragraphs].join("\n\n");
  return {
    id: ordinal + 1,
    session_id: READ_SESSION,
    ordinal,
    role: ordinal % 2 === 0 ? "user" : "assistant",
    content,
    timestamp: new Date(Date.UTC(2026, 0, 1, 0, ordinal)).toISOString(),
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: content.length,
    model: "",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    has_context_tokens: false,
    has_output_tokens: false,
    tool_calls: [],
    is_system: false,
  };
}

async function fulfillReadSessionPage(route: Route) {
  const url = new URL(route.request().url());
  const desc = url.searchParams.get("direction") === "desc";
  const limit = Number(url.searchParams.get("limit") ?? READ_SESSION_MESSAGES);
  const fromParam = url.searchParams.get("from");
  const from = fromParam === null ? (desc ? READ_SESSION_MESSAGES - 1 : 0) : Number(fromParam);
  const ordinals = Array.from({ length: READ_SESSION_MESSAGES }, (_, i) => i)
    .filter((ordinal) => (desc ? ordinal <= from : ordinal >= from))
    .sort((a, b) => (desc ? b - a : a - b))
    .slice(0, limit);
  await route.fulfill({
    json: { messages: ordinals.map(readSessionMessage), count: ordinals.length },
  });
}

async function openSession(page: Page, sessionId: string) {
  await page.locator(`.session-item[data-session-id="${sessionId}"]`).click();
  await expect(
    page
      .locator(
        `.message-list-scroll[data-messages-session-id="${sessionId}"][data-loaded="true"] .virtual-row`,
      )
      .first(),
  ).toBeVisible();
}

interface ReadingPosition {
  /** Ordinal parsed from the first row visible at the viewport top. */
  ordinal: number | null;
  /** Pixels the viewport top sits below that row's top. */
  offset: number;
}

function readingPosition(scroller: Locator): Promise<ReadingPosition> {
  return scroller.evaluate((el) => {
    const top = el.getBoundingClientRect().top;
    const first = [...el.querySelectorAll(".virtual-row")]
      .map((row) => ({ row, rect: row.getBoundingClientRect() }))
      .filter(({ rect }) => rect.bottom > top + 0.5)
      .sort((a, b) => a.rect.top - b.rect.top)[0];
    const match = first?.row.textContent?.match(/Scroll anchor row (\d+)\./);
    return {
      ordinal: match ? Number(match[1]) : null,
      offset: first ? Math.round(top - first.rect.top) : 0,
    };
  });
}

/** Wait until row measurements and restore corrections stop moving the
 *  viewport, then report where it ended up. */
function settledReadingPosition(scroller: Locator): Promise<ReadingPosition> {
  return waitForStableValue(
    () => readingPosition(scroller),
    800,
    100,
    10_000,
    (a, b) => a.ordinal === b.ordinal && a.offset === b.offset,
  );
}

async function scrollWithWheel(page: Page, scroller: Locator, steps: number) {
  const box = await scroller.boundingBox();
  if (!box) throw new Error("message list is not visible");
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  for (let i = 0; i < steps; i++) {
    await page.mouse.wheel(0, 400);
  }
}

test.describe("Session scroll restore", () => {
  test.beforeEach(async ({ page }) => {
    await page.route(`**/api/v1/sessions/${READ_SESSION}/messages*`, fulfillReadSessionPage);
  });

  test("returns to the same reading position after repeated session switches", async ({ page }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await openSession(page, READ_SESSION);

    await scrollWithWheel(page, sp.scroller, 30);
    const before = await settledReadingPosition(sp.scroller);
    expect(before.ordinal).toBeGreaterThan(10);

    for (let round = 1; round <= 4; round++) {
      await openSession(page, OTHER_SESSION);
      await openSession(page, READ_SESSION);

      const after = await settledReadingPosition(sp.scroller);
      expect(after.ordinal, `row at the top after round ${round}`).toBe(before.ordinal);
      expect(
        Math.abs(after.offset - before.offset),
        `offset drift after round ${round}`,
      ).toBeLessThanOrEqual(2);
    }
  });

  test("opens a session that has not been shown yet at the top", async ({ page }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await openSession(page, READ_SESSION);
    await scrollWithWheel(page, sp.scroller, 30);
    await settledReadingPosition(sp.scroller);

    await openSession(page, OTHER_SESSION);

    const scrollTop = await waitForStableValue(
      () => sp.scroller.evaluate((el) => el.scrollTop),
      800,
      100,
      10_000,
    );
    expect(scrollTop).toBe(0);
  });
});
