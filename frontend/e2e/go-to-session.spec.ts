import { createRequire } from "node:module";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "@playwright/test";

type RenderLintModule = {
  renderLintSnippet: (scopeSelector: string, options?: Record<string, unknown>) => string;
};

const require = createRequire(import.meta.url);
const renderLintPath = process.env.PR_RENDER_LINT_PATH;
const renderLint = renderLintPath
  ? (require(renderLintPath) as RenderLintModule)
  : undefined;
const renderProofDir = process.env.PR_RENDER_PROOF_DIR;
const renderArtifactDir = process.env.PR_RENDER_ARTIFACT_DIR;

const TARGET_UUID = "123e4567-e89b-12d3-a456-426614174000";
const MISSING_UUID = "123e4567-e89b-12d3-a456-426614174001";
const UNKNOWN_UUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const TARGET_ID = `remote-host~codex:${TARGET_UUID}`;
const MISSING_ID = `remote-host~codex:${MISSING_UUID}`;
const OPAQUE_ID = "test-session-project-reclassification-nested";
const TARGET_PATH = `/sessions/remote-host~codex/${TARGET_UUID}`;
const MISSING_PATH = `/sessions/remote-host~codex/${MISSING_UUID}`;
const OPAQUE_PATH = `/sessions/${encodeURIComponent(OPAQUE_ID)}`;

const now = "2026-09-14T12:00:00Z";

function session(id: string, firstMessage: string) {
  return {
    id,
    project: "go-to-session-project",
    machine: "test-machine",
    agent: "codex",
    first_message: firstMessage,
    display_name: "Off-list session",
    started_at: now,
    ended_at: now,
    created_at: now,
    message_count: 1,
    user_message_count: 1,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    is_automated: false,
    is_teammate: false,
  };
}

function message(id: string, content: string) {
  return {
    id: 700001,
    session_id: id,
    ordinal: 0,
    role: "assistant",
    content,
    content_length: content.length,
    timestamp: now,
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
  };
}

