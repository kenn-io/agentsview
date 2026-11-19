import { test, expect, type Page } from "@playwright/test";
import { clickNavTab } from "./helpers/nav";

// The fixture session created by createRecentEditsFixture in
// cmd/testfixture/main.go. It carries an Edit tool call on
// /src/server/handler.go with FilePath set directly so the feed
// filter (category IN ('Edit','Write') AND file_path IS NOT NULL)
// returns a row.
const FIXTURE_SESSION_ID = "test-session-recent-edits";

function fileRow(project: string, file: string, count: number, lastEditedAt?: string) {
  return {
    project,
    file_path: file,
    edit_count: count,
    ...(lastEditedAt ? { last_edited_at: lastEditedAt } : {}),
    last_session_id: "s1",
    edits: [],
    edits_truncated: false,
  };
}

async function withRecentEdits(page: Page) {
  await page.route("**/api/v1/recent-edits?*", (route) =>
    route.fulfill({
      json: {
        has_more: false,
        files: [
          fileRow(
            "2026-03-04-agentsview-long-project-name",
            "/src/server/handler.go",
            3,
            "2026-11-05T10:00:00Z",
          ),
          fileRow("tools", "/cmd/agentsview/main.go", 123456, "2026-11-01T10:00:00Z"),
          fileRow("tools", "/internal/db/sessions.go", 7),
        ],
      },
    }),
  );
}

