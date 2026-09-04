// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { usage } from "../../stores/usage.svelte.js";
import { testMoney } from "../../test/money.js";
import TopSessionsTable from "./TopSessionsTable.svelte";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) {
    unmount(component);
    component = undefined;
  }
  usage.topSessions = null;
  usage.mode = "cost";
  usage.setSelectedTokenTypes(["input", "cache_write", "cache_read", "output"]);
  document.body.innerHTML = "";
});

describe("TopSessionsTable", () => {
  it("renders and labels the selected output-token ranking", async () => {
    usage.mode = "token";
    usage.setSelectedTokenTypes(["output"]);
    usage.topSessions = [
      {
        sessionId: "session-1",
        displayName: "Output-heavy session",
        agent: "codex",
        project: "demo",
        startedAt: "2026-07-01T00:00:00Z",
        inputTokens: 100,
        cacheCreationTokens: 40,
        cacheReadTokens: 800,
        outputTokens: 25,
        totalTokens: 965,
        cost: testMoney(1),
      },
    ];

    component = mount(TopSessionsTable, {
      target: document.body,
    });
    await tick();

    expect(document.querySelector(".chart-title")?.textContent?.trim()).toBe(
      "Top Sessions by Output Tokens",
    );
    expect(document.querySelector(".session-tokens")?.textContent?.trim()).toBe("25");
  });
});
