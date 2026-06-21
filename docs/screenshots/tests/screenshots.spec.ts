import { test, expect, Page, Locator } from '@playwright/test';
import { join } from 'path';

const DIR = process.env.SCREENSHOT_DIR || join(
  __dirname, '..', '..', 'assets', 'generated', 'screenshots'
);

const FULL = { width: 1440, height: 900 };

// PG serve instance for pg-sync screenshots (machine labels, etc.)
const PG_BASE_URL = process.env.PG_BASE_URL || '';

async function snap(page: Page, name: string) {
  await page.screenshot({
    path: join(DIR, `${name}.png`),
    type: 'png',
  });
}

async function snapEl(loc: Locator, name: string) {
  await loc.screenshot({
    path: join(DIR, `${name}.png`),
    type: 'png',
  });
}

async function waitForApp(page: Page) {
  await page.goto('/');
  await page.waitForSelector('.session-item', {
    timeout: 15_000,
  });
  // Let analytics charts render
  await page.waitForTimeout(3000);
}

async function setDateRange1Y(page: Page) {
  const btn = page.locator('.preset-btn', { hasText: '1y' });
  if (await btn.count() > 0) {
    await btn.click();
    await page.waitForTimeout(3000);
  }
}

async function selectFirstSession(page: Page) {
  const items = page.locator('.session-item');
  await items.first().click();
  await page.waitForSelector('.message', { timeout: 10_000 });
  await page.waitForTimeout(1000);
}

// Find a session likely to have thinking/tool content
// by looking for ones with higher message counts
async function selectRichSession(page: Page) {
  const items = page.locator('.session-item');
  const count = await items.count();
  // Try the 3rd session (index 2) for variety, fall back to first
  const idx = Math.min(2, count - 1);
  await items.nth(idx).click();
  await page.waitForSelector('.message', { timeout: 10_000 });
  await page.waitForTimeout(1000);
}

// ── Dashboard / Analytics ───────────────────────────────

test.describe('Dashboard', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
    await setDateRange1Y(page);
  });

  test('full dashboard', async ({ page }) => {
    await snap(page, 'dashboard');
  });

  test('summary cards', async ({ page }) => {
    const el = page.locator('.summary-cards');
    if (await el.count() > 0) {
      await snapEl(el, 'summary-cards');
    }
  });

  test('date range and toolbar', async ({ page }) => {
    const el = page.locator('.analytics-toolbar');
    if (await el.count() > 0) {
      await snapEl(el, 'date-range');
    }
  });

  test('activity heatmap', async ({ page }) => {
    const heatmap = page.locator('.heatmap-container');
    if (await heatmap.count() > 0) {
      // Wait for SVG cells to render
      await page.waitForSelector('.heatmap-cell', {
        timeout: 10_000,
      });
      await page.waitForTimeout(500);
      await snapEl(heatmap, 'heatmap');
    }
  });

  test('heatmap click-to-filter', async ({ page }) => {
    // Click a cell with data (has .clickable class)
    const clickable = page.locator('.heatmap-cell.clickable');
    if (await clickable.count() > 0) {
      await clickable.first().click();
      await page.waitForTimeout(2000);
      await snap(page, 'heatmap-filtered');
      // Click again to deselect
      const selected = page.locator('.heatmap-cell.selected');
      if (await selected.count() > 0) {
        await selected.first().click();
        await page.waitForTimeout(500);
      }
    }
  });

  test('hour of week heatmap', async ({ page }) => {
    const panels = page.locator('.chart-panel');
    const count = await panels.count();
    for (let i = 0; i < count; i++) {
      const text = await panels.nth(i).textContent();
      if (text && (text.includes('Hour') || text.includes('Week'))) {
        await snapEl(panels.nth(i), 'hour-of-week');
        break;
      }
    }
  });

  test('activity timeline', async ({ page }) => {
    const timeline = page.locator('.timeline-container');
    if (await timeline.count() > 0) {
      await timeline.scrollIntoViewIfNeeded();
      await page.waitForSelector('.timeline-svg', {
        timeout: 10_000,
      });
      await page.waitForTimeout(500);
      // Capture the parent chart-panel that wraps the timeline
      const panel = page.locator(
        '.chart-panel:has(.timeline-container)'
      );
      if (await panel.count() > 0) {
        await snapEl(panel, 'activity-timeline');
      }
    }
  });

  test('top sessions', async ({ page }) => {
    // Scroll down to find top sessions
    const content = page.locator('.analytics-content');
    await content.evaluate(
      (el) => el.scrollTo(0, el.scrollHeight / 3)
    );
    await page.waitForTimeout(500);

    const panels = page.locator('.chart-panel');
    const count = await panels.count();
    for (let i = 0; i < count; i++) {
      const text = await panels.nth(i).textContent();
      if (text && text.includes('Top')) {
        await panels.nth(i).scrollIntoViewIfNeeded();
        await page.waitForTimeout(300);
        await snapEl(panels.nth(i), 'top-sessions');
        break;
      }
    }
  });

  test('project breakdown', async ({ page }) => {
    const content = page.locator('.analytics-content');
    await content.evaluate(
      (el) => el.scrollTo(0, el.scrollHeight / 3)
    );
    await page.waitForTimeout(500);

    const panels = page.locator('.chart-panel');
    const count = await panels.count();
    for (let i = 0; i < count; i++) {
      const text = await panels.nth(i).textContent();
      if (text && text.includes('Project')) {
        await panels.nth(i).scrollIntoViewIfNeeded();
        await page.waitForTimeout(300);
        await snapEl(panels.nth(i), 'project-breakdown');
        break;
      }
    }
  });

  test('session shape', async ({ page }) => {
    const content = page.locator('.analytics-content');
    await content.evaluate(
      (el) => el.scrollTo(0, el.scrollHeight / 2)
    );
    await page.waitForTimeout(500);

    const panels = page.locator('.chart-panel');
    const count = await panels.count();
    for (let i = 0; i < count; i++) {
      const text = await panels.nth(i).textContent();
      if (text && (text.includes('Shape') || text.includes('Distribution'))) {
        await panels.nth(i).scrollIntoViewIfNeeded();
        await page.waitForTimeout(300);
        await snapEl(panels.nth(i), 'session-shape');
        break;
      }
    }
  });

  test('tool usage', async ({ page }) => {
    const content = page.locator('.analytics-content');
    await content.evaluate(
      (el) => el.scrollTo(0, (el.scrollHeight * 2) / 3)
    );
    await page.waitForTimeout(500);

    const panels = page.locator('.chart-panel');
    const count = await panels.count();
    for (let i = 0; i < count; i++) {
      const text = await panels.nth(i).textContent();
      if (text && text.includes('Tool')) {
        await panels.nth(i).scrollIntoViewIfNeeded();
        await page.waitForTimeout(300);
        await snapEl(panels.nth(i), 'tool-usage');
        break;
      }
    }
  });

  test('top skills', async ({ page }) => {
    const panel = page.locator('.chart-panel:has(.skills-container)');
    await expect(panel).toBeVisible({ timeout: 10_000 });
    await panel.scrollIntoViewIfNeeded();
    await page.waitForTimeout(500);
    await snapEl(panel, 'top-skills');
  });

  test('velocity metrics', async ({ page }) => {
    const content = page.locator('.analytics-content');
    await content.evaluate(
      (el) => el.scrollTo(0, (el.scrollHeight * 3) / 4)
    );
    await page.waitForTimeout(500);

    const panels = page.locator('.chart-panel');
    const count = await panels.count();
    for (let i = 0; i < count; i++) {
      const text = await panels.nth(i).textContent();
      if (text && text.includes('Velocity')) {
        await panels.nth(i).scrollIntoViewIfNeeded();
        await page.waitForTimeout(300);
        await snapEl(panels.nth(i), 'velocity');
        break;
      }
    }
  });

  test('agent comparison', async ({ page }) => {
    const content = page.locator('.analytics-content');
    await content.evaluate(
      (el) => el.scrollTo(0, el.scrollHeight)
    );
    await page.waitForTimeout(500);

    const panels = page.locator('.chart-panel');
    const count = await panels.count();
    for (let i = 0; i < count; i++) {
      const text = await panels.nth(i).textContent();
      if (text && text.includes('Comparison')) {
        await panels.nth(i).scrollIntoViewIfNeeded();
        await page.waitForTimeout(300);
        await snapEl(panels.nth(i), 'agent-comparison');
        break;
      }
    }
  });
});

