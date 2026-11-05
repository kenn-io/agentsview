import { expect, test, type Page, type Response } from "@playwright/test";

async function openSettledSettings(page: Page) {
  const responses: Response[] = [];
  const settingsLoaded = new Promise<void>((resolve) => {
    const onResponse = (response: Response) => {
      if (new URL(response.url()).pathname !== "/api/v1/settings") return;
      responses.push(response);
      if (responses.length < 2) return;
      page.off("response", onResponse);
      void Promise.all(responses.map((item) => item.finished())).then(() => {
        resolve();
      });
    };
    page.on("response", onResponse);
  });

  await page.goto("/settings");
  await settingsLoaded;
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
      }),
  );
  await expect(page.locator(".settings-loading")).toHaveCount(0);
}

test.describe("Settings layout", () => {
  test("keeps navigation and actions fixed while panel content scrolls", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/settings");

    const nav = page.getByRole("navigation", { name: "Settings" });
    await expect(nav).toBeVisible();

    const host = page.locator(".settings-page-host");
    await expect
      .poll(() => host.evaluate((element) => getComputedStyle(element).overflow))
      .toBe("hidden");
    await expect
      .poll(() => host.evaluate((element) => element.scrollHeight))
      .toBe(await host.evaluate((element) => element.clientHeight));

    await nav.locator("button", { hasText: "Session providers" }).click();
    await expect(page.getByRole("heading", { name: "Session providers" })).toBeVisible();

    const scroller = page.locator(".kit-settings__scroll");
    await expect
      .poll(() => scroller.evaluate((element) => element.scrollHeight > element.clientHeight))
      .toBe(true);
    await scroller.evaluate((element) => {
      element.scrollTop = element.scrollHeight;
    });
    await expect.poll(() => scroller.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
    await expect(page.getByRole("button", { name: "Full Resync" })).toBeVisible();

    await nav.locator("button", { hasText: "Terminal" }).click();
    await expect(page.getByRole("heading", { name: "Terminal" })).toBeVisible();
    await expect.poll(() => scroller.evaluate((element) => element.scrollTop)).toBe(0);
  });

  test("keeps settings operable at the narrow breakpoint", async ({ page }) => {
    await page.setViewportSize({ width: 700, height: 800 });
    await page.goto("/settings");

    const search = page.getByRole("searchbox", { name: "Search settings" });
    await expect(search).toBeVisible();

    const nav = page.getByRole("navigation", { name: "Settings" });
    await nav.locator("button", { hasText: "Terminal" }).click();
    await expect(page.getByRole("heading", { name: "Terminal" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Full Resync" })).toBeVisible();
    await expect
      .poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth))
      .toBe(true);
  });

  test("stacks each label above its control on a phone", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 800 });
    await openSettledSettings(page);

    const nav = page.getByRole("navigation", { name: "Settings" });
    await nav.locator("button", { hasText: "Appearance" }).click();
    await expect(page.getByRole("heading", { name: "Appearance" })).toBeVisible();

    const rows = await page
      .locator(".kit-settings__panel .setting-row.option-row")
      .evaluateAll((elements) =>
        elements.map((row) => {
          const label = row.querySelector(".setting-label")!.getBoundingClientRect();
          const control = row.querySelector(".kit-segmented")!.getBoundingClientRect();
          const rowRect = row.getBoundingClientRect();
          const buttons = [...row.querySelectorAll<HTMLElement>(".kit-segmented__btn")];
          return {
            labelAbove: label.bottom <= control.top + 1,
            controlFillsRow: control.width >= rowRect.width - 1,
            controlInsideRow: control.right <= rowRect.right + 1,
            buttonTextFits: buttons.every((button) => button.scrollWidth <= button.clientWidth + 1),
          };
        }),
      );
    expect(rows.length).toBeGreaterThan(0);
    for (const row of rows) {
      expect(row).toEqual({
        labelAbove: true,
        controlFillsRow: true,
        controlInsideRow: true,
        buttonTextFits: true,
      });
    }
  });

  test("starts every panel's controls on one column at one height", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 800 });
    await openSettledSettings(page);
    const nav = page.getByRole("navigation", { name: "Settings" });
    const panel = page.locator(".settings-panel:not([hidden])");

    async function openPanel(name: string) {
      await nav.locator("button", { hasText: name }).click();
      await expect(page.getByRole("heading", { name, exact: true })).toBeVisible();
    }
    const leftOf = (selector: string) =>
      panel.locator(selector).evaluateAll((elements) =>
        elements.map((element) => Math.round(element.getBoundingClientRect().left)),
      );
    // Row controls only: the per-role color pickers are a dense sub-list.
    const rowControlHeights = () =>
      panel
        .locator(
          ".kit-button, .kit-text-input, .kit-segmented:not(.kit-segmented--borderless), .kit-typeahead__trigger",
        )
        .evaluateAll((elements) =>
          elements
            .filter((element) => !element.closest(".role-colors"))
            .map((element) => Math.round(element.getBoundingClientRect().height)),
        );

    await openPanel("Appearance");
    const [column] = await leftOf(".setting-row.option-row .kit-segmented");
    expect(column).toBeGreaterThan(0);
    const appearanceHeights = await rowControlHeights();
    expect(appearanceHeights.length).toBeGreaterThan(0);
    expect(new Set(appearanceHeights)).toEqual(new Set([28]));

    await openPanel("Session providers");
    const switches = await leftOf(".provider-row > .kit-toggle");
    expect(switches.length).toBeGreaterThan(0);
    expect(new Set(switches)).toEqual(new Set([column]));
    const nameOnSwitchLine = await panel.locator(".provider-row").evaluateAll((rows) =>
      rows.map((row) => {
        const name = row.querySelector(".provider-name")!.getBoundingClientRect();
        const track = row.querySelector(".kit-toggle__track")!.getBoundingClientRect();
        return Math.abs(name.top + name.height / 2 - (track.top + track.height / 2)) <= 1;
      }),
    );
    expect(new Set(nameOnSwitchLine)).toEqual(new Set([true]));
    expect(new Set(await rowControlHeights())).toEqual(new Set([28]));

    await openPanel("Remote access");
    expect(await leftOf(".setting-row > .kit-toggle")).toEqual([column]);
    expect(await leftOf(".setting-row > .kit-text-input")).toEqual([column, column]);
    expect(await leftOf(".actions .kit-button:first-child")).toEqual([column]);
    expect(new Set(await rowControlHeights())).toEqual(new Set([28]));

    await openPanel("Tool-result images");
    expect(await leftOf(".setting-row > .kit-text-input")).toEqual([column, column]);
    expect(await leftOf(".action-row .kit-button")).toEqual([column]);
    expect(new Set(await rowControlHeights())).toEqual(new Set([28]));
  });

  test("wraps a long translated label inside the label column", async ({ page }) => {
    await page.addInitScript(() => localStorage.setItem("agentsview-locale", "es"));
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/settings");

    const nav = page.getByRole("navigation", { name: "Configuración" });
    await nav.locator("button", { hasText: "Contenido del archivo" }).click();
    await expect(page.getByRole("heading", { name: "Contenido del archivo" })).toBeVisible();

    const row = page.locator(".archive-content-settings .setting-row.option-row");
    await expect(row.locator(".setting-label")).toHaveText("Imágenes de resultados de herramienta");
    const layout = await row.evaluate((element) => {
      const label = element.querySelector<HTMLElement>(".setting-label")!;
      const control = element.querySelector(".kit-segmented")!.getBoundingClientRect();
      const labelRect = label.getBoundingClientRect();
      return {
        clearOfControl: labelRect.right <= control.left,
        wrapped: labelRect.height > parseFloat(getComputedStyle(label).lineHeight) * 1.5,
        notClipped: label.scrollWidth <= label.clientWidth + 1,
      };
    });
    expect(layout).toEqual({ clearOfControl: true, wrapped: true, notClipped: true });
  });

  test("keeps the horizontal nav strip free of desktop bottom padding", async ({ page }) => {
    await page.setViewportSize({ width: 700, height: 800 });
    await page.goto("/settings");
    const nav = page.locator(".kit-settings__nav");
    await expect(nav).toBeVisible();
    await expect.poll(() => nav.evaluate((el) => getComputedStyle(el).paddingBottom)).not.toBe("4px");

    await page.setViewportSize({ width: 1280, height: 800 });
    await expect.poll(() => nav.evaluate((el) => getComputedStyle(el).paddingBottom)).toBe("4px");
  });

  test("hides an unmatched panel without discarding its draft", async ({ page }) => {
    // App and SettingsPage both start an initial settings read. Wait for both
    // before editing so a late response cannot reset the unsaved draft.
    await openSettledSettings(page);

    const nav = page.getByRole("navigation", { name: "Settings" });
    await nav.locator("button", { hasText: "Terminal" }).click();
    await page.getByRole("radio", { name: "Custom", exact: true }).click();

    const binary = page.getByLabel("Terminal binary");
    await binary.fill("/usr/bin/kitty");

    const search = page.getByRole("searchbox", { name: "Search settings" });
    await search.fill("no such setting");

    await expect(page.getByText("No matching settings", { exact: true })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Terminal" })).toHaveCount(0);
    await expect(page.locator("#terminal-bin")).toBeHidden();

    await search.fill("");

    await expect(page.getByRole("heading", { name: "Terminal" })).toBeVisible();
    await expect(binary).toHaveValue("/usr/bin/kitty");
  });
});

