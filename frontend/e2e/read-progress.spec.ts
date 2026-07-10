import { test, expect } from "@playwright/test";
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

test("persists read progress until later output is visible", async ({ page }) => {
  let messageCount = 50;
  const [session] = createMockSessions(1, "read-progress", () => "project");
  session!.id = "read-progress";
  session!.message_count = messageCount;

  await page.route(
    sessionsRoutePattern,
    handleSessionsRoute([{ sessions: [session!], project: null }]),
  );
  await page.route(
    "**/api/v1/sessions/read-progress/messages*",
    async (route) => {
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

  messageCount = 60;
  session!.message_count = messageCount;
  await page.reload();
  await sp.goto();
  await sp.selectFirstSession();

  const unreadIndicator = page.locator(".unread-indicator");
  const unreadDivider = page.locator(".read-progress-divider");
  await expect(unreadIndicator).toHaveCount(1);

  await sp.scroller.evaluate((element) => {
    element.scrollTop = element.scrollHeight;
    element.dispatchEvent(new Event("scroll"));
  });
  await expect(unreadIndicator).toHaveCount(0);
  await expect(unreadDivider).toHaveCount(0);
});