// ── Activity dashboard ───────────────────────────────────

test.describe('Activity dashboard', () => {
  async function navigateToActivity(page: Page, path = '/activity') {
    await page.goto(path);
    await page.waitForSelector('.activity-page', { timeout: 10_000 });
    await expect(
      page.locator('.activity-page .summary-cards .card').first()
    ).toBeVisible({ timeout: 15_000 });
    await expect(
      page.locator('.activity-page .timeline-svg')
    ).toBeVisible({ timeout: 15_000 });
    await page.waitForTimeout(2000);
  }

  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('daily activity page', async ({ page }) => {
    await navigateToActivity(page);
    await snap(page, 'activity-page');
  });

  test('weekly activity page', async ({ page }) => {
    await navigateToActivity(page, '/activity?preset=week');
    await snap(page, 'activity-week');
  });

  test('activity concurrency chart', async ({ page }) => {
    await navigateToActivity(page, '/activity?preset=week');
    const panel = page.locator('.activity-page .chart-panel:has(.timeline)');
    await expect(panel).toBeVisible({ timeout: 5_000 });
    await panel.scrollIntoViewIfNeeded();
    await page.waitForTimeout(500);
    await snapEl(panel, 'activity-concurrency');
  });

  test('activity sessions table', async ({ page }) => {
    await navigateToActivity(page, '/activity?preset=week');
    const panel = page.locator(
      '.activity-page .chart-panel:has(.sessions-table)'
    );
    await expect(panel).toBeVisible({ timeout: 5_000 });
    await panel.scrollIntoViewIfNeeded();
    await page.waitForTimeout(500);
    await snapEl(panel, 'activity-sessions');
  });

  test('activity breakdowns', async ({ page }) => {
    await navigateToActivity(page, '/activity?preset=week');
    const panel = page.locator('.activity-page .chart-panel:has(.breakdowns)');
    await expect(panel).toBeVisible({ timeout: 5_000 });
    await panel.scrollIntoViewIfNeeded();
    await page.waitForTimeout(500);
    await snapEl(panel, 'activity-breakdowns');
  });

  test('activity insight', async ({ page }) => {
    await navigateToActivity(page, '/activity?preset=week');
    const panel = page.locator(
      '.activity-page .chart-panel:has(.activity-insight)'
    );
    await expect(panel).toBeVisible({ timeout: 10_000 });
    await panel.scrollIntoViewIfNeeded();
    await page.waitForTimeout(500);
    await snapEl(panel, 'activity-insight');
  });
});

// ── Session browser ─────────────────────────────────────

