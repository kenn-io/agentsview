import { expect, test, type Page } from "@playwright/test";
import type { Message } from "../src/lib/api/types.js";

const SESSION_ID = "test-session-xlarge-5500";
const QUERY = "needle";
/** 5:text (2), 7:thinking, 9:code, 12:tool-output, 12:tool-history, 5100:text. */
const ALL_MATCHES = 7;
/** The same session without the tool blocks (tool filter off or focused mode). */
const PROSE_MATCHES = 5;
const ALL_KEYS = [
  "12:tool-history:0.0",
  "12:tool-output:0",
  "5100:text:0",
  "5:text:0",
  "7:thinking:0",
  "9:code:0",
];
const TOOL_KEYS = ["12:tool-history:0.0", "12:tool-output:0"];

function fixtureMessages(): Message[] {
  const messages: Message[] = Array.from({ length: 5500 }, (_, ordinal) => {
    const content = `Message ${ordinal}`;
    return {
      id: ordinal + 1,
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
      is_system: false,
    };
  });
  function replace(ordinal: number, content: string, extra: Partial<Message> = {}) {
    Object.assign(messages[ordinal]!, { content, content_length: content.length }, extra);
  }
  replace(5, "needle first occurrence, needle second occurrence.");
  replace(7, "[Thinking]\nneedle thinking\n[/Thinking]", { has_thinking: true });
  replace(9, "```typescript\nconst needle = 42;\nconst tail = true;\n```");
  replace(12, "", {
    role: "assistant",
    has_tool_use: true,
    tool_calls: [
      {
        tool_name: "Bash",
        category: "Bash",
        input_json: JSON.stringify({ command: "printf safe" }),
        result_content: `${"output line\n".repeat(80)}needle in output\n`,
        result_events: [
          {
            event_index: 0,
            status: "completed",
            source: "wait_output",
            content: "needle history\n",
            content_length: 15,
          },
        ],
      },
    ],
  });
  replace(5100, "needle newest occurrence");
  return messages;
}

interface FixtureOptions {
  mode?: "normal" | "focused";
  hidden?: string[];
}

async function installFixture(page: Page, options: FixtureOptions = {}) {
  const messages = fixtureMessages();
  const mode = options.mode ?? "normal";
  const hidden = options.hidden ?? [];
  const requests: number[] = [];
  let releaseHistory!: () => void;
  const historyGate = new Promise<void>((resolve) => {
    releaseHistory = resolve;
  });
  await page.addInitScript(
    ({ filters, mode }) => {
      localStorage.setItem("agentsview-block-filters", filters);
      localStorage.setItem("agentsview-transcript-mode", mode);
      localStorage.setItem("agentsview-message-layout", "skim");
    },
    { filters: JSON.stringify({ hidden }), mode },
  );
  await page.route(`**/api/v1/sessions/${SESSION_ID}/messages*`, async (route) => {
    const url = new URL(route.request().url());
    const descending = url.searchParams.get("direction") === "desc";
    const from = Number(url.searchParams.get("from") ?? (descending ? messages.length - 1 : 0));
    const limit = Number(url.searchParams.get("limit") ?? 1000);
    requests.push(from);
    if (descending && from < messages.length - 1) await historyGate;
    const selected = descending
      ? messages.slice(Math.max(0, from - limit + 1), from + 1).reverse()
      : messages.slice(from, from + limit);
    await route.fulfill({ json: { messages: selected, count: selected.length } });
  });
  return { requests, releaseHistory, messages };
}

async function openSession(page: Page) {
  await page.goto(`/sessions/${SESSION_ID}`);
  const transcript = page.locator(".message-list-scroll");
  await expect(transcript).toHaveAttribute("data-messages-session-id", SESSION_ID);
  await expect(transcript).toHaveAttribute("data-loaded", "true");
  return transcript;
}

async function openFind(page: Page) {
  const modifier = await page.evaluate(() => (/Mac/.test(navigator.platform) ? "Meta" : "Control"));
  await page.keyboard.press(`${modifier}+f`);
  const input = page.locator(".kit-find-bar__input");
  await expect(input).toBeFocused();
  await input.fill(QUERY);
  return input;
}