// One provider with a path segment and an alternate home far wider than
// the settings column, so nothing can fit by luck.
async function withLongProviderPaths(page: Page) {
  await page.route("**/api/v1/settings", async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    const response = await route.fetch();
    const body = await response.json();
    const [first, ...rest] = body.session_providers ?? [];
    await route.fulfill({
      response,
      json: {
        ...body,
        session_providers: [
          {
            ...first,
            dirs: ["/Users/someone/Library/Application Support/AnExtremelyLongApplicationDirectoryNameWithNoSeparatorsAtAll/sessions"],
            homes_supported: true,
            homes: ["/Volumes/ExternalDrive/AnotherExtremelyLongHomeDirectoryNameWithNoSeparatorsAnywhere/.agent"],
          },
          ...rest,
        ],
      },
    });
  });
}

for (const width of [1280, 375]) {
  test(`Session providers keep long paths and their controls inside the panel at ${width}px`, async ({ page }) => {
    await withLongProviderPaths(page);
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/settings");
    const nav = page.locator(".kit-settings__nav");
    await nav.getByText("Session providers").first().click();
    const row = page.locator(".provider-row").first();
    await expect(row.locator(".provider-home")).toHaveCount(1);

    const layout = await row.evaluate((element) => {
      const panel = element.closest(".kit-settings__panel")!.getBoundingClientRect();
      const details = element.querySelector(".provider-details")!.getBoundingClientRect();
      const paths = [...element.querySelectorAll<HTMLElement>(".provider-path")].map((path) => {
        const r = path.getBoundingClientRect();
        const segs = [...path.querySelectorAll(".provider-path-seg")].map((s) => s.getBoundingClientRect().right);
        return { right: Math.max(r.right, ...segs), overflow: path.scrollWidth > path.clientWidth + 1 };
      });
      const home = element.querySelector(".provider-home")!;
      const homePath = home.querySelector(".provider-path")!;
      const homeSegRight = Math.max(...[...homePath.querySelectorAll(".provider-path-seg")].map((s) => s.getBoundingClientRect().right));
      const remove = home.querySelector("button")!.getBoundingClientRect();
      const scroller = element.closest(".kit-settings__scroll") as HTMLElement | null;
      return {
        panelRight: panel.right,
        detailsWidth: details.width,
        detailsRight: details.right,
        paths,
        homeSegRight,
        removeLeft: remove.left,
        removeRight: remove.right,
        scrollerOverflow: scroller ? scroller.scrollWidth > scroller.clientWidth + 1 : false,
      };
    });
    expect(layout.detailsRight).toBeLessThanOrEqual(layout.panelRight + 0.5);
    for (const path of layout.paths) {
      expect(path.overflow, JSON.stringify(layout)).toBe(false);
      expect(path.right).toBeLessThanOrEqual(layout.detailsRight + 0.5);
    }
    expect(layout.homeSegRight, JSON.stringify(layout)).toBeLessThanOrEqual(layout.removeLeft + 0.5);
    expect(layout.removeRight).toBeLessThanOrEqual(layout.panelRight + 0.5);
    expect(layout.scrollerOverflow).toBe(false);
    if (width < 640) {
      // Phones give the details the row's full width.
      expect(layout.detailsWidth).toBeGreaterThanOrEqual(250);
    }
    const add = await row.locator(".provider-home-add").evaluate((element) => {
      const input = element.querySelector(".kit-text-input")!.getBoundingClientRect();
      const button = element.querySelector("button")!.getBoundingClientRect();
      return { inputRight: input.right, inputBottom: input.bottom, buttonLeft: button.left, buttonTop: button.top, buttonRight: button.right };
    });
    expect(add.buttonRight, JSON.stringify(add)).toBeLessThanOrEqual(layout.panelRight + 0.5);
    expect(add.inputRight <= add.buttonLeft + 0.5 || add.inputBottom <= add.buttonTop + 0.5).toBe(true);
  });
}

