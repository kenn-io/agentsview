// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import SessionFilterControl from "./SessionFilterControl.svelte";
import { sessions } from "../../stores/sessions.svelte.js";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
  sessions.agents = [];
  sessions.filters.agent = "";
  vi.restoreAllMocks();
});

describe("SessionFilterControl agent options", () => {
  it("keeps custom session labels under one base Claude option", () => {
    sessions.agents = [{ name: "claude", session_count: 2 }];
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "loadMachines").mockResolvedValue();

    component = mount(SessionFilterControl, { target: document.body });
    document.querySelector<HTMLButtonElement>(".filter-btn")?.click();
    flushSync();

    const rows = document.querySelectorAll(".agent-select-row");
    expect(rows).toHaveLength(2);
    expect(document.querySelectorAll(".agent-select-name")[1]?.textContent).toBe(
      "Claude",
    );

    (rows[1] as HTMLButtonElement).click();
    flushSync();
    expect(sessions.filters.agent).toBe("claude");
  });
});
