// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi, onTestFinished } from "vite-plus/test";
import { flushSync, mount, unmount, tick } from "svelte";
import { createClassComponent } from "svelte/legacy";
import SessionItem from "./SessionItem.svelte";
import { sessions } from "../../stores/sessions.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { formatTimestamp } from "../../utils/format.js";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
  ui.alwaysShowYear = false;
  sessions.machineLabels = {};
  sessions.details.clear();
});

describe("SessionItem identity", () => {
  const baseSession = {
    compaction_count: 0,
    consecutive_failure_max: 0,
    edit_churn_count: 0,
    ended_with_role: "",
    final_failure_streak: 0,
    has_peak_context_tokens: false,
    has_total_output_tokens: false,
    peak_context_tokens: 0,
    total_output_tokens: 0,
    mid_task_compaction_count: 0,
    outcome: "",
    outcome_confidence: "",
    secret_leak_count: 0,
    tool_failure_signal_count: 0,
    tool_retry_count: 0,
    id: "session",
    project: "project",
    machine: "local",
    agent: "claude",
    first_message: "Inspect the issue",
    started_at: "2024-01-01T00:00:00Z",
    ended_at: "2024-01-01T00:01:00Z",
    created_at: "2024-01-01T00:00:00Z",
    message_count: 1,
    user_message_count: 1,
  };

  function mountSession(overrides: Record<string, unknown> = {}, props = {}) {
    component = mount(SessionItem, {
      target: document.body,
      props: {
        session: { ...baseSession, ...overrides },
        ...props,
      },
    });
    flushSync();
  }

  it.each([false, true])(
    "updates the date and keeps the full hover time in compact=%s",
    (compact) => {
      const ended = "2024-06-01T12:00:00Z";
      mountSession({ ended_at: ended }, { compact });
      const time = () => document.querySelector<HTMLElement>(".session-time")!;
      expect(time().title).toBe(formatTimestamp(ended, true));
      expect(time().textContent).not.toContain("2024");
      ui.alwaysShowYear = true;
      flushSync();
      expect(time().textContent).toContain("2024");
      ui.alwaysShowYear = false;
      flushSync();
      expect(time().textContent).not.toContain("2024");
    },
  );

  it("updates a mounted row when its title arrives", () => {
    mountSession({ first_message: null, is_index_only: true });
    const row = document.querySelector(".session-item");
    sessions.details.set("session", {
      ...baseSession,
      is_automated: false,
      first_message: "Loaded title",
    });
    flushSync();
    expect(document.querySelector(".session-item")).toBe(row);
    expect(document.querySelector(".session-name")?.textContent).toBe("Loaded title");
  });

  it("renders the session label and puts the entrypoint in the agent tooltip", () => {
    sessions.machineLabels = { "installation-a": "Workstation A" };
    mountSession({
      id: "custom-label",
      display_name: "A session with a long title",
      agent_label: "Claude Triage",
      entrypoint: "sdk-cli",
      machine: "installation-a",
    });

    const agentTag = document.querySelector<HTMLElement>(".agent-tag");
    expect(agentTag?.textContent).toBe("Claude Triage");
    expect(agentTag?.title).toBe("Claude Triage · sdk-cli");
    // The entrypoint is detail: it no longer takes room on the row.
    expect(document.querySelector(".entrypoint-tag")).toBeNull();
    expect(document.querySelector<HTMLElement>(".session-name")?.title).toBe(
      "A session with a long title",
    );
    expect(document.querySelector<HTMLElement>(".session-project")?.title).toBe("project");
    expect(document.querySelector(".machine-tag")?.textContent).toBe("Workstation A");
  });

  it("explains the message count on hover", () => {
    mountSession({ id: "count", user_message_count: 1200 });
    const count = document.querySelector<HTMLElement>(".session-count");
    expect(count?.textContent).toBe("1.2K");
    expect(count?.title).toBe("1.2K user messages");
  });

  it("removes the star while the title is being renamed", () => {
    mountSession({ id: "rename-star" });
    expect(document.querySelector(".star-btn")).not.toBeNull();

    document
      .querySelector<HTMLElement>(".session-name")!
      .dispatchEvent(new MouseEvent("dblclick", { bubbles: true, cancelable: true }));
    flushSync();

    const input = document.querySelector<HTMLInputElement>(".rename-input");
    expect(input).not.toBeNull();
    expect(document.querySelector(".star-btn")).toBeNull();

    input!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    flushSync();

    expect(document.querySelector(".rename-input")).toBeNull();
    expect(document.querySelector(".star-btn")).not.toBeNull();
  });

  it("keeps a rejected rename for retry", async () => {
    const rename = vi
      .spyOn(sessions, "renameSession")
      .mockRejectedValueOnce(new Error("retry shortly"))
      .mockResolvedValueOnce(undefined);
    onTestFinished(() => rename.mockRestore());
    mountSession();
    document
      .querySelector<HTMLElement>(".session-name")!
      .dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));
    await tick();
    const input = document.querySelector<HTMLInputElement>(".rename-input")!;
    input.focus();
    input.value = "My new name";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await tick();
    await tick();
    expect(document.querySelector<HTMLInputElement>(".rename-input")?.value).toBe("My new name");
    expect(document.activeElement).toBe(document.querySelector(".rename-input"));
    expect(document.querySelector('[role="alert"]')?.textContent).toContain("retry shortly");
    document
      .querySelector<HTMLInputElement>(".rename-input")!
      .dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await tick();
    await tick();
    expect(rename).toHaveBeenLastCalledWith("session", "My new name");
    expect(document.querySelector(".rename-input")).toBeNull();
    expect(document.querySelector('[role="alert"]')).toBeNull();
  });

  it("does not restore a failed rename on another session", async () => {
    let reject!: (error: Error) => void;
    const pending = new Promise<void>((_, rejectPromise) => { reject = rejectPromise; });
    const rename = vi.spyOn(sessions, "renameSession").mockReturnValueOnce(pending);
    onTestFinished(() => rename.mockRestore());
    const instance = createClassComponent({ component: SessionItem, target: document.body, props: { session: { ...baseSession, id: "first" } } });
    onTestFinished(() => instance.$destroy());
    flushSync();
    document.querySelector<HTMLElement>(".session-name")!.dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));
    await tick();
    const input = document.querySelector<HTMLInputElement>(".rename-input")!;
    input.value = "First title";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    instance.$set({ session: { ...baseSession, id: "second" } });
    await tick();
    reject(new Error("retry shortly"));
    await tick();
    await tick();
    expect(document.querySelector(".rename-input")).toBeNull();
    expect(rename).toHaveBeenCalledTimes(1);
    expect(rename).toHaveBeenCalledWith("first", "First title");
  });

  it("keeps focus outside a rejected rename submitted on blur", async () => {
    const rename = vi.spyOn(sessions, "renameSession").mockRejectedValue(new Error("retry shortly"));
    onTestFinished(() => rename.mockRestore());
    mountSession();
    document.querySelector<HTMLElement>(".session-name")!.dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));
    await tick();
    const input = document.querySelector<HTMLInputElement>(".rename-input")!;
    input.focus();
    const other = document.createElement("button");
    document.body.append(other);
    other.focus();
    await tick();
    await tick();
    expect(rename).toHaveBeenCalledTimes(1);
    expect(document.activeElement).toBe(other);
    expect(document.querySelector<HTMLInputElement>(".rename-input")).not.toBeNull();
  });

  it("uses the singular for one message", () => {
    mountSession({ id: "count-one", user_message_count: 1 });
    expect(document.querySelector<HTMLElement>(".session-count")?.title).toBe("1 user message");
  });

  it("uses the registry label when no override exists", () => {
    mountSession({ agent: "qwen" });

    const agentTag = document.querySelector<HTMLElement>(".agent-tag");
    expect(agentTag?.textContent).toBe("Qwen Code");
    expect(agentTag?.title).toBe("Qwen Code");
  });

  it("shows all hosted source devices instead of the projection machine", () => {
    sessions.machineLabels = { "raw-device:61": "Laptop", "raw-device:62": "Desktop" };
    mountSession({ machine: "hosted", source_machines: ["raw-device:61", "raw-device:62"] });

    const badge = document.querySelector<HTMLElement>(".machine-tag");
    expect(badge?.textContent).toBe("Laptop, Desktop");
  });

  it("uses the registry label for a whitespace-only override", () => {
    mountSession({ agent: "qwen", agent_label: "   " });

    const agentTag = document.querySelector<HTMLElement>(".agent-tag");
    expect(agentTag?.textContent).toBe("Qwen Code");
    expect(agentTag?.title).toBe("Qwen Code");
  });

  it("updates the visible label and title when the session prop changes", () => {
    const session = { ...baseSession, agent: "qwen", agent_label: "Initial Label" };
    const legacyComponent = createClassComponent({
      component: SessionItem,
      target: document.body,
      props: { session },
    });
    component = legacyComponent as unknown as ReturnType<typeof mount>;
    flushSync();

    legacyComponent.$set({
      session: { ...session, agent_label: "Updated Label" },
    });
    flushSync();

    const agentTag = document.querySelector<HTMLElement>(".agent-tag");
    expect(agentTag?.textContent).toBe("Updated Label");
    expect(agentTag?.title).toBe("Updated Label");
  });

  it("suppresses the default cli entrypoint badge", () => {
    mountSession({ id: "default-entrypoint", entrypoint: "cli" });

    expect(document.querySelector(".agent-tag")?.textContent).toBe("Claude");
    expect(document.querySelector(".entrypoint-tag")).toBeNull();
  });

  it("preserves compact and grouped metadata visibility", () => {
    mountSession({ machine: "remote" }, { compact: true });
    expect(document.querySelector(".agent-tag")).toBeNull();
    expect(document.querySelector(".machine-tag")).toBeNull();
    expect(document.querySelector(".session-item")?.classList.contains("compact")).toBe(true);

    unmount(component!);
    component = undefined;
    document.body.innerHTML = "";

    mountSession({ machine: "remote", agent_label: "Grouped Label" }, { hideAgent: true });
    expect(document.querySelector(".agent-tag")).toBeNull();
    expect(document.querySelector(".machine-tag")?.textContent).toBe("remote");
  });
});