for (const [width, locale] of [[761, "fr"], [375, "en"]] as const) {
  test(`the GitHub token field and its action fit the panel at ${width}px in ${locale}`, async ({ page }) => {
    await page.addInitScript((value) => localStorage.setItem("agentsview-locale", value), locale);
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/settings");
    await page.locator(".kit-settings__nav").getByText("GitHub", { exact: true }).first().click();
    const row = page.locator(".token-row");
    await expect(row).toBeVisible();
    const layout = await row.evaluate((element) => {
      const panel = element.closest(".kit-settings__panel")!.getBoundingClientRect();
      const input = element.querySelector(".kit-text-input")!.getBoundingClientRect();
      const button = element.querySelector("button")!;
      const buttonRect = button.getBoundingClientRect();
      return {
        panelRight: panel.right,
        inputWidth: input.width,
        inputRight: input.right,
        buttonLeft: buttonRect.left,
        buttonRight: buttonRect.right,
        buttonTop: buttonRect.top,
        inputBottom: input.bottom,
        labelClipped: button.scrollWidth > button.clientWidth + 1,
      };
    });
    expect(layout.buttonRight, JSON.stringify(layout)).toBeLessThanOrEqual(layout.panelRight + 0.5);
    expect(layout.labelClipped).toBe(false);
    expect(layout.inputWidth, JSON.stringify(layout)).toBeGreaterThanOrEqual(160);
    // Side by side or stacked, never overlapping.
    expect(layout.inputRight <= layout.buttonLeft + 0.5 || layout.inputBottom <= layout.buttonTop + 0.5).toBe(true);
  });
}