test.describe('Session browser', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('session list', async ({ page }) => {
    const sidebar = page.locator('.sidebar');
    if (await sidebar.count() > 0) {
      await snapEl(sidebar, 'session-list');
    }
  });

  test('project filter', async ({ page }) => {
    // Select a project from the dropdown
    const select = page.locator('.project-select');
    if (await select.count() > 0) {
      await select.selectOption({ index: 1 });
      await page.waitForTimeout(1000);
      await snap(page, 'session-filtered');
    }
  });

  test('session filter dropdown', async ({ page }) => {
    const filterBtn = page.locator('button.filter-btn');
    if (await filterBtn.count() > 0) {
      await filterBtn.click();
      await page.waitForTimeout(300);
      await snap(page, 'session-filters');
    }
  });

  test('session filters active', async ({ page }) => {
    const filterBtn = page.locator('button.filter-btn');
    if (await filterBtn.count() > 0) {
      await filterBtn.click();
      await page.waitForTimeout(300);

      // Select "Min Prompts 10" filter
      const minPrompts = page.getByRole('button', {
        name: '10',
        exact: true,
      });
      if (await minPrompts.count() > 0) {
        await minPrompts.click();
        await page.waitForTimeout(300);
      }

      // Select an agent filter
      const agentBtn = page.locator(
        'button.agent-filter-btn'
      ).first();
      if (await agentBtn.count() > 0) {
        await agentBtn.click();
        await page.waitForTimeout(300);
      }

      await snap(page, 'session-filters-active');

      // Clean up
      const clear = page.locator('button.clear-filters-btn');
      if (await clear.count() > 0) await clear.click();
    }
  });

  test('starred session', async ({ page }) => {
    // Star the first session using 's' key
    await selectFirstSession(page);
    await page.keyboard.press('s');
    await page.waitForTimeout(500);

    const sidebar = page.locator('.sidebar');
    if (await sidebar.count() > 0) {
      await snapEl(sidebar, 'starred-session');
    }

    // Unstar to clean up
    await page.keyboard.press('s');
  });

  test('group by agent', async ({ page }) => {
    // Find and click the group-by-agent toggle
    const groupBtn = page.locator(
      'button[title*="group"], button[title*="Group"]'
    );
    if (await groupBtn.count() > 0) {
      await groupBtn.click();
      await page.waitForTimeout(500);

      // Expand the first group
      const groupHeader = page.locator(
        '.agent-group-header'
      ).first();
      if (await groupHeader.count() > 0) {
        await groupHeader.click();
        await page.waitForTimeout(300);
      }

      const sidebar = page.locator('.sidebar');
      await snapEl(sidebar, 'group-by-agent');

      // Toggle off to clean up
      await groupBtn.click();
    }
  });
});

// ── Message viewer ──────────────────────────────────────