test.describe("go to session", () => {
  test("opens by shortcut, resolves an off-list UUID, and preserves session state", async ({
    page,
  }, testInfo) => {
    const listResponses: Array<{ ids: string[] }> = [];
    await page.route("**/api/v1/session-ids/resolve**", async (route) => {
      const partial = new URL(route.request().url()).searchParams.get("partial");
      if (partial === TARGET_UUID) {
        await route.fulfill({ json: { ids: [TARGET_ID] } });
      } else if (partial === MISSING_UUID) {
        await route.fulfill({ json: { ids: [MISSING_ID] } });
      } else if (partial === OPAQUE_ID) {
        await route.fulfill({ json: { ids: [OPAQUE_ID] } });
      } else {
        await route.fulfill({ json: { ids: [] } });
      }
    });

    await page.route(/\/api\/v1\/sessions(?:\/sidebar-index)?(?:\?.*)?$/, async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      const ids = Array.isArray(body.sessions)
        ? body.sessions.map((item: { id: string }) => item.id)
        : [];
      listResponses.push({ ids });
      const sessions = Array.isArray(body.sessions)
        ? body.sessions.filter((item: { id: string }) => !item.id.includes(TARGET_UUID))
        : body.sessions;
      await route.fulfill({ response, json: { ...body, sessions } });
    });

    await page.route(/\/api\/v1\/sessions\/[^/]+$/, async (route) => {
      const pathname = new URL(route.request().url()).pathname;
      const id = decodeURIComponent(pathname.slice(pathname.lastIndexOf("/") + 1));
      if (id === TARGET_ID) {
        await route.fulfill({ json: session(TARGET_ID, "Off-list target") });
      } else if (id === MISSING_ID) {
        await route.fulfill({ status: 404, json: { error: "session not found" } });
      } else if (id === OPAQUE_ID) {
        await route.fulfill({ json: session(OPAQUE_ID, "Opaque session ID") });
      } else {
        await route.fallback();
      }
    });

    await page.route(/\/api\/v1\/sessions\/[^/]+\/messages(?:\?.*)?$/, async (route) => {
      const pathname = new URL(route.request().url()).pathname;
      const id = decodeURIComponent(pathname.split("/").at(-2)!);
      if (id === TARGET_ID) {
        await route.fulfill({
          json: {
            messages: [message(TARGET_ID, "Off-list hydrated content hydrated")],
            count: 1,
          },
        });
      } else if (id === MISSING_ID) {
        await route.fulfill({ status: 404, json: { error: "session not found" } });
      } else if (id === OPAQUE_ID) {
        await route.fulfill({
          json: {
            messages: [message(OPAQUE_ID, "Opaque session ID content")],
            count: 1,
          },
        });
      } else {
        await route.fallback();
      }
    });

    await page.goto("/usage?desktop=1");
    await expect(page.locator(".usage-page")).toBeVisible();
    const initialHistoryLength = await page.evaluate(() => window.history.length);
    await page.evaluate(() => {
      const observed = [] as boolean[];
      document.addEventListener("keydown", (event) => {
        if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "g") {
          observed.push(event.defaultPrevented);
        }
      });
      Object.defineProperty(window, "__goToSessionShortcutObserved", {
        configurable: true,
        value: observed,
      });
    });

    const modifier = await page.evaluate(() => (/Mac/.test(navigator.platform) ? "Meta" : "Control"));
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.keyboard.press(`${modifier}+g`);
    const dialog = page.getByRole("dialog", { name: "Go to session" });
    const input = page.getByRole("textbox", { name: "Session ID or UUID" });
    await expect(dialog).toBeVisible();
    await expect(input).toBeFocused();
    const focusStyles = await page.locator(".go-to-session-control").evaluate((element) => ({
      control: getComputedStyle(element).outlineStyle,
      input: getComputedStyle(element.querySelector("input")!).outlineStyle,
    }));
    expect(focusStyles).toEqual({ control: "none", input: "none" });

    await input.fill(UNKNOWN_UUID);
    await input.press("Enter");
    await expect(page.getByRole("alert")).toHaveText("No session matches that ID or UUID.");
    await expect(page).toHaveURL(/\/usage\?desktop=1$/);

    for (const width of [1280, 768, 400]) {
      await page.setViewportSize({ width, height: 900 });
      await expect(dialog).toBeVisible();
      const panel = await page.locator(".kit-modal-panel").boundingBox();
      const field = await input.boundingBox();
      expect(panel).not.toBeNull();
      expect(field).not.toBeNull();
      expect(panel!.x + panel!.width).toBeLessThanOrEqual(width + 1);
      expect(panel!.x).toBeGreaterThanOrEqual(-1);
      expect(field!.x + field!.width).toBeLessThanOrEqual(width + 1);
      await page.screenshot({ path: testInfo.outputPath(`go-to-session-error-${width}.png`) });
      if (renderLint) {
        const lint = await page.evaluate((snippet) => (0, eval)(snippet),
          renderLint.renderLintSnippet(".kit-modal-panel"));
        console.log(`render-lint width=${width}px violations=${JSON.stringify(lint)}`);
        if (renderProofDir) {
          mkdirSync(renderProofDir, { recursive: true });
          writeFileSync(
            join(renderProofDir, `go-to-session-render-lint-${width}.json`),
            JSON.stringify(lint, null, 2),
          );
        }
        expect(lint).toEqual([]);
      }
      if (renderArtifactDir) {
        mkdirSync(renderArtifactDir, { recursive: true });
        await page.screenshot({
          path: join(
            renderArtifactDir,
            width === 1280
              ? "agentsview-1768-2-after.png"
              : `agentsview-1768-2-${width}.png`,
          ),
        });
      }
    }

    const observed = await page.evaluate(
      () => (window as Window & { __goToSessionShortcutObserved?: boolean[] })
        .__goToSessionShortcutObserved,
    );
    expect(observed?.[0]).toBe(true);
    await input.press("Escape");
    await expect(dialog).toHaveCount(0);

    const beforeTarget = await page.evaluate(() => window.history.length);
    await page.keyboard.press(`${modifier}+g`);
    await expect(input).toBeFocused();
    await input.fill(TARGET_UUID);
    await input.press("Enter");

    await expect(page).toHaveURL(new RegExp(`${TARGET_PATH.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\?desktop=1$`));
    await expect(page.getByText("Off-list hydrated content hydrated")).toBeVisible();
    expect(await page.evaluate(() => window.history.length)).toBe(beforeTarget + 1);
    expect(await page.evaluate(() => window.location.search)).toBe("?desktop=1");
    expect(await page.evaluate(() => window.location.pathname)).toBe(TARGET_PATH);
    expect(listResponses.length).toBeGreaterThan(0);
    expect(listResponses.every(({ ids }) => ids.every((id) => !id.includes(TARGET_UUID)))).toBe(true);

    await page.keyboard.press(`${modifier}+f`);
    const findInput = page.locator(".kit-find-bar__input");
    await expect(findInput).toBeFocused();
    await page.keyboard.insertText("hydrated");
    const announcement = page.locator(".search-announcement");
    await expect(announcement).toContainText("Match 1 of 2");
    await page.keyboard.press(`${modifier}+g`);
    await expect(announcement).toContainText("Match 2 of 2");
    await page.keyboard.press(`${modifier}+Shift+g`);
    await expect(announcement).toContainText("Match 1 of 2");
    await expect(findInput).toHaveCount(1);
    await expect(dialog).toHaveCount(0);

    await page.keyboard.press("Escape");
    await expect(findInput).toHaveCount(0);
    await page.keyboard.press(`${modifier}+g`);
    await expect(input).toBeFocused();
    await input.fill(OPAQUE_ID);
    await input.press("Enter");

    await expect(page).toHaveURL(new RegExp(`${OPAQUE_PATH.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\?desktop=1$`));
    await expect(page.getByText("Opaque session ID content")).toBeVisible();

    await page.keyboard.press(`${modifier}+g`);
    await expect(input).toBeFocused();
    await input.fill(MISSING_UUID);
    await input.press("Enter");

    await expect(page).toHaveURL(new RegExp(`${MISSING_PATH.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\?desktop=1$`));
    await expect(page.getByText("Session not found")).toBeVisible();
    await expect(page.getByText("Off-list hydrated content hydrated")).toHaveCount(0);
  });
});