test("Embeddings status values stay inside the panel on a phone", async ({ page }) => {
  await page.route("**/api/v1/embeddings/generations*", (route) => route.fulfill({ json: { generations: [] } }));
  await page.route("**/api/v1/embeddings/status*", (route) =>
    route.fulfill({
      json: { running: false, done: 0, total: 0, eta_milliseconds: 0, model: "embeddinggemma-300m-qat-q8_0", dimension: 768 },
    }),
  );
  await page.setViewportSize({ width: 320, height: 800 });
  await page.goto("/settings");
  await page.locator(".kit-settings__nav").getByText("Embeddings", { exact: true }).first().click();
  const grid = page.locator(".status-grid").first();
  await expect(grid).toBeVisible();
  const layout = await grid.evaluate((element) => {
    const panel = element.closest(".kit-settings__panel")!.getBoundingClientRect();
    return [...element.querySelectorAll<HTMLElement>(".row-value")].map((value) => ({
      right: value.getBoundingClientRect().right,
      overflow: value.scrollWidth > value.clientWidth + 1,
      panelRight: panel.right,
    }));
  });
  expect(layout.length).toBeGreaterThan(1);
  for (const value of layout) {
    expect(value.overflow, JSON.stringify(value)).toBe(false);
    expect(value.right, JSON.stringify(value)).toBeLessThanOrEqual(value.panelRight + 0.5);
  }
});