test.describe('Message viewer', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('full message view', async ({ page }) => {
    await selectRichSession(page);
    await snap(page, 'message-viewer');
  });

  test('thinking blocks', async ({ page }) => {
    await selectRichSession(page);
    // showThinking defaults to true — don't press 't'
    await page.waitForTimeout(500);

    // Look for a thinking block already visible
    const thinking = page.locator('.thinking-block').first();
    if (await thinking.isVisible()) {
      await snapEl(thinking, 'thinking-blocks');
    } else {
      // Scroll through messages to find one
      const rows = page.locator('.message, .virtual-row');
      const count = await rows.count();
      for (let i = 0; i < Math.min(count, 40); i++) {
        await rows.nth(i).scrollIntoViewIfNeeded();
        await page.waitForTimeout(150);
        const tb = page.locator('.thinking-block').first();
        if (await tb.isVisible()) {
          await snapEl(tb, 'thinking-blocks');
          break;
        }
      }
    }
  });

  test('tool blocks', async ({ page }) => {
    await selectRichSession(page);

    const tool = page.locator('.tool-block').first();
    if (await tool.isVisible()) {
      // Expand it
      const header = tool.locator('.tool-header');
      if (await header.count() > 0) {
        await header.click();
        await page.waitForTimeout(300);
      }
      await snapEl(tool, 'tool-blocks');
    } else {
      // Scroll to find one
      const messages = page.locator('.message');
      const count = await messages.count();
      for (let i = 0; i < Math.min(count, 20); i++) {
        await messages.nth(i).scrollIntoViewIfNeeded();
        await page.waitForTimeout(200);
        const tb = page.locator('.tool-block').first();
        if (await tb.isVisible()) {
          const hdr = tb.locator('.tool-header');
          if (await hdr.count() > 0) await hdr.click();
          await page.waitForTimeout(300);
          await snapEl(tb, 'tool-blocks');
          break;
        }
      }
    }
  });

  test('tool call groups', async ({ page }) => {
    await selectRichSession(page);

    const group = page.locator('.tool-group').first();
    if (await group.isVisible()) {
      await snapEl(group, 'tool-groups');
    } else {
      const messages = page.locator('.virtual-row');
      const count = await messages.count();
      for (let i = 0; i < Math.min(count, 30); i++) {
        await messages.nth(i).scrollIntoViewIfNeeded();
        await page.waitForTimeout(150);
        const tg = page.locator('.tool-group').first();
        if (await tg.isVisible()) {
          await snapEl(tg, 'tool-groups');
          break;
        }
      }
    }
  });

  test('compact layout', async ({ page }) => {
    await selectRichSession(page);

    // Press 'l' to cycle to compact layout
    await page.keyboard.press('l');
    await page.waitForTimeout(500);

    // Verify we're in compact layout
    const list = page.locator('.layout-compact');
    if (await list.count() > 0) {
      await snap(page, 'layout-compact');
    }

    // Cycle back to default
    await page.keyboard.press('l');
    await page.keyboard.press('l');
  });

  test('stream layout', async ({ page }) => {
    await selectRichSession(page);

    // Press 'l' twice to reach stream layout
    await page.keyboard.press('l');
    await page.keyboard.press('l');
    await page.waitForTimeout(500);

    const list = page.locator('.layout-stream');
    if (await list.count() > 0) {
      await snap(page, 'layout-stream');
    }

    // Cycle back to default
    await page.keyboard.press('l');
  });

  test('block-type filter dropdown', async ({ page }) => {
    await selectRichSession(page);

    // Click the block-type filter button
    const filterBtn = page.locator(
      'button[title="Filter block types"]'
    );
    if (await filterBtn.count() > 0) {
      await filterBtn.click();
      await page.waitForTimeout(300);

      const dropdown = page.locator('.block-filter-dropdown');
      if (await dropdown.count() > 0) {
        await snapEl(dropdown, 'block-filter');
      }

      // Close by clicking elsewhere
      await page.keyboard.press('Escape');
    }
  });

  test('copy button on message', async ({ page }) => {
    await selectRichSession(page);

    // Hover over a message to reveal the copy button
    const message = page.locator('.message').first();
    if (await message.count() > 0) {
      await message.hover();
      await page.waitForTimeout(300);

      const copyBtn = message.locator('.copy-btn');
      if (await copyBtn.count() > 0) {
        await snapEl(message, 'message-copy-btn');
      }
    }
  });

  test('copy button on code block', async ({ page }) => {
    // Walk sessions until we find one with at least one fenced
    // code block. We scan more aggressively than selectRichSession
    // because most sessions have only inline code or no code at
    // all, and we need a `.code-block` (CodeBlock.svelte:34) for
    // this test to be meaningful.
    const items = page.locator('.session-item');
    const total = await items.count();
    const max = Math.min(40, total);
    let found = false;
    for (let i = 0; i < max; i++) {
      await items.nth(i).click();
      await page.waitForSelector('.message', { timeout: 10_000 });
      await page.waitForTimeout(400);
      if (await page.locator('.code-block').first().count() > 0) {
        found = true;
        break;
      }
    }
    if (!found) {
      test.skip(true,
        `No .code-block found in the first ${max} sessions`);
    }

    // Capture just the top of a code block where the copy
    // button lives (CopyButton is absolutely positioned at
    // top:6px, right:6px inside .code-block). We clip the
    // page screenshot to a small region around that corner
    // so multi-page diffs don't produce a 4000px tall image.
    const codeBlock = page.locator('.code-block').first();
    await codeBlock.evaluate((el) => {
      el.scrollIntoView({ block: 'start' });
    });
    await page.waitForTimeout(200);
    await codeBlock.hover();
    // Hover transition: .code-copy fades to opacity:1; give
    // the transition time to settle before capturing.
    await page.waitForTimeout(600);

    const box = await codeBlock.boundingBox();
    if (!box) {
      throw new Error('code-block has no bounding box');
    }
    await page.screenshot({
      path: join(DIR, 'code-block-copy-btn.png'),
      type: 'png',
      clip: {
        x: Math.max(0, box.x),
        y: Math.max(0, box.y),
        width: Math.min(box.width, 1440 - box.x),
        height: Math.min(box.height, 140),
      },
    });
  });
});

// ── Command palette & search ────────────────────────────

test.describe('Command palette', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('recent sessions', async ({ page }) => {
    await page.keyboard.press('Control+k');
    await page.waitForSelector('.palette-overlay', {
      timeout: 5_000,
    });
    await page.waitForTimeout(500);
    await snap(page, 'command-palette');
  });

  test('search results', async ({ page }) => {
    await page.keyboard.press('Control+k');
    await page.waitForSelector('.palette-overlay', {
      timeout: 5_000,
    });

    const input = page.locator('.palette-input');
    await input.fill('implement');
    await page.waitForTimeout(1500);
    await snap(page, 'search-results');
  });
});

// ── Modals ──────────────────────────────────────────────

test.describe('Modals', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('shortcuts modal', async ({ page }) => {
    await page.keyboard.press('?');
    await page.waitForSelector('.shortcuts-overlay', {
      timeout: 5_000,
    });
    await page.waitForTimeout(300);
    await snap(page, 'shortcuts-modal');
  });

  test('resync modal', async ({ page }) => {
    const gear = page.locator('button[title="Full resync"]');
    if (await gear.count() > 0) {
      await gear.click();
      await page.waitForSelector('.resync-panel', {
        timeout: 5_000,
      });
      await page.waitForTimeout(300);
      await snap(page, 'resync-modal');
      await page.keyboard.press('Escape');
    }
  });

  test('publish modal', async ({ page }) => {
    await selectFirstSession(page);
    await page.keyboard.press('p');
    await page.waitForTimeout(500);

    const modal = page.locator(
      '.publish-overlay, .publish-modal'
    );
    if (await modal.count() > 0) {
      await snap(page, 'publish-modal');
    }
    await page.keyboard.press('Escape');
  });
});

// ── Insights ────────────────────────────────────────────

