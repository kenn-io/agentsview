// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import { createClassComponent } from "svelte/legacy";
import SessionItem from "./SessionItem.svelte";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
});

describe("SessionItem identity", () => {
  const baseSession = {
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

  it("renders the session label and entrypoint badge", () => {
    mountSession({ id: "custom-label", agent_label: "Claude Triage", entrypoint: "sdk-cli" });

    const agentTag = document.querySelector<HTMLElement>(".agent-tag");
    expect(agentTag?.textContent).toBe("Claude Triage");
    expect(agentTag?.title).toBe("Claude Triage");
    expect(document.querySelector(".entrypoint-tag")?.textContent).toBe("sdk-cli");
  });

  it("uses the registry label when no override exists", () => {
    mountSession({ agent: "qwen" });

    const agentTag = document.querySelector<HTMLElement>(".agent-tag");
    expect(agentTag?.textContent).toBe("Qwen Code");
    expect(agentTag?.title).toBe("Qwen Code");
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
