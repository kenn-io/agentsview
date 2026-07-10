import { test, expect } from "@playwright/test";
import { createRequire } from "node:module";
import path from "node:path";
import {
  createMockSessions,
  handleSessionsRoute,
  sessionsRoutePattern,
} from "./helpers/mock-sessions";
import { SessionsPage } from "./pages/sessions-page";

function makeMessages(count: number) {
  const timestamp = new Date().toISOString();
  return Array.from({ length: count }, (_, ordinal) => ({
    id: ordinal + 1,
    session_id: "read-progress",
    ordinal,
    role: ordinal % 2 === 0 ? "user" : "assistant",
    content: `Message ${ordinal}`,
    timestamp,
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: 9,
    model: "",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    has_context_tokens: false,
    has_output_tokens: false,
    tool_calls: [],
    is_system: false,
  }));
}

function renderLintSnippet(scope: string): string {
  const lintPath = process.env.PR_RENDER_LINT_PATH;
  if (!lintPath) throw new Error("PR_RENDER_LINT_PATH is required");
  const require = createRequire(import.meta.url);
  return (require(lintPath) as {
    renderLintSnippet: (selector: string, options: object) => string;
  }).renderLintSnippet(scope, {
    checks: ["overlap", "clip", "container-escape", "raw-string", "a11y"],
  });
}

test("persists read progress until later output is visible", async ({ page }) => {
  let messageCount = 50;
  let messageRequests = 0;
  const [session] = createMockSessions(1, "read-progress", () => "project");
  session!.id = "read-progress";
  session!.message_count = messageCount;
  await page.addInitScript(() => {
    class TestEventSource extends EventTarget {
      url: string;

      constructor(url: string) {
        super();
        this.url = url;
        (window as Window & { sessionSources: TestEventSource[] }).sessionSources
          ??= [];
        (window as Window & { sessionSources: TestEventSource[] }).sessionSources
          .push(this);
      }

      close() {}
    }
    window.EventSource = TestEventSource as unknown as typeof EventSource;
  });

  await page.route(
    sessionsRoutePattern,
    handleSessionsRoute([{ sessions: [session!], project: null }]),
  );
  await page.route(
    "**/api/v1/sessions/read-progress/messages*",
    async (route) => {
      messageRequests += 1;
      await route.fulfill({
        json: { messages: makeMessages(messageCount), count: messageCount },
      });
    },
  );

  const sp = new SessionsPage(page);
  await sp.goto();
  await sp.selectFirstSession();
  await expect
    .poll(() => page.evaluate(() => localStorage.getItem("agentsview-read-progress")))
    .toContain('"messageCount":50');
  const initialMessageRequests = messageRequests;

  messageCount = 60;
  session!.message_count = messageCount;
  await expect
    .poll(() => page.evaluate(() => (
      window as Window & { sessionSources: Array<{ url: string }> }
    ).sessionSources.some((source) => source.url.includes("read-progress/watch"))))
    .toBe(true);
  await page.evaluate(() => {
    const source = (window as Window & {
      sessionSources: Array<EventTarget & { url: string }>;
    }).sessionSources.find((entry) => entry.url.includes("read-progress/watch"));
    source?.dispatchEvent(new Event("session_updated"));
  });

  const unreadIndicator = page.locator(".unread-indicator");
  const unreadDivider = page.locator(".read-progress-divider");
  await expect(unreadIndicator).toHaveCount(1);
  await expect(unreadDivider).toHaveCount(0);
  await expect.poll(() => messageRequests).toBeGreaterThan(initialMessageRequests);
  await expect
    .poll(() => page.evaluate(() => localStorage.getItem("agentsview-read-progress")))
    .toContain('"messageCount":50');
  await page.reload();
  await sp.goto();
  await sp.selectFirstSession();
  await expect(unreadIndicator).toHaveCount(1);
  await sp.scroller.evaluate((element) => {
    element.scrollTop = element.scrollHeight;
    element.dispatchEvent(new Event("scroll"));
  });
  await expect(page.getByText("Message 59")).toBeVisible();
  await expect
    .poll(() => page.evaluate(() => localStorage.getItem("agentsview-read-progress")))
    .toContain('"messageCount":60');
  await expect(unreadIndicator).toHaveCount(0);
  await expect(unreadDivider).toHaveCount(0);
});

test("captures responsive unread state", async ({ page }) => {
  const artifactDir = process.env.PR_RENDER_ARTIFACT_DIR;
  test.skip(
    !process.env.PR_RENDER_LINT_PATH || !artifactDir,
    "render proof requires PR_RENDER_LINT_PATH and PR_RENDER_ARTIFACT_DIR",
  );
  const [session] = createMockSessions(1, "read-progress-proof", () => "project");
  session!.id = "read-progress-proof";
  session!.message_count = 60;
  await page.addInitScript(() => {
    localStorage.setItem("agentsview-read-progress", JSON.stringify({
      version: 1,
      sessions: {
        "read-progress-proof": { ordinal: 49, messageCount: 50 },
      },
    }));
  });
  await page.route(
    sessionsRoutePattern,
    handleSessionsRoute([{ sessions: [session!], project: null }]),
  );
  await page.route(
    "**/api/v1/sessions/read-progress-proof/messages*",
    async (route) => route.fulfill({
      json: { messages: makeMessages(60), count: 60 },
    }),
  );

  const sessions = new SessionsPage(page);
  await sessions.goto();
  await sessions.selectFirstSession();
  const unreadIndicator = page.locator(".unread-indicator");
  await expect(unreadIndicator).toHaveCount(1);

  const lint: Record<number, unknown> = {};
  for (const width of [1280, 768, 400]) {
    await page.setViewportSize({ width, height: 720 });
    await expect(unreadIndicator).toHaveCount(1);
    lint[width] = {
      sidebar: await page.evaluate(renderLintSnippet(".session-list-scroll")),
      transcript: await page.evaluate(renderLintSnippet(".message-list-scroll")),
    };
    expect(lint[width]).toEqual({ sidebar: [], transcript: [] });
    if (width === 400) {
      await expect(page.getByLabel("Close sidebar")).not.toBeVisible();
      await page.screenshot({
        path: path.join(artifactDir, "agentsview-T1057-2-after-400-closed.png"),
      });
      await page.getByLabel("Toggle sidebar").click();
      await expect(unreadIndicator).toBeVisible();
      await page.screenshot({
        path: path.join(artifactDir, "agentsview-T1057-2-after-400-sidebar.png"),
      });
      await page.getByLabel("Close sidebar").evaluate((element) => {
        element.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      });
      await expect(page.getByLabel("Close sidebar")).not.toBeVisible();
    }
    await page.screenshot({
      path: path.join(artifactDir, `agentsview-T1057-2-after-${width}.png`),
    });
  }
  await page.screenshot({
    path: path.join(artifactDir, "agentsview-T1057-2-after.png"),
  });
  console.log(`render lint ${JSON.stringify(lint)}`);
});