test.describe('Insights', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  async function navigateToInsights(page: Page) {
    // Insights lives under the More dropdown as of 0.21.0
    const moreBtn = page.locator('.nav-btn', { hasText: 'More' });
    await expect(moreBtn).toBeVisible({ timeout: 5_000 });
    await moreBtn.click();
    const insightsItem = page.locator(
      '.more-item', { hasText: 'Insights' }
    );
    await expect(insightsItem).toBeVisible({ timeout: 5_000 });
    await insightsItem.click();
    await page.waitForSelector('.insights-page', {
      timeout: 10_000,
    });
    await page.waitForTimeout(1000);
  }

  test('full insights page', async ({ page }) => {
    await navigateToInsights(page);

    // Select the first completed insight (weekly analysis)
    const rows = page.locator('.insight-row');
    if (await rows.count() > 0) {
      await rows.first().click();
      await page.waitForTimeout(500);
    }
    await snap(page, 'insights');
  });

  test('insight content', async ({ page }) => {
    await navigateToInsights(page);

    // Select the first insight to show content
    const rows = page.locator('.insight-row');
    if (await rows.count() > 0) {
      await rows.first().click();
      await page.waitForTimeout(500);
    }

    const content = page.locator('.content-panel');
    if (await content.count() > 0) {
      await snapEl(content, 'insight-content');
    }
  });
});

// ── Themes ──────────────────────────────────────────────

test.describe('Themes', () => {
  test('dark theme session view', async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
    await selectRichSession(page);

    // Ensure dark mode
    const isDark = await page.evaluate(
      () => document.documentElement.classList.contains('dark')
    );
    if (!isDark) {
      // Find and click theme toggle
      const btns = page.locator('.header-btn');
      const count = await btns.count();
      for (let i = 0; i < count; i++) {
        const title = await btns.nth(i).getAttribute('title');
        const aria = await btns.nth(i).getAttribute('aria-label');
        const text = (title || '') + (aria || '');
        if (text.toLowerCase().includes('theme')) {
          await btns.nth(i).click();
          await page.waitForTimeout(500);
          break;
        }
      }
    }
    await snap(page, 'theme-dark');
  });

  test('light theme session view', async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
    await selectRichSession(page);

    // Switch to light mode
    const isDark = await page.evaluate(
      () => document.documentElement.classList.contains('dark')
    );
    if (isDark) {
      const btns = page.locator('.header-btn');
      const count = await btns.count();
      for (let i = 0; i < count; i++) {
        const title = await btns.nth(i).getAttribute('title');
        const aria = await btns.nth(i).getAttribute('aria-label');
        const text = (title || '') + (aria || '');
        if (text.toLowerCase().includes('theme')) {
          await btns.nth(i).click();
          await page.waitForTimeout(500);
          break;
        }
      }
    }
    await snap(page, 'theme-light');
  });
});

// ── Settings page ────────────────────────────────────────

test.describe('Settings', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  async function openSettings(page: Page) {
    const settingsBtn = page.locator(
      'button[title*="Settings"], button[title*="settings"], ' +
      'a[href*="settings"], .nav-btn:has-text("Settings")'
    );
    await expect(settingsBtn.first()).toBeVisible({
      timeout: 5_000,
    });
    await settingsBtn.first().click();
    const settingsPage = page.locator(
      '.settings-page, .settings-container'
    );
    await expect(settingsPage).toBeVisible({ timeout: 5_000 });
    await page.waitForTimeout(500);
  }

  test('settings page', async ({ page }) => {
    await openSettings(page);
    await snap(page, 'settings');
  });

  test('settings remote access section', async ({ page }) => {
    await openSettings(page);

    // Find the settings-section that contains "Remote Access"
    const remoteSection = page.locator(
      '.settings-section:has(.section-title:text("Remote Access"))'
    );
    await expect(remoteSection).toBeVisible({ timeout: 5_000 });
    await remoteSection.scrollIntoViewIfNeeded();
    await page.waitForTimeout(500);

    await snapEl(remoteSection, 'settings-remote');
  });

  test('worktree project mappings section', async ({ page }) => {
    await openSettings(page);

    // SettingsSection renders a heading with the title prop;
    // match the section that contains the "Worktree mappings"
    // header text, the same pattern used for Remote Access above.
    const worktreeSection = page.locator(
      '.settings-section:has-text("Worktree mappings")'
    );
    await expect(worktreeSection.first()).toBeVisible({
      timeout: 5_000,
    });
    await worktreeSection.first().scrollIntoViewIfNeeded();
    await page.waitForTimeout(500);

    await snapEl(worktreeSection.first(), 'worktree-mappings');
  });
});

// ── About dialog ─────────────────────────────────────────

test.describe('About', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('about dialog', async ({ page }) => {
    const versionEl = page.locator('button.version');
    await expect(versionEl.first()).toBeVisible({
      timeout: 5_000,
    });
    await versionEl.first().click();

    const dialog = page.locator('.about-modal');
    await expect(dialog).toBeVisible({ timeout: 5_000 });
    await snapEl(dialog, 'about-dialog');
    await page.keyboard.press('Escape');
  });
});

// ── In-session search ────────────────────────────────────

test.describe('In-session search', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
    await selectRichSession(page);
  });

  test('search bar with matches', async ({ page }) => {
    // Open in-session search with Cmd+F
    await page.keyboard.press('Meta+f');
    await page.waitForSelector(
      '.session-search, .in-session-search, .find-bar',
      { timeout: 5_000 }
    );
    await page.waitForTimeout(300);

    // Type a common word to get matches
    const input = page.locator(
      '.session-search input, ' +
      '.in-session-search input, ' +
      '.find-bar input'
    );
    await input.fill('the');
    await page.waitForTimeout(1000);

    await snap(page, 'in-session-search');

    // Close search
    await page.keyboard.press('Escape');
  });
});

// ── Token usage ──────────────────────────────────────────

test.describe('Token usage', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('token usage in session header', async ({ page }) => {
    await selectRichSession(page);
    await page.waitForTimeout(500);

    // Token badge lives in SessionBreadcrumb
    const badge = page.locator('.token-badge');
    await expect(badge.first()).toBeVisible({
      timeout: 5_000,
    });

    // Capture the parent breadcrumb row for context
    const breadcrumb = page.locator(
      '.session-breadcrumb, .breadcrumb'
    );
    if (await breadcrumb.count() > 0) {
      await snapEl(breadcrumb.first(), 'token-usage');
    } else {
      await snapEl(badge.first(), 'token-usage');
    }
  });
});