async function expectCompleteIndex(page: Page, total = ALL_MATCHES) {
  await expect(page.locator(".search-announcement")).toHaveText(
    new RegExp(`^Match \\d+ of ${total}$`),
  );
}

async function openResults(page: Page) {
  await page.getByRole("button", { name: "Show search results" }).click();
  await expect(page.getByRole("region", { name: "Search results" })).toBeVisible();
}

async function toggleBlockFilter(page: Page, label: string) {
  const toggle = page.getByRole("button", { name: "Filter block types" });
  await toggle.click();
  await expect(page.locator(".block-filter-dropdown")).toHaveCount(1);
  await page
    .locator(".block-filter-item")
    .filter({ has: page.locator(".block-filter-label", { hasText: label }) })
    .click();
  if (await page.locator(".block-filter-dropdown").count()) await toggle.click();
  await expect(page.locator(".block-filter-dropdown")).toHaveCount(0);
}

async function setTranscriptMode(page: Page, mode: "normal" | "focused") {
  await page
    .getByRole("button", {
      name: mode === "normal" ? "Normal transcript mode" : "Focused transcript mode",
    })
    .click();
}

/** Inspect actual browser ranges and clipping, not a mocked virtualizer. */
async function currentGeometry(page: Page) {
  return page.evaluate(() => {
    const root = document.querySelector<HTMLElement>(".message-list-scroll");
    const block = root?.querySelector<HTMLElement>('[data-search-current="true"]');
    if (!root || !block) return null;
    const css = CSS as typeof CSS & {
      highlights?: Map<string, Iterable<AbstractRange>>;
    };
    const ranges = Array.from(css.highlights?.get("av-find-current") ?? []);
    const selected = ranges[0];
    const rect = block.getBoundingClientRect();
    let target = rect;
    let text: string | null = null;
    let offset = -1;
    if (selected) {
      if (ranges.length !== 1 || !block.contains(selected.startContainer)) return null;
      const range = document.createRange();
      range.setStart(selected.startContainer, selected.startOffset);
      range.setEnd(selected.endContainer, selected.endOffset);
      target = range.getBoundingClientRect();
      text = range.toString();
      const prefix = document.createRange();
      prefix.selectNodeContents(block);
      prefix.setEnd(selected.startContainer, selected.startOffset);
      offset = prefix.toString().length;
    }
    let top = 0;
    let bottom = innerHeight;
    let left = 0;
    let right = innerWidth;
    for (let node: HTMLElement | null = block; node; node = node.parentElement) {
      const style = getComputedStyle(node);
      const clip = node.getBoundingClientRect();
      if (/(auto|scroll|hidden|clip)/.test(style.overflowY)) {
        top = Math.max(top, clip.top);
        bottom = Math.min(bottom, clip.bottom);
      }
      if (/(auto|scroll|hidden|clip)/.test(style.overflowX)) {
        left = Math.max(left, clip.left);
        right = Math.min(right, clip.right);
      }
      if (node === root) break;
    }
    return {
      key: block.dataset.searchBlock,
      native: !!selected,
      text,
      offset,
      visible:
        target.height > 0 &&
        target.bottom > top &&
        target.top < bottom &&
        target.right > left &&
        target.left < right,
    };
  });
}

/** Step through every occurrence, asserting each one is truly visible. */
async function stepOccurrences(page: Page, total: number, direction: "F3" | "Shift+F3") {
  const counter = page.locator(".kit-find-bar__counter");
  const steps: NonNullable<Awaited<ReturnType<typeof currentGeometry>>>[] = [];
  for (let index = 0; index < total; index++) {
    const before = await counter.innerText();
    await page.keyboard.press(direction);
    await expect(counter).not.toHaveText(before);
    await expect.poll(async () => (await currentGeometry(page))?.visible).toBe(true);
    const current = await currentGeometry(page);
    expect(current).not.toBeNull();
    steps.push(current!);
  }
  return steps;
}

