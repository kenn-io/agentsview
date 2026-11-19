import { expect, test, type Locator, type Page } from "@playwright/test";
import type { DbMessage as Message } from "../src/lib/api/generated/index.js";

const SESSION_ID = "test-session-xlarge-5500";

function message(ordinal: number, content: string): Message {
  return {
    id: 880000 + ordinal,
    session_id: SESSION_ID,
    ordinal,
    role: ordinal % 2 === 0 ? "user" : "assistant",
    content,
    content_length: content.length,
    timestamp: "2026-01-01T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    has_context_tokens: false,
    has_output_tokens: false,
    is_system: false,
  };
}

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: "ignoreErrors" });
});

async function waitForScrollSettled(transcript: Locator): Promise<number> {
  let last = -1;
  await expect
    .poll(
      async () => {
        const top = await transcript.evaluate((el) => el.scrollTop);
        const settled = top === last;
        last = top;
        return settled;
      },
      { intervals: [300] },
    )
    .toBe(true);
  return last;
}

async function installLiveSession(page: Page) {
  const all = Array.from({ length: 300 }, (_, i) =>
    message(i, i === 5 || i === 250 ? `needle at ${i}` : `Message ${i}`),
  );
  await page.addInitScript(() => {
    localStorage.setItem("agentsview-message-layout", "skim");
    localStorage.setItem("agentsview-transcript-mode", "normal");
  });
  await page.route(`**/api/v1/sessions/${SESSION_ID}`, async (route) => {
    const response = await route.fetch();
    const session = await response.json();
    await route.fulfill({ response, json: { ...session, message_count: all.length } });
  });
  await page.route(`**/api/v1/sessions/${SESSION_ID}/messages*`, async (route) => {
    const url = new URL(route.request().url());
    const descending = url.searchParams.get("direction") === "desc";
    const from = Number(url.searchParams.get("from") ?? (descending ? all.length - 1 : 0));
    const limit = Number(url.searchParams.get("limit") ?? 1000);
    const selected = descending
      ? all.slice(Math.max(0, from - limit + 1), from + 1).reverse()
      : all.slice(from, from + limit);
    await route.fulfill({ json: { messages: selected, count: selected.length } });
  });
  // The first watch connection stays open until the test emits an update.
  let emit!: () => void;
  const gate = new Promise<void>((resolve) => {
    emit = resolve;
  });
  let served = false;
  await page.route(`**/api/v1/sessions/${SESSION_ID}/watch*`, async (route) => {
    if (served) return new Promise<void>(() => {});
    served = true;
    await gate;
    await route.fulfill({
      status: 200,
      contentType: "text/event-stream",
      body: "event: session_updated\ndata: {}\n\n",
    });
  });
  return {
    refresh: () => {
      for (let i = 0; i < 5; i++) {
        const ordinal = all.length;
        all.push(message(ordinal, `Live message ${ordinal}`));
      }
      emit();
    },
  };
}

for (const newestFirst of [false, true])
  for (const withFind of [false, true]) {
    test(`a live refresh does not move the transcript (newest first ${newestFirst}, find ${withFind})`, async ({
      page,
    }) => {
      const live = await installLiveSession(page);
      await page.goto(`/sessions/${SESSION_ID}`);
      const transcript = page.locator(".message-list-scroll");
      await expect(transcript).toHaveAttribute("data-loaded", "true");

      if (newestFirst) {
        await page.getByRole("button", { name: "Toggle sort order (o)" }).click();
      }

      if (withFind) {
        const modifier = await page.evaluate(() =>
          /Mac/.test(navigator.platform) ? "Meta" : "Control",
        );
        await page.keyboard.press(`${modifier}+f`);
        await page.locator(".kit-find-bar__input").fill("needle");
        await expect(page.locator(".search-announcement")).toHaveText(/^Match \d+ of 2$/);
      }

      // Let the initial reveal settle so it cannot race the user's scroll.
      await waitForScrollSettled(transcript);

      // The user scrolls away from the current match.
      const settledTop = await transcript.evaluate((el) => el.scrollTop);
      await transcript.hover();
      await page.mouse.wheel(0, 6000);
      await expect.poll(() => transcript.evaluate((el) => el.scrollTop)).not.toBe(settledTop);
      // wheel() returns before WebKit finishes scrolling and measuring rows.
      // Compare the refresh against the user's final position, not an intermediate frame.
      const before = await waitForScrollSettled(transcript);

      // The refresh only counts once the client has fetched the appended rows.
      const reloaded = page.waitForResponse(
        async (r) =>
          /\/messages/.test(r.url()) && r.ok() && (await r.text()).includes("Live message 304"),
      );
      live.refresh();
      await reloaded;
      // Nothing observable marks "the reveal did not run", so give a stray
      // re-reveal time to land before comparing offsets.
      await page.waitForTimeout(500);

      const after = await transcript.evaluate((el) => el.scrollTop);
      // Row re-measurement can nudge the offset by a few hundred pixels; a reveal
      // jumps thousands of pixels to the match.
      expect(Math.abs(after - before)).toBeLessThan(1000);
    });
  }