// ── Sub-agent tree ───────────────────────────────────────

test.describe('Sub-agent tree', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('collapsible sub-agent tree', async ({ page }) => {
    // The sidebar uses virtual scrolling, so only the visible
    // window of sessions is in the DOM. The first session with
    // a subagent chain may not be in the initial viewport, so
    // scroll the list until a `.tree-toggle` button appears.
    const treeToggle = page.locator('.tree-toggle');
    const scrollContainer = page.locator('.session-list-scroll');

    let found = false;
    for (let i = 0; i < 30; i++) {
      if (await treeToggle.first().isVisible().catch(() => false)) {
        found = true;
        break;
      }
      await scrollContainer.evaluate(
        (el) => { el.scrollTop += 400; }
      );
      await page.waitForTimeout(150);
    }
    expect(found).toBe(true);

    await treeToggle.first().click();
    await page.waitForTimeout(500);

    const sidebar = page.locator('.sidebar');
    await snapEl(sidebar, 'subagent-tree');
  });
});

// ── Focused transcript mode ─────────────────────────────

test.describe('Follow latest', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
    await selectRichSession(page);
  });

  test('follow latest toggle active', async ({ page }) => {
    const toggle = page.locator(
      'button[aria-label="Follow latest messages"]'
    );
    await expect(toggle).toBeVisible({ timeout: 5_000 });
    await toggle.click();
    // The button toggles its `class:active` and `aria-pressed`;
    // wait for the state to be reflected in the rendered DOM.
    await expect(toggle).toHaveAttribute('aria-pressed', 'true');
    await page.waitForTimeout(500);

    // The toggle is a 14×14 icon inside .header-btn — snapping the
    // whole 1440px header bar drowns it visually. Crop to a tight
    // ~240×56 box centered on the button so the active state and
    // its immediate neighbors (sort/layout) are both visible.
    const box = await toggle.boundingBox();
    if (!box) {
      throw new Error('follow-latest toggle has no bounding box');
    }
    const padX = 100;
    const padY = 14;
    await page.screenshot({
      path: join(DIR, 'follow-latest-toggle.png'),
      type: 'png',
      clip: {
        x: Math.max(0, box.x - padX),
        y: Math.max(0, box.y - padY),
        width: box.width + padX * 2,
        height: box.height + padY * 2,
      },
    });

    // Reset state
    await toggle.click();
  });
});

test.describe('Focused transcript mode', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
    await selectRichSession(page);
  });

  test('focused transcript view', async ({ page }) => {
    // Click the "Focused" pill in the transcript strip
    const focusedPill = page.locator(
      'button[aria-label="Focused transcript mode"]'
    );
    await expect(focusedPill).toBeVisible({ timeout: 5_000 });
    await focusedPill.click();
    await page.waitForTimeout(1000);
    await snap(page, 'focused-transcript');

    // Toggle back to normal mode
    const normalPill = page.locator(
      'button[aria-label="Normal transcript mode"]'
    );
    await normalPill.click();
  });
});

// ── Machine labels (pg sync) ────────────────────────────

test.describe('Machine labels', () => {
  test('machine labels on session items', async ({ page }) => {
    test.skip(!PG_BASE_URL, 'PG_BASE_URL not set');

    await page.setViewportSize(FULL);
    await page.goto(PG_BASE_URL);
    await page.waitForSelector('.session-item', {
      timeout: 15_000,
    });
    await page.waitForTimeout(2000);

    const machineTag = page.locator(
      '.machine-tag, .machine-label'
    );
    await expect(machineTag.first()).toBeVisible({
      timeout: 5_000,
    });

    const sidebar = page.locator('.sidebar');
    await snapEl(sidebar, 'machine-labels');
  });
});

// ── Search grouping and sort ────────────────────────────

test.describe('Search grouping', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('grouped search results with sort toggle', async ({ page }) => {
    await page.keyboard.press('Control+k');
    await page.waitForSelector('.palette-overlay', {
      timeout: 5_000,
    });

    const input = page.locator('.palette-input');
    await input.fill('implement');
    await page.waitForTimeout(1500);

    // Assert grouped results rendered (each result shows a
    // session name via .item-name, indicating per-session grouping)
    const results = page.locator('.palette-results .palette-item');
    await expect(results.first()).toBeVisible({
      timeout: 5_000,
    });
    const sessionName = results.first().locator('.item-name');
    await expect(sessionName).toBeVisible();
    const sortBtns = page.locator('.sort-btn');
    await expect(sortBtns.first()).toBeVisible({
      timeout: 5_000,
    });

    // Verify relevance is the default active sort
    const relevanceBtn = page.locator('.sort-btn.active');
    await expect(relevanceBtn).toHaveText('Relevance');

    // Toggle to recency and verify it becomes active
    const recencyBtn = page.locator('.sort-btn', {
      hasText: 'Recency',
    });
    await recencyBtn.click();
    await page.waitForTimeout(500);
    await expect(recencyBtn).toHaveClass(/active/);

    await snap(page, 'search-grouped');
  });
});

// ── Model info in session header ────────────────────────

test.describe('Model info', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('model badge is visible in session header', async ({ page }) => {
    await selectRichSession(page);
    await page.waitForTimeout(500);

    // Assert the model badge renders — no separate screenshot
    // since it shares the breadcrumb with the token-usage shot.
    const modelBadge = page.locator('.model-badge');
    await expect(modelBadge.first()).toBeVisible({
      timeout: 5_000,
    });
  });
});

