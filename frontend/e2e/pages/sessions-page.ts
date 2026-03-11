import { expect, type Locator, type Page } from "@playwright/test";

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

  readonly analyticsPage: Locator;
  readonly analyticsToolbar: Locator;
  readonly exportBtn: Locator;

  constructor(readonly page: Page) {
    this.sessionItems = page.locator(".session-item");
    this.sessionListScroll = page.locator(".session-list-scroll");
    this.messageRows = page.locator(".virtual-row");
    this.scroller = page.locator(".message-list-scroll");
    this.sortButton = page.getByLabel("Toggle sort order");
    this.projectTypeahead = page.locator(".typeahead");
    this.sessionListHeader = page.locator(".session-list-header");
    this.analyticsPage = page.locator(".analytics-page");
    this.analyticsToolbar = page.locator(".analytics-toolbar");
    this.exportBtn = page.locator(".export-btn");
  }

  async goto() {
    await this.page.goto("/");
    await expect(this.sessionItems.first()).toBeVisible({
      timeout: 5_000,
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
    const trigger = this.projectTypeahead.locator(".typeahead-trigger");
    const input = this.projectTypeahead.locator(".typeahead-input");
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
    await this.projectTypeahead
      .locator(".typeahead-option", {
        hasText: new RegExp(`^${escaped} \\(`),
      })
      .click();
  }

  async clearProjectFilter() {
    await this.projectTypeahead.locator(".typeahead-trigger").click();
    await this.projectTypeahead
      .locator(".typeahead-option", { hasText: "All Projects" })
      .click();
  }

  async pressNextSessionShortcut() {
    await this.page.keyboard.press("]");
  }

  async pressPreviousSessionShortcut() {
    await this.page.keyboard.press("[");
  }
}