test.describe("In-session find", () => {
  test.setTimeout(60_000);

  test("filters whole words and restores substring results", async ({ page }) => {
    const fixture = await installFixture(page);
    fixture.messages[5]!.content = "needlework need**le** needle_";
    fixture.messages[5]!.content_length = fixture.messages[5]!.content.length;
    fixture.releaseHistory();
    await openSession(page);
    await openFind(page);
    await expectCompleteIndex(page, 8);
    const wholeWord = page.getByRole("button", { name: "Match whole word", exact: true });
    await wholeWord.click();
    await expect(wholeWord).toHaveAttribute("aria-pressed", "true");
    await expectCompleteIndex(page, 6);
    await openResults(page);
    const steps = await stepOccurrences(page, 6, "F3");
    expect(steps.every((step) => step.visible)).toBe(true);
    expect(steps.filter((step) => step.key === "5:text:0")).toHaveLength(1);
    await wholeWord.click();
    await expect(wholeWord).toHaveAttribute("aria-pressed", "false");
    await expectCompleteIndex(page, 8);
  });

  test("reveals every occurrence through nested scrolling and restores state on close", async ({
    page,
    browserName,
  }) => {
    const fixture = await installFixture(page);
    const transcript = await openSession(page);
    await expect(transcript).toHaveClass(/layout-skim/);
    await openFind(page);
    await expect(page.locator(".search-announcement")).toHaveText("Loading older messages…");
    fixture.releaseHistory();
    await expectCompleteIndex(page, ALL_MATCHES);
    await expect(transcript).not.toHaveClass(/layout-skim/);
    expect(fixture.requests.some((from) => from < 1000)).toBe(true);

    await openResults(page);
    const steps = await stepOccurrences(page, ALL_MATCHES, "F3");
    const blocks = new Set(steps.map((step) => step.key!));
    const occurrences = new Set(steps.map((step) => `${step.key}:${step.offset}`));
    expect([...blocks].sort()).toEqual(ALL_KEYS);
    if (browserName === "chromium") {
      expect(steps.every((step) => step.native)).toBe(true);
      expect(occurrences.size).toBe(ALL_MATCHES);
      expect(steps.every((step) => step.text === QUERY)).toBe(true);
    }
    await expect(transcript.locator("mark")).toHaveCount(0);

    const rail = page.getByRole("button", { name: `${ALL_MATCHES} matches in this session` });
    await rail.focus();
    await rail.press("Home");
    await expect(transcript.locator('[data-search-current="true"]')).toHaveAttribute(
      "data-search-block",
      "5:text:0",
    );
    await expect.poll(async () => (await currentGeometry(page))?.visible).toBe(true);
    await rail.press("End");
    await expect(transcript.locator('[data-search-current="true"]')).toHaveAttribute(
      "data-search-block",
      "5100:text:0",
    );
    await expect.poll(async () => (await currentGeometry(page))?.visible).toBe(true);

    await page.locator(".find-result-button").first().click();
    await expect.poll(async () => (await currentGeometry(page))?.visible).toBe(true);
    await page.keyboard.press("Escape");
    await expect(page.locator(".session-find")).toHaveCount(0);
    await expect(page.locator("#session-find-results")).toHaveCount(0);
    await expect(transcript).toHaveClass(/layout-skim/);
    await expect(transcript.locator("[data-search-current]")).toHaveCount(0);
    expect(
      await page.evaluate(() => ({
        filters: localStorage.getItem("agentsview-block-filters"),
        mode: localStorage.getItem("agentsview-transcript-mode"),
        layout: localStorage.getItem("agentsview-message-layout"),
      })),
    ).toEqual({
      filters: JSON.stringify({ hidden: [] }),
      mode: "normal",
      layout: "skim",
    });
  });

  test("excludes tool calls hidden at start and restores them when the filter returns", async ({
    page,
  }) => {
    const fixture = await installFixture(page, { hidden: ["tool"] });
    fixture.releaseHistory();
    const transcript = await openSession(page);
    await openFind(page);
    await expectCompleteIndex(page, PROSE_MATCHES);
    await openResults(page);
    const hiddenSteps = await stepOccurrences(page, PROSE_MATCHES, "F3");
    const hiddenKeys = new Set(hiddenSteps.map((step) => step.key!));
    expect(hiddenKeys.has("12:tool-output:0")).toBe(false);
    expect(hiddenKeys.has("12:tool-history:0.0")).toBe(false);
    await expect(transcript.locator(".tool-block")).toHaveCount(0);

    await toggleBlockFilter(page, "Tool calls");
    await expectCompleteIndex(page, ALL_MATCHES);
    const shownSteps = await stepOccurrences(page, ALL_MATCHES, "F3");
    const shownKeys = new Set(shownSteps.map((step) => step.key!));
    expect(shownKeys.has("12:tool-output:0")).toBe(true);
    expect(shownKeys.has("12:tool-history:0.0")).toBe(true);

    await toggleBlockFilter(page, "Tool calls");
    await expectCompleteIndex(page, PROSE_MATCHES);
    await expect
      .poll(async () => {
        const current = await currentGeometry(page);
        return current !== null && current.visible && !TOOL_KEYS.includes(current.key ?? "");
      })
      .toBe(true);
    await expect(transcript.locator("mark")).toHaveCount(0);
  });

  test("keeps the current occurrence valid when a filter hides it mid-search", async ({ page }) => {
    const fixture = await installFixture(page);
    fixture.releaseHistory();
    const transcript = await openSession(page);
    const input = await openFind(page);
    await expectCompleteIndex(page, ALL_MATCHES);

    // Select a tool occurrence, then hide the filter that owns it.
    let guard = 0;
    while (guard++ < ALL_MATCHES) {
      const current = await currentGeometry(page);
      if (current?.key === "12:tool-output:0") break;
      await page.keyboard.press("F3");
      await page.waitForTimeout(250);
    }
    expect((await currentGeometry(page))?.key).toBe("12:tool-output:0");
    await expect.poll(async () => (await currentGeometry(page))?.visible).toBe(true);

    await toggleBlockFilter(page, "Tool calls");
    await expectCompleteIndex(page, PROSE_MATCHES);
    await expect
      .poll(async () => {
        const current = await currentGeometry(page);
        return current !== null && current.visible && !TOOL_KEYS.includes(current.key ?? "");
      })
      .toBe(true);
    await expect(transcript.locator('[data-search-current="true"]')).not.toHaveCount(0);
    await expect(transcript.locator("[data-search-current]")).toHaveCount(1);

    await toggleBlockFilter(page, "Tool calls");
    await expectCompleteIndex(page, ALL_MATCHES);
    await expect(input).toHaveValue(QUERY);
    await expect.poll(async () => (await currentGeometry(page))?.visible).toBe(true);
  });

  test("keeps counts and block navigation without the Highlight API", async ({ page }) => {
    // Counts, reveal geometry, and navigation come from the block model, so a
    // browser without CSS Highlight must still search and step occurrences.
    await page.addInitScript(() => {
      Object.defineProperty(window, "Highlight", { configurable: true, value: undefined });
    });
    const fixture = await installFixture(page, { hidden: ["tool"] });
    fixture.releaseHistory();
    const transcript = await openSession(page);
    await openFind(page);
    await expectCompleteIndex(page, PROSE_MATCHES);
    const hiddenSteps = await stepOccurrences(page, PROSE_MATCHES, "F3");
    expect(hiddenSteps.every((step) => step.native === false)).toBe(true);
    expect(hiddenSteps.every((step) => step.visible === true)).toBe(true);
    expect(hiddenSteps.some((step) => TOOL_KEYS.includes(step.key ?? ""))).toBe(false);
    await expect(transcript.locator('[data-search-current="true"]')).toHaveCount(1);
    await expect(transcript.locator("mark")).toHaveCount(0);
    expect(
      await page.evaluate(() => {
        const css = CSS as typeof CSS & { highlights?: Map<string, unknown> };
        return [css.highlights?.has("av-find"), css.highlights?.has("av-find-current")];
      }),
    ).not.toContain(true);

    // The tool filter still owns its blocks when it returns.
    await toggleBlockFilter(page, "Tool calls");
    await expectCompleteIndex(page, ALL_MATCHES);
    const shownSteps = await stepOccurrences(page, ALL_MATCHES, "F3");
    expect(shownSteps.every((step) => step.native === false)).toBe(true);
    expect(shownSteps.every((step) => step.visible === true)).toBe(true);
    expect(shownSteps.some((step) => step.key === "12:tool-output:0")).toBe(true);

    // Step backwards through the same model.
    const backwards = await stepOccurrences(page, 3, "Shift+F3");
    expect(backwards.every((step) => step.native === false && step.visible === true)).toBe(true);
  });

  test("treats focused mode as the search range and follows live mode changes", async ({
    page,
  }) => {
    const fixture = await installFixture(page, { mode: "focused" });
    fixture.releaseHistory();
    await openSession(page);
    await openFind(page);
    // Focused mode drops the intermediate assistant tool row, so its output and
    // history are neither searchable nor rendered.
    await expectCompleteIndex(page, PROSE_MATCHES);
    const focusedSteps = await stepOccurrences(page, PROSE_MATCHES, "F3");
    expect(focusedSteps.some((step) => TOOL_KEYS.includes(step.key ?? ""))).toBe(false);

    await setTranscriptMode(page, "normal");
    await expectCompleteIndex(page, ALL_MATCHES);
    const normalSteps = await stepOccurrences(page, ALL_MATCHES, "F3");
    expect(normalSteps.some((step) => step.key === "12:tool-output:0")).toBe(true);

    await setTranscriptMode(page, "focused");
    await expectCompleteIndex(page, PROSE_MATCHES);
    expect(await page.evaluate(() => localStorage.getItem("agentsview-transcript-mode"))).toBe(
      "focused",
    );
  });

  test("rapid edits and closing do not resurrect stale ranges", async ({ page }) => {
    const fixture = await installFixture(page);
    fixture.releaseHistory();
    const transcript = await openSession(page);
    const input = await openFind(page);
    await expectCompleteIndex(page, ALL_MATCHES);
    await input.fill("needle history");
    await input.fill("no-such-search-occurrence");
    await expect(transcript.locator("[data-search-current]")).toHaveCount(0);
    await expect(page.locator(".search-announcement")).not.toContainText("Match ");
    await input.fill(QUERY);
    await input.press("Escape");
    await expect(page.locator(".session-find")).toHaveCount(0);
    // Two rendering frames cover the mutation-observer repaint after teardown.
    await page.evaluate(
      () =>
        new Promise<void>((resolve) => {
          requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
        }),
    );
    await expect(transcript.locator("[data-search-current]")).toHaveCount(0);
    expect(
      await page.evaluate(() => {
        const css = CSS as typeof CSS & { highlights?: Map<string, unknown> };
        return [css.highlights?.has("av-find"), css.highlights?.has("av-find-current")];
      }),
    ).not.toContain(true);
  });

  test("switching sessions cancels a pending historical reveal", async ({ page }) => {
    const fixture = await installFixture(page);
    await openSession(page);
    await openFind(page);
    await expect(page.locator(".search-announcement")).toHaveText("Loading older messages…");
    const nextSession = page
      .locator(`.session-item:not([data-session-id="${SESSION_ID}"])`)
      .first();
    const nextId = await nextSession.getAttribute("data-session-id");
    expect(nextId).toBeTruthy();
    await nextSession.click();
    const transcript = page.locator(".message-list-scroll");
    await expect(transcript).toHaveAttribute("data-messages-session-id", nextId!);
    fixture.releaseHistory();
    await expect(transcript).toHaveAttribute("data-loaded", "true");
    await expect(transcript.locator("[data-search-current]")).toHaveCount(0);
    await expect(transcript).toHaveAttribute("data-messages-session-id", nextId!);
  });
});