// ── Import conversations ────────────────────────────────

test.describe('Import conversations', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('import button in header', async ({ page }) => {
    const importBtn = page.locator(
      'button[title="Import conversations"]'
    );
    await expect(importBtn).toBeVisible({ timeout: 5_000 });

    // Capture a region around the button for context — snap
    // the header-right section that contains it.
    const headerRight = page.locator('.header-right');
    if (await headerRight.count() > 0) {
      await snapEl(headerRight, 'import-button');
    } else {
      await snapEl(importBtn, 'import-button');
    }
  });

  test('import modal claude-ai', async ({ page }) => {
    const importBtn = page.locator(
      'button[title="Import conversations"]'
    );
    await expect(importBtn).toBeVisible({ timeout: 5_000 });
    await importBtn.click();

    const modal = page.locator('div[role="dialog"]');
    await expect(modal).toBeVisible({ timeout: 5_000 });
    await page.waitForTimeout(300);

    // Claude.ai is the default provider
    await snapEl(modal, 'import-modal-claude');

    await page.keyboard.press('Escape');
  });

  test('import modal chatgpt', async ({ page }) => {
    const importBtn = page.locator(
      'button[title="Import conversations"]'
    );
    await expect(importBtn).toBeVisible({ timeout: 5_000 });
    await importBtn.click();

    const modal = page.locator('div[role="dialog"]');
    await expect(modal).toBeVisible({ timeout: 5_000 });
    await page.waitForTimeout(300);

    // Select ChatGPT provider via its label
    await modal.getByText('ChatGPT').click();
    await page.waitForTimeout(300);

    await snapEl(modal, 'import-modal-chatgpt');

    await page.keyboard.press('Escape');
  });
});

// ── Session Vital Signs ─────────────────────────────────

test.describe('Session Vital Signs', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  // Walk the sidebar until we find a session whose vitals panel
  // actually has content (a populated `Calls` list). Sessions
  // without tool calls render an empty-ish panel that makes a dull
  // screenshot, so we prefer one that exercises the layout.
  async function selectSessionWithCalls(page: Page) {
    const items = page.locator('.session-item');
    const total = await items.count();
    const limit = Math.min(total, 25);
    const vitalsBtn = page.locator(
      'button[aria-label="Show session analysis"], button[aria-label="Hide session analysis"]',
    );
    for (let i = 0; i < limit; i++) {
      await items.nth(i).click();
      await page.waitForSelector('.message', { timeout: 10_000 });
      await page.waitForTimeout(400);
      if (!(await vitalsBtn.count()) || !(await vitalsBtn.isVisible())) {
        continue;
      }
      const aside = page.locator('aside.vitals');
      const isOpen = (await aside.count()) > 0;
      if (!isOpen) {
        await vitalsBtn.click();
        await page.waitForTimeout(600);
      }
      const callRows = page.locator('aside.vitals .call');
      if ((await callRows.count()) >= 3) {
        return;
      }
      // Close before trying the next session.
      if (!isOpen) {
        await vitalsBtn.click();
        await page.waitForTimeout(150);
      }
    }
    throw new Error(
      `no session with a populated vitals panel found in first ${limit} items`,
    );
  }

  test('session with vital signs', async ({ page }) => {
    await selectSessionWithCalls(page);
    const aside = page.locator('aside.vitals');
    await expect(aside).toBeVisible({ timeout: 5_000 });
    await snap(page, 'session-vital-signs');
  });

  test('vital signs panel detail', async ({ page }) => {
    await selectSessionWithCalls(page);
    const aside = page.locator('aside.vitals');
    await expect(aside).toBeVisible({ timeout: 5_000 });
    await snapEl(aside, 'vital-signs-panel');
  });
});

// ── Trends ──────────────────────────────────────────────

test.describe('Trends', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  test('trends page', async ({ page }) => {
    await page.goto('/trends');
    await page.waitForSelector('.trends-page', { timeout: 10_000 });
    // The default-terms request resolves async; wait for both the
    // chart svg lines and the term-table rows so the page is fully
    // populated before we snap.
    await page.waitForSelector('.term-table-wrap tbody tr', {
      timeout: 15_000,
    });
    await page.waitForTimeout(1500);
    await snap(page, 'trends');
  });
});

// ── Session intelligence ────────────────────────────────

