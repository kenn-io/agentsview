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

  for (const width of [521, 600]) {
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
    const listWidth = () => page.locator(".re-list").evaluate((el) => el.getBoundingClientRect().width);
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

    const handles = page.locator(".re-list thead th").getByRole("separator");
    await handles.first().focus();
    await page.keyboard.press("Tab");
    await expect(handles.nth(1)).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(handles.nth(2)).toBeFocused();
    // Widen Edits (the third column) by one keyboard step.
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

  test("clicking a resize handle without dragging keeps no width", async ({ page }) => {
    await withRecentEdits(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);

    const handle = page.locator(".re-list thead th").nth(1).getByRole("separator");
    const box = (await handle.boundingBox())!;
    await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
    expect(await keptColumns(page)).toEqual([]);
  });

  test("clicking Last edit's handle after narrowing a column keeps no Last edit width", async ({
    page,
  }) => {
    await withRecentEdits(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);
    const handles = page.locator(".re-list thead th").getByRole("separator");

    // Narrow Project by four keyboard steps; Last edit stretches to fill.
    await handles.first().focus();
    for (let i = 0; i < 4; i++) await page.keyboard.press("ArrowLeft");
    await expect.poll(() => keptColumns(page)).toEqual(["project"]);

    const box = (await handles.nth(3).boundingBox())!;
    await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
    expect(await keptColumns(page)).toEqual(["project"]);
  });

  test("after a refresh, clicking File's handle without dragging keeps no File width", async ({
    page,
  }) => {
    await withRecentEdits(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);
    const handles = page.locator(".re-list thead th").getByRole("separator");
    await handles.first().focus();
    for (let i = 0; i < 4; i++) await page.keyboard.press("ArrowLeft");
    await expect.poll(() => keptColumns(page)).toEqual(["project"]);

    await page.getByRole("button", { name: "Reload list" }).click();
    await expect(page.locator(".re-file-row")).toHaveCount(3);
    const box = (await handles.nth(1).boundingBox())!;
    await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
    expect(await keptColumns(page)).toEqual(["project"]);
  });

  test("after a reload, clicking Last edit's handle without dragging keeps no Last edit width", async ({
    page,
  }) => {
    await withRecentEdits(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);
    const handles = page.locator(".re-list thead th").getByRole("separator");
    // Narrow File, so Last edit fills the room it leaves after a reload.
    await handles.nth(1).focus();
    for (let i = 0; i < 4; i++) await page.keyboard.press("ArrowLeft");
    await expect.poll(() => keptColumns(page)).toEqual(["file"]);

    await page.reload();
    await expect(page.locator(".re-file-row")).toHaveCount(3);
    const box = (await handles.nth(3).boundingBox())!;
    await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
    expect(await keptColumns(page)).toEqual(["file"]);
  });

  test("a phone resize is not kept once the window is wide again", async ({ page }) => {
    await withRecentEdits(page);
    await page.setViewportSize({ width: 400, height: 800 });
    await page.goto("/recent-edits");
    await expect(page.locator(".re-file-row")).toHaveCount(3);
    // Stacked: File is the first column. Narrow it by one keyboard step.
    await page.locator(".re-list thead th").first().getByRole("separator").focus();
    await page.keyboard.press("ArrowLeft");
    expect(await keptColumns(page)).toEqual([]);

    await page.setViewportSize({ width: 1280, height: 800 });
    await expect(page.locator(".re-list thead th")).toHaveCount(4);
    // Wide: Edits is the third column. Widen it by one keyboard step.
    await page.locator(".re-list thead th").nth(2).getByRole("separator").focus();
    await page.keyboard.press("ArrowRight");
    await expect.poll(() => keptColumns(page)).toEqual(["edits"]);
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
