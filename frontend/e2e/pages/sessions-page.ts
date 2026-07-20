import { expect, type Locator, type Page } from "@playwright/test";

const SESSION_READY_TIMEOUT_MS = process.env.CI === "true" ? 15_000 : 5_000;

/**
 * Page object for the sessions view.
 * Encapsulates selectors and common navigation actions
 * shared across E2E specs.
 */
export class SessionsPage {
  readonly sessionItems: Locator;
  readonly sessionListScroll: Locator;
  readonly messageRows: Locator;
  readonly scroller: Locator;

  readonly sortButton: Locator;
  readonly projectTypeahead: Locator;
  readonly sessionListHeader: Locator;
  readonly sessionCount: Locator;

  readonly analyticsPage: Locator;
  readonly analyticsToolbar: Locator;
  readonly exportBtn: Locator;

  constructor(readonly page: Page) {
    this.sessionItems = page.locator(".session-item");
    this.sessionListScroll = page.locator(".session-list-scroll");
    this.messageRows = page.locator(".virtual-row");
    this.scroller = page.locator(".message-list-scroll");
    this.sortButton = page.getByLabel("Toggle sort order");
    this.projectTypeahead = page.locator(".kit-typeahead");
    this.sessionListHeader = page.locator(".session-list-header");
    this.sessionCount = this.sessionListHeader.locator(".session-count");
    this.analyticsPage = page.locator(".analytics-page");
    this.analyticsToolbar = page.locator(".analytics-toolbar");
    this.exportBtn = page.locator(".export-btn");
  }

  async goto() {
    await this.page.goto("/");
    await expect(this.sessionItems.first()).toBeVisible({
      timeout: SESSION_READY_TIMEOUT_MS,
    });
  }

  async selectSession(index: number = 0) {
    await this.sessionItems.nth(index).click();
    await expect(this.messageRows.first()).toBeVisible({
      timeout: 3_000,
    });
  }

  async selectFirstSession() {
    await this.selectSession(0);
  }

  async selectLastSession() {
    await this.sessionItems.last().click();
    await expect(this.messageRows.first()).toBeVisible({
      timeout: 3_000,
    });
  }

  async toggleSortOrder(times: number = 1) {
    for (let i = 0; i < times; i++) {
      await this.sortButton.click();
    }
  }

  async filterByProject(project: string) {
    const trigger = this.projectTypeahead.locator(".kit-typeahead__trigger");
    const input = this.projectTypeahead.locator(".kit-typeahead__input");
    // The typeahead may close immediately if a reactive update
    // steals focus right after opening. Retry until stable.
    await expect(async () => {
      if (await trigger.isVisible()) {
        await trigger.click();
      }
      await expect(input).toBeVisible({ timeout: 1_000 });
    }).toPass({ timeout: 5_000 });
    // Re-focus the input in case blur closed and re-opened it.
    await input.click();
    await input.fill(project);
    const escaped = project.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    // kit-ui renders the option label as match-highlight segments inside
    // a label span, so the option's text carries template whitespace
    // around the name — anchor with \s* instead of exact spacing.
    await this.projectTypeahead
      .locator(".kit-typeahead__option", {
        hasText: new RegExp(`^\\s*${escaped}\\s*\\(`),
      })
      .click();
  }

  async clearProjectFilter() {
    await this.projectTypeahead.locator(".kit-typeahead__trigger").click();
    await this.projectTypeahead
      .locator(".kit-typeahead__option", { hasText: "All Projects" })
      .click();
  }

  async pressNextSessionShortcut() {
    await this.page.keyboard.press("]");
  }

  async pressPreviousSessionShortcut() {
    await this.page.keyboard.press("[");
  }
}
