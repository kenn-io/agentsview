import { test, expect, type Locator } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

function readZoom(page: import("@playwright/test").Page): Promise<string> {
  return page.evaluate(() =>
    document.documentElement.style.getPropertyValue("zoom"),
  );
}

function luminance([r, g, b]: [number, number, number]): number {
  const [rr, gg, bb] = [r, g, b].map((channel) => {
    const value = channel / 255;
    return value <= 0.03928
      ? value / 12.92
      : Math.pow((value + 0.055) / 1.055, 2.4);
  });
  return 0.2126 * rr! + 0.7152 * gg! + 0.0722 * bb!;
}

function contrastRatio(
  foreground: [number, number, number],
  background: [number, number, number],
): number {
  const lighter = Math.max(luminance(foreground), luminance(background));
  const darker = Math.min(luminance(foreground), luminance(background));
  return (lighter + 0.05) / (darker + 0.05);
}

function parseRgb(value: string): [number, number, number] {
  const match = value.match(/rgba?\((\d+),\s*(\d+),\s*(\d+)/);
  if (!match) {
    throw new Error(`Expected rgb() color, got ${value}`);
  }
  return [Number(match[1]), Number(match[2]), Number(match[3])];
}

async function elementColors(locator: Locator): Promise<{
  background: string;
  foreground: string;
}> {
  return locator.evaluate((element) => {
    const styles = getComputedStyle(element);
    return {
      background: styles.backgroundColor,
      foreground: styles.color,
    };
  });
}

function expectReadableContrast(colors: {
  background: string;
  foreground: string;
}) {
  expect(
    contrastRatio(parseRgb(colors.foreground), parseRgb(colors.background)),
  ).toBeGreaterThanOrEqual(4.5);
}

test.describe("Appearance accessibility", () => {
  test("text size scales the UI on web without horizontal overflow", async ({
    page,
  }) => {
    await page.addInitScript(() => {
      localStorage.setItem("agentsview-font-scale", "130");
    });
    const sp = new SessionsPage(page);
    await sp.goto();

    expect(await readZoom(page)).toBe("1.3");

    const overflow = await page.evaluate(
      () =>
        document.documentElement.scrollWidth -
        document.documentElement.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(2);

    await sp.selectFirstSession();
    await expect(sp.messageRows.first()).toBeVisible();
  });

  test("text size at 90% renders and scrolls the transcript", async ({
    page,
  }) => {
    await page.addInitScript(() => {
      localStorage.setItem("agentsview-font-scale", "90");
    });
    const sp = new SessionsPage(page);
    await sp.goto();

    expect(await readZoom(page)).toBe("0.9");

    await sp.selectFirstSession();
    await expect(sp.messageRows.first()).toBeVisible();
  });

  test("desktop window zoom stays separate from text size in the browser", async ({
    page,
  }) => {
    await page.addInitScript(() => {
      localStorage.setItem("agentsview-zoom-level", "150");
      localStorage.setItem("agentsview-font-scale", "120");
    });
    await page.goto("/?desktop");
    const sp = new SessionsPage(page);
    await expect(sp.sessionItems.first()).toBeVisible({ timeout: 5_000 });

    expect(await readZoom(page)).toBe("1.8");
  });

  test("high contrast applies the root class and overrides tokens", async ({
    page,
  }) => {
    await page.addInitScript(() => {
      localStorage.setItem("theme", "light");
      localStorage.setItem("agentsview-high-contrast", "true");
    });
    const sp = new SessionsPage(page);
    await sp.goto();

    const hasClass = await page.evaluate(() =>
      document.documentElement.classList.contains("high-contrast"),
    );
    expect(hasClass).toBe(true);

    // Browsers may normalize #000000 to #000; compare after stripping
    // leading # and zero-padding each channel to 6 hex digits.
    const textPrimary = await page.evaluate((): string => {
      const raw = getComputedStyle(document.documentElement)
        .getPropertyValue("--text-primary")
        .trim();
      // Expand shorthand #rgb → #rrggbb before comparing.
      if (/^#[0-9a-fA-F]{3}$/.test(raw)) {
        return "#" + raw[1]!.repeat(2) + raw[2]!.repeat(2) + raw[3]!.repeat(2);
      }
      return raw;
    });
    expect(textPrimary).toBe("#000000");
  });

  test("dark high contrast keeps accent-filled controls readable", async ({
    page,
  }) => {
    await page.addInitScript(() => {
      localStorage.setItem("theme", "dark");
      localStorage.setItem("agentsview-high-contrast", "true");
    });
    const sp = new SessionsPage(page);
    await sp.goto();

    // Primary/solid actions (kit-ui Button, modal confirm buttons) pair
    // --accent-blue fills with --accent-blue-foreground ink; kit-ui's own
    // styles are component-scoped, so measure the token pairing directly.
    const primaryButtonColors = await page.evaluate(() => {
      const button = document.createElement("button");
      button.textContent = "Save";
      button.style.background = "var(--accent-blue)";
      button.style.color = "var(--accent-blue-foreground)";
      document.body.append(button);
      const styles = getComputedStyle(button);
      const result = {
        background: styles.backgroundColor,
        foreground: styles.color,
      };
      button.remove();
      return result;
    });
    expectReadableContrast(primaryButtonColors);

    await sp.selectFirstSession();
    const agentBadge = page.locator(".agent-badge").first();
    await expect(agentBadge).toBeVisible();
    expectReadableContrast(await elementColors(agentBadge));

    const nonBlueAgentBadgeColors = await page.evaluate(() => {
      const badge = document.createElement("span");
      badge.className = "agent-badge";
      badge.style.background = "var(--accent-green)";
      badge.style.color = "var(--accent-green-foreground)";
      badge.textContent = "Codex";
      document.body.append(badge);
      const styles = getComputedStyle(badge);
      const result = {
        background: styles.backgroundColor,
        foreground: styles.color,
      };
      badge.remove();
      return result;
    });
    expectReadableContrast(nonBlueAgentBadgeColors);

    const userRoleIcon = page.locator(".role-icon", { hasText: "U" }).first();
    await expect(userRoleIcon).toBeVisible();
    expectReadableContrast(await elementColors(userRoleIcon));

    const assistantRoleIcon = page.locator(".role-icon", { hasText: "A" }).first();
    await expect(assistantRoleIcon).toBeVisible();
    expectReadableContrast(await elementColors(assistantRoleIcon));
  });
});
