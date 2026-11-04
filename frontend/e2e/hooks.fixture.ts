import { createMockSessions } from "./helpers/mock-sessions.js";
import type { Page } from "@playwright/test";
import type { DbMessage as Message } from "../src/lib/api/generated/index.js";

// Synthetic API data only. No local agent archive or server account is used.
export const HOOK_SESSION_ID = "hook-visibility-fixture";
export const HOOK_TEXT =
  'Hook name: Example\n\nHook event: PreToolUse\n\nContent:\n  中文 e\u0301\n<script>globalThis.hookExecuted = true</script>\n\nStdout:\n{"result":"' +
  "long-output-".repeat(35) +
  '"}\n\nStderr:\n  preserved whitespace  \n';

export async function installHooksFixture(page: Page, includeHook = true) {
  const unknownRequests: string[] = [];
  const timestamp = "2024-01-01T00:00:00Z";
  const row = (
    ordinal: number,
    role: string,
    content: string,
    extras: Partial<Message> = {},
  ): Message => ({
    id: ordinal + 1,
    session_id: HOOK_SESSION_ID,
    ordinal,
    role,
    content,
    timestamp,
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: content.length,
    model: "",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    has_context_tokens: false,
    has_output_tokens: false,
    is_system: false,
    ...extras,
  });
  const messages = [
    row(0, "user", "Synthetic request"),
    row(1, "assistant", "Intermediate reply"),
    ...(includeHook
      ? [
          row(2, "user", HOOK_TEXT, {
            is_system: true,
            source_type: "attachment",
            source_subtype: "hook_success",
            source_uuid: "synthetic-hook",
          }),
        ]
      : []),
    row(3, "assistant", "Final reply"),
    row(4, "user", "Existing boundary content", {
      is_system: true,
      source_type: "system",
      source_subtype: "continuation",
    }),
  ];
  const session = {
    ...createMockSessions(1, "hooks", () => "synthetic-hooks")[0],
    id: HOOK_SESSION_ID,
    agent: "claude",
    first_message: "Synthetic request",
    display_name: "Synthetic hook transcript",
    message_count: messages.length,
    user_message_count: 1,
  };
  await page.addInitScript(() => {
    for (const [key, value] of Object.entries({
      "agentsview-locale": "en",
      "agentsview-transcript-mode": "normal",
      "agentsview-block-filters": JSON.stringify({ hidden: [] }),
      theme: "light",
    })) {
      if (localStorage.getItem(key) === null) localStorage.setItem(key, value);
    }
  });
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    let body: unknown = {};
    if (path.endsWith("/sessions") || path.endsWith("/sidebar-index"))
      body = { sessions: [session], total: 1, next_cursor: null };
    else if (path.endsWith(`/sessions/${HOOK_SESSION_ID}`)) body = session;
    else if (path.includes(`/sessions/${HOOK_SESSION_ID}/messages`))
      body = { messages, count: messages.length };
    else if (path.endsWith("/version"))
      body = {
        api_version: 1,
        version: "test",
        commit: "6571f86",
        build_date: "",
        data_version: 0,
        friction_available: false,
        friction_build_allowed: false,
        insight_generation_available: false,
        kata_available: false,
        session_stats_available: false,
      };
    else if (path.endsWith("/settings"))
      body = {
        agent_dirs: {},
        chart_palette: "agentsview",
        disabled_agents: [],
        github_configured: false,
        host: "127.0.0.1",
        insight_default_agent: "",
        port: 0,
        read_only: false,
        require_auth: false,
        session_providers: [],
        terminal: { mode: "auto" },
        tool_result_images: "keep",
        zoom_level: 100,
      };
    else if (path.endsWith("/agents")) body = { agents: [{ agent: "claude", session_count: 1 }] };
    else if (path.endsWith("/machines"))
      body = { machines: ["test-machine"], machine_aliases: {}, machine_labels: {} };
    else if (path.endsWith("/projects")) body = { projects: [] };
    else if (path.endsWith("/stats"))
      body = {
        session_count: 1,
        project_count: 1,
        machine_count: 1,
        message_count: messages.length,
        earliest_session: timestamp,
      };
    else if (path.endsWith("/trash")) body = { sessions: [], total: 0 };
    else if (path.endsWith("/codex-cloud/status") || path.endsWith("/children")) body = [];
    else if (path.endsWith("/pins")) body = { pins: [] };
    else if (path.endsWith("/events")) {
      await route.fulfill({ contentType: "text/event-stream", body: ": synthetic fixture\n\n" });
      return;
    } else unknownRequests.push(route.request().method() + " " + path);
    await route.fulfill({ json: body });
  });
  return { session, messages, unknownRequests };
}