test.describe('Session intelligence', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
  });

  // Walks through the sidebar until it finds a session whose
  // header renders a `.grade-badge`. Sessions without scored
  // signals don't render the badge at all, so we can't rely on
  // `selectRichSession` here.
  async function selectGradedSession(page: Page) {
    const items = page.locator('.session-item');
    const total = await items.count();
    const limit = Math.min(total, 25);
    for (let i = 0; i < limit; i++) {
      await items.nth(i).click();
      await page.waitForSelector('.message', { timeout: 10_000 });
      await page.waitForTimeout(400);
      const badge = page.locator('.grade-badge');
      if (
        (await badge.count()) > 0 &&
        (await badge.first().isVisible())
      ) {
        return;
      }
    }
    throw new Error(
      `no session with a .grade-badge found in first ${limit} items`,
    );
  }

  // Same as `selectGradedSession` but requires the opened signal
  // panel to render `.penalty` chips. Panels with no penalties are
  // valid but make a duller screenshot, so we prefer a session
  // whose panel actually exercises the penalty-chip layout. Falls
  // back to any graded session if no session in the scan window
  // has penalties.
  async function openSignalPanelWithPenalties(page: Page) {
    const items = page.locator('.session-item');
    const total = await items.count();
    const limit = Math.min(total, 30);
    let firstGradedIdx = -1;
    for (let i = 0; i < limit; i++) {
      await items.nth(i).click();
      await page.waitForSelector('.message', { timeout: 10_000 });
      await page.waitForTimeout(300);
      const badge = page.locator('.grade-badge').first();
      if (!(await badge.count()) || !(await badge.isVisible())) {
        continue;
      }
      if (firstGradedIdx < 0) firstGradedIdx = i;
      await badge.click();
      const panel = page.locator('.signal-panel');
      await expect(panel).toBeVisible({ timeout: 3_000 });
      await page.waitForTimeout(200);
      if ((await panel.locator('.penalty').count()) > 0) {
        return;
      }
      await badge.click(); // close and move on
      await page.waitForTimeout(100);
    }
    if (firstGradedIdx < 0) {
      throw new Error('no graded session found in scan window');
    }
    // Fall back to the first graded session we saw.
    await items.nth(firstGradedIdx).click();
    await page.waitForSelector('.message', { timeout: 10_000 });
    await page.waitForTimeout(300);
    await page.locator('.grade-badge').first().click();
    await expect(page.locator('.signal-panel')).toBeVisible({
      timeout: 3_000,
    });
    await page.waitForTimeout(200);
  }

  test('grade badge in session header', async ({ page }) => {
    await selectGradedSession(page);
    // Capture the breadcrumb row so the badge is shown in context
    // alongside the session title and action buttons.
    const breadcrumb = page.locator('.session-breadcrumb').first();
    await snapEl(breadcrumb, 'grade-badge');
  });

  test('signal panel dropdown', async ({ page }) => {
    await openSignalPanelWithPenalties(page);

    const panel = page.locator('.signal-panel');
    await snapEl(panel, 'signal-panel');

    // Toggle off to leave the UI clean for later tests.
    await page.locator('.grade-badge').first().click();
  });
});

// ── Dashboard session health section ────────────────────

test.describe('Dashboard session health', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
    await setDateRange1Y(page);
  });

  test('session health section', async ({ page }) => {
    const section = page.locator('.health-section');
    await expect(section).toBeVisible({ timeout: 10_000 });
    await section.scrollIntoViewIfNeeded();
    await page.waitForTimeout(800);
    await snapEl(section, 'session-health');
  });
});

// ── Usage dashboard (token usage & cost) ────────────────

test.describe('Usage dashboard', () => {
  async function navigateToUsage(page: Page) {
    const navBtn = page.locator('.nav-btn', { hasText: 'Usage' });
    await expect(navBtn).toBeVisible({ timeout: 5_000 });
    await navBtn.click();
    await page.waitForSelector('.usage-page', { timeout: 10_000 });
    // Wait for summary + charts to finish loading
    await expect(
      page.locator('.usage-page .summary-cards .card').first()
    ).toBeVisible({ timeout: 10_000 });
    // Give SVG charts time to render
    await page.waitForTimeout(2000);
  }

  test.beforeEach(async ({ page }) => {
    await page.setViewportSize(FULL);
    await waitForApp(page);
    await navigateToUsage(page);
  });

  test('full usage page', async ({ page }) => {
    await snap(page, 'usage-page');
  });

  test('usage summary cards', async ({ page }) => {
    const cards = page.locator('.usage-page .summary-cards');
    await snapEl(cards, 'usage-summary-cards');
  });

  test('usage toolbar with filters', async ({ page }) => {
    const toolbar = page.locator('.usage-toolbar');
    await snapEl(toolbar, 'usage-toolbar');
  });

  test('cost over time chart', async ({ page }) => {
    const panel = page.locator(
      '.usage-page .chart-panel:has(.chart-title:text("Cost Over Time"))'
    );
    await expect(panel).toBeVisible({ timeout: 5_000 });
    await panel.scrollIntoViewIfNeeded();
    await page.waitForTimeout(500);
    await snapEl(panel, 'usage-cost-trend');
  });

  test('attribution treemap', async ({ page }) => {
    const panel = page.locator('.attribution-panel');
    await expect(panel).toBeVisible({ timeout: 5_000 });
    await panel.scrollIntoViewIfNeeded();
    await page.waitForTimeout(500);
    // Treemap is the default view
    await snapEl(panel, 'usage-attribution');
  });

  test('top sessions by cost', async ({ page }) => {
    const panel = page.locator(
      '.chart-panel:has(.top-sessions-container)'
    );
    await expect(panel).toBeVisible({ timeout: 5_000 });
    await panel.scrollIntoViewIfNeeded();
    await page.waitForTimeout(500);
    await snapEl(panel, 'usage-top-sessions');
  });

  test('cache efficiency panel', async ({ page }) => {
    const panel = page.locator('.chart-panel:has(.cache-panel)');
    await expect(panel).toBeVisible({ timeout: 5_000 });
    await panel.scrollIntoViewIfNeeded();
    await page.waitForTimeout(500);
    await snapEl(panel, 'usage-cache-efficiency');
  });

  test('model filter dropdown open', async ({ page }) => {
    // Click the Model filter trigger in the toolbar
    const trigger = page.locator(
      '.usage-toolbar .filter-dropdown .filter-trigger',
      { hasText: 'Model' }
    );
    await expect(trigger).toBeVisible({ timeout: 5_000 });
    await trigger.click();
    await page.waitForSelector(
      '.filter-dropdown .dropdown-panel',
      { timeout: 5_000 }
    );
    await page.waitForTimeout(300);
    await snap(page, 'usage-filter-dropdown');
    await page.keyboard.press('Escape');
  });
});