test.describe("Recent Edits feed", () => {
  test.beforeEach(async ({ page }) => {
    await page.goto("/");
  });

  for (const width of [320, 375]) {
    test(`keeps the file name visible at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 800 });
      await page.goto("/recent-edits");
      const row = page.locator(".re-file-row").first();
      await expect(row).toBeVisible({ timeout: 5_000 });
      await expect(row.locator(".re-project")).toHaveAttribute("title", /.+/);

      const layout = await row.evaluate((element) => {
        const rowRect = element.getBoundingClientRect();
        const rect = (selector: string) => element.querySelector(selector)!.getBoundingClientRect();
        const path = element.querySelector<HTMLElement>(".re-path")!;
        const project = rect(".re-project");
        const pathRect = path.getBoundingClientRect();
        const count = rect(".re-count");
        return {
          pathWidth: pathRect.width,
          pathText: path.textContent ?? "",
          pathClipped: path.scrollWidth > path.clientWidth + 1,
          projectAbovePath: project.bottom <= pathRect.top + 1,
          countInsideRow: count.right <= rowRect.right + 1,
        };
      });
      // The fixture's short path ("server/handler.go") fits whole.
      expect(layout.pathText).toContain("handler.go");
      expect(layout.pathClipped).toBe(false);
      expect(layout.pathWidth).toBeGreaterThan(80);
      expect(layout.projectAbovePath).toBe(true);
      expect(layout.countInsideRow).toBe(true);
    });
  }

  test("header and rows share columns with long headings and rows without a time", async ({
    page,
  }) => {
    // French has the longest column headings ("Dernière modification").
    await page.addInitScript(() => localStorage.setItem("agentsview-locale", "fr"));
    await withRecentEdits(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);

    const edges = (selector: string) =>
      page.locator(selector).evaluateAll((els) =>
        els.map((el) => {
          const r = el.getBoundingClientRect();
          return { left: Math.round(r.left), right: Math.round(r.right) };
        }),
      );
    const label = (column: number) => `.re-list thead th:nth-child(${column}) .kit-th__label`;
    const head = {
      path: (await edges(label(2)))[0]!,
      count: (await edges(label(3)))[0]!,
      time: (await edges(label(4)))[0]!,
    };
    for (const path of await edges(".re-file-row .re-path")) expect(path.left).toBe(head.path.left);
    // Every count, including the six-digit one and the one in the row
    // with no time, ends at the Edits column's edge.
    const counts = await edges(".re-file-row .re-count");
    expect(counts).toHaveLength(3);
    for (const count of counts) expect(count.right).toBe(head.count.right);
    const times = await edges(".re-file-row .re-time");
    expect(times).toHaveLength(2);
    for (const time of times) expect(time.right).toBe(head.time.right);
  });

  for (const width of [641, 700]) {
    test(`keeps file names readable just above the stacked layout at ${width}px`, async ({
      page,
    }) => {
      await withRecentEdits(page);
      await page.setViewportSize({ width, height: 800 });
      await page.goto("/recent-edits");
      const path = page.locator(".re-file-row .re-path").first();
      await expect(path).toBeVisible();
      await expect(page.locator(".re-list thead")).toBeVisible();
      const widthPx = await path.evaluate((el) => el.getBoundingClientRect().width);
      // Enough for "server/handler.go" in the 12.5px mono face.
      expect(widthPx).toBeGreaterThanOrEqual(130);
    });
  }

  test("columns resize by dragging a header edge and keep their width", async ({ page }) => {
    await withRecentEdits(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);

    const layout = () =>
      page.locator(".re-list").evaluate((list) => ({
        columns: [...list.querySelectorAll("thead th")].map((th) =>
          Math.round(th.getBoundingClientRect().width),
        ),
        overflow:
          list.querySelector(".kit-table")!.getBoundingClientRect().width -
          list.getBoundingClientRect().width,
        projectClipped: (() => {
          const head = list.querySelector<HTMLElement>(".re-file-row .re-project-head")!;
          return head.scrollWidth > head.clientWidth + 1;
        })(),
      }));
    const before = await layout();
    // The table opens at the list's width, with no sideways scrolling.
    expect(before.overflow).toBeLessThanOrEqual(2);

    const handle = page.locator(".re-list thead th").first().getByRole("separator");
    const box = (await handle.boundingBox())!;
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.move(box.x + 100, box.y + box.height / 2, { steps: 6 });
    await page.mouse.up();

    const widened = await layout();
    expect(widened.columns[0]).toBeGreaterThanOrEqual(before.columns[0]! + 90);
    // The long project name was cut at the starting width and shows whole
    // once the column is wider.
    expect(before.projectClipped).toBe(true);
    expect(widened.projectClipped).toBe(false);

    await page.reload();
    await expect(page.locator(".re-file-row")).toHaveCount(3);
    const reloaded = await layout();
    expect(reloaded.columns[0]).toBe(widened.columns[0]);
    // The file column gives way, so the table still fits.
    expect(reloaded.overflow).toBeLessThanOrEqual(2);
  });

  const tableLayout = (page: Page) =>
    page.locator(".re-list").evaluate((list) => {
      const cols = [...list.querySelectorAll("thead th")].map((th) =>
        Math.round(th.getBoundingClientRect().width),
      );
      return {
        columns: cols,
        overflow:
          list.querySelector(".kit-table")!.getBoundingClientRect().width -
          list.getBoundingClientRect().width,
      };
    });

  test("columns nobody resized follow a window resize", async ({ page }) => {
    await withRecentEdits(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);
    const listWidth = () =>
      page.locator(".re-list").evaluate((el) => el.getBoundingClientRect().width);
    const wide = await tableLayout(page);
    const wideList = await listWidth();

    // Narrower: the table still fits, and the file column gives way.
    await page.setViewportSize({ width: 760, height: 800 });
    await expect.poll(listWidth).toBeLessThan(wideList - 200);
    await expect.poll(async () => (await tableLayout(page)).overflow).toBeLessThanOrEqual(2);
    const narrow = await tableLayout(page);
    expect(narrow.columns[1]).toBeLessThan(wide.columns[1]! - 100);

    // Wider again: the spare room goes back to the file column, not to
    // Last edit.
    await page.setViewportSize({ width: 1280, height: 800 });
    await expect.poll(async () => (await tableLayout(page)).columns).toEqual(wide.columns);
  });

  test("tabbing across resize handles keeps no width; resizing one keeps only that one", async ({
    page,
  }) => {
    await withRecentEdits(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);

    // File fills the spare width and has no handle: Project, Edits, and
    // Last edit do.
    const handles = page.locator(".re-list thead th").getByRole("separator");
    await expect(handles).toHaveCount(3);
    await handles.first().focus();
    await page.keyboard.press("Tab");
    await expect(handles.nth(1)).toBeFocused();
    // Widen Edits by one keyboard step.
    await page.keyboard.press("ArrowRight");

    const kept = await page.evaluate(() =>
      Object.keys(JSON.parse(localStorage.getItem("agentsview-recent-edits-columns") ?? "{}")),
    );
    expect(kept).toEqual(["edits"]);
  });

  const keptColumns = (page: Page) =>
    page.evaluate(() =>
      Object.keys(JSON.parse(localStorage.getItem("agentsview-recent-edits-columns") ?? "{}")),
    );

  test("File has no resize handle, takes the spare width, and ignores a stored width", async ({
    page,
  }) => {
    await withRecentEdits(page);
    await page.addInitScript(() =>
      localStorage.setItem("agentsview-recent-edits-columns", JSON.stringify({ file: 200 })),
    );
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);

    const file = page.locator(".re-list thead th").nth(1);
    await expect(file).toHaveText(/File/);
    await expect(file.getByRole("separator")).toHaveCount(0);
    const layout = await tableLayout(page);
    // The four columns fill the list without sideways scrolling, and File
    // is far wider than the stored 200px.
    expect(layout.overflow).toBeLessThanOrEqual(2);
    expect(layout.columns[1]).toBeGreaterThan(400);
  });

  test("a phone resize is not kept once the window is wide again", async ({ page }) => {
    await withRecentEdits(page);
    await page.setViewportSize({ width: 400, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);
    // Stacked: File fills and has no handle; Edits is the second column.
    // Narrow it by one keyboard step.
    await page.locator(".re-list thead th").nth(1).getByRole("separator").focus();
    await page.keyboard.press("ArrowLeft");
    expect(await keptColumns(page)).toEqual([]);

    await page.setViewportSize({ width: 1280, height: 800 });
    await expect(page.locator(".re-list thead th")).toHaveCount(4);
    // Wide: Edits is the third column. Widen it by one keyboard step.
    await page.locator(".re-list thead th").nth(2).getByRole("separator").focus();
    await page.keyboard.press("ArrowRight");
    await expect.poll(() => keptColumns(page)).toEqual(["edits"]);
  });

  test("a width set in one layout does not carry into the other", async ({ page }) => {
    await withRecentEdits(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);
    const headers = page.locator(".re-list thead th");
    const width = (i: number) =>
      headers.nth(i).evaluate((th) => Math.round(th.getBoundingClientRect().width));

    // Wide: widen Edits (third column) well past its 84px default.
    await headers.nth(2).getByRole("separator").focus();
    for (let i = 0; i < 6; i++) await page.keyboard.press("ArrowRight");
    await expect.poll(() => keptColumns(page)).toEqual(["edits"]);
    const wideEdits = await width(2);
    expect(wideEdits).toBeGreaterThan(100);

    // Phone: Edits (second column) starts at the phone default again.
    await page.setViewportSize({ width: 400, height: 800 });
    await expect(headers).toHaveCount(3);
    await expect.poll(() => width(1)).toBe(52);
    await headers.nth(1).getByRole("separator").focus();
    await page.keyboard.press("ArrowLeft");

    // Wide again: Edits has its kept desktop width, not the phone one.
    await page.setViewportSize({ width: 1280, height: 800 });
    await expect(headers).toHaveCount(4);
    await expect.poll(() => width(2)).toBe(wideEdits);
  });

  test("a window resize keeps an expanded file's height and the rows in view", async ({ page }) => {
    const edits = Array.from({ length: 10 }, (_, i) => ({
      session_id: `session-${i}`,
      ordinal: i,
      call_index: 0,
      tool_name: "Edit",
      category: "Edit",
      timestamp: "2026-11-05T10:00:00Z",
    }));
    const files = Array.from({ length: 80 }, (_, i) => ({
      ...fileRow(
        "tools",
        `/src/pkg/file-${String(i).padStart(2, "0")}.go`,
        10,
        "2026-11-05T10:00:00Z",
      ),
      edits,
    }));
    await page.route("**/api/v1/recent-edits?*", (route) =>
      route.fulfill({ json: { has_more: false, files } }),
    );
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row").first()).toBeVisible();

    // Expand the first file, then scroll it out of view above.
    await page.locator(".re-file-row").first().click();
    await expect(page.locator(".re-edits-row")).toHaveCount(1);
    const anchor = await page.locator(".re-list").evaluate((list) => {
      let scroller: HTMLElement | null = list.parentElement;
      while (scroller && !/(auto|scroll)/.test(getComputedStyle(scroller).overflowY)) {
        scroller = scroller.parentElement;
      }
      const el = scroller ?? (document.scrollingElement as HTMLElement);
      el.scrollTop += 1200;
      return el.scrollTop;
    });
    expect(anchor).toBeGreaterThan(600);
    // The table renders rows for a new scroll position on its next frame,
    // so wait for a rendered row below the sticky header before reading.
    const rowInView = () =>
      page.locator(".re-file-row").evaluateAll((rows) => {
        const row = rows.find((r) => r.getBoundingClientRect().top >= 120);
        if (!row) return null;
        return {
          path: row.querySelector(".re-path")!.getAttribute("title"),
          top: Math.round(row.getBoundingClientRect().top),
        };
      });
    const firstInView = async () => {
      await expect.poll(rowInView).not.toBeNull();
      return (await rowInView())!;
    };
    const before = await firstInView();
    const fileWidth = () =>
      page
        .locator(".re-list thead th")
        .nth(1)
        .evaluate((th) => th.getBoundingClientRect().width);
    const wideFile = await fileWidth();

    // Narrow the window below the page's 1088px cap: File gives way, and
    // the rows in view stay put.
    await page.setViewportSize({ width: 1000, height: 800 });
    // The old remount resized the columns only once it rebuilt the table,
    // so waiting for File to narrow also waits for any rebuild.
    await expect.poll(fileWidth).toBeLessThan(wideFile - 50);
    // Same row, within subpixel rounding of the same place.
    const after = await firstInView();
    expect(after.path).toBe(before.path);
    expect(Math.abs(after.top - before.top)).toBeLessThanOrEqual(2);
  });

  test("expand file row and jump to session transcript", async ({ page }) => {
    // Navigate to Recent Edits via its TopBar tab.
    await clickNavTab(page, "Recent Edits");

    await expect(page.locator(".re-file-row").first()).toBeVisible({ timeout: 5_000 });

    // Expand the first file row.
    await page.locator(".re-file-row").first().click();

    // The edits panel should appear with at least one jump button.
    const editsPanel = page.locator(".re-edits").first();
    await expect(editsPanel).toBeVisible({ timeout: 3_000 });
    const editBtn = editsPanel.locator(".re-edit").first();
    await expect(editBtn).toBeVisible();

    // Click the edit to jump to the session transcript.
    await editBtn.click();

    // The message list must reflect the fixture session.
    const ml = page.locator(".message-list-scroll");
    await expect(ml).toHaveAttribute("data-session-id", FIXTURE_SESSION_ID, { timeout: 5_000 });
    await expect(ml).toHaveAttribute("data-loaded", "true", {
      timeout: 5_000,
    });
  });
});
