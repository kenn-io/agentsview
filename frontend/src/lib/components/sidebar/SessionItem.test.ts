// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import SessionItem from "./SessionItem.svelte";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
});

describe("SessionItem identity", () => {
  it("renders the session label and entrypoint badge", () => {
    component = mount(SessionItem, {
      target: document.body,
      props: {
        session: {
          id: "custom-label",
          project: "project",
          machine: "local",
          agent: "claude",
          agent_label: "Claude Triage",
          entrypoint: "sdk-cli",
          first_message: "Inspect the issue",
          started_at: "2024-01-01T00:00:00Z",
          ended_at: "2024-01-01T00:01:00Z",
          created_at: "2024-01-01T00:00:00Z",
          message_count: 1,
          user_message_count: 1,
        },
      },
    });
    flushSync();

    expect(document.querySelector(".agent-tag")?.textContent).toBe(
      "Claude Triage",
    );
    expect(document.querySelector(".entrypoint-tag")?.textContent).toBe(
      "sdk-cli",
    );
  });

  it("suppresses the default cli entrypoint badge", () => {
    component = mount(SessionItem, {
      target: document.body,
      props: {
        session: {
          id: "default-entrypoint",
          project: "project",
          machine: "local",
          agent: "claude",
          entrypoint: "cli",
          first_message: "Inspect the issue",
          started_at: "2024-01-01T00:00:00Z",
          ended_at: "2024-01-01T00:01:00Z",
          created_at: "2024-01-01T00:00:00Z",
          message_count: 1,
          user_message_count: 1,
        },
      },
    });
    flushSync();

    expect(document.querySelector(".agent-tag")?.textContent).toBe("Claude");
    expect(document.querySelector(".entrypoint-tag")).toBeNull();
  });
});
