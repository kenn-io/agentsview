import { expect, type Page } from "@playwright/test";

// The kit-ui TopBar collapses its tab row into a SelectDropdown by
// measurement, and the threshold is engine-dependent (WebKit's first
// layout pass measures the side regions wider than Chromium's, freezing
// the bar collapsed at widths where Chromium expands). Navigation helpers
// therefore drive whichever mode actually rendered.

/** Click a TopBar destination: the tab when expanded, or the nav
 * dropdown option when collapsed. */
export async function clickNavTab(page: Page, label: string): Promise<void> {
  const tab = page.locator(".kit-top-bar__tabs .kit-top-bar__tab", {
    hasText: label,
  });
  const trigger = page.locator(
    ".kit-top-bar__nav-select .kit-select-dropdown__trigger",
  );
  await expect(tab.or(trigger)).toBeVisible();
  if (await tab.isVisible()) {
    await tab.click();
    return;
  }
  await trigger.click();
  await page
    .locator(".kit-select-dropdown__option", { hasText: label })
    .click();
}

/** Assert `label` is the active TopBar destination in either nav mode. */
export async function expectActiveNavTab(
  page: Page,
  label: string,
): Promise<void> {
  const activeTab = page.locator(
    ".kit-top-bar__tabs .kit-top-bar__tab.active",
    { hasText: label },
  );
  const value = page.locator(
    ".kit-top-bar__nav-select .kit-select-dropdown__value",
  );
  await expect(activeTab.or(value)).toBeVisible();
  if (await value.isVisible()) {
    await expect(value).toHaveText(label);
  } else {
    await expect(activeTab).toBeVisible();
  }
}
