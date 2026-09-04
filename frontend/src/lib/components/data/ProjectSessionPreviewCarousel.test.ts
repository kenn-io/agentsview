// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
import type { DbWorktreeReclassificationSessionSample } from "../../api/generated/index";

const api = vi.hoisted(() => ({ getSession: vi.fn() }));

vi.mock("../../api/generated/index", () => ({
  SessionsService: { getApiV1SessionsId: api.getSession },
}));
vi.mock("../../api/runtime.js", () => ({
  callGenerated: (request: () => Promise<unknown>) => request(),
  isAbortError: () => false,
}));

import ProjectSessionPreviewCarousel from "./ProjectSessionPreviewCarousel.svelte";
import { m } from "../../i18n/index.js";

const samples: DbWorktreeReclassificationSessionSample[] = [
  {
    id: "session-1",
    cwd: "/worktrees/project-a/branch-one",
    current_project: "project-a-old",
    next_project: "project-a",
  },
  {
    id: "session-2",
    cwd: "/worktrees/project-a/branch-two",
    current_project: "project-b-old",
    next_project: "project-a",
  },
];

let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  api.getSession.mockReset();
  api.getSession.mockImplementation(({ id }: { id: string }) =>
    Promise.resolve({
      id,
      agent: id === "session-1" ? "claude" : "codex",
      created_at: "2026-08-20T12:00:00Z",
      started_at: "2026-08-20T12:00:00Z",
      ended_at: "2026-08-20T12:05:00Z",
      first_message: id === "session-1" ? "Fix the first project" : "Review the second project",
      message_count: 4,
    }),
  );
});

afterEach(() => {
  if (component) void unmount(component);
  component = undefined;
  document.body.innerHTML = "";
});

async function flush() {
  await tick();
  await Promise.resolve();
  await tick();
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("ProjectSessionPreviewCarousel", () => {
  it("loads session content lazily and moves through the preview samples", async () => {
    component = mount(ProjectSessionPreviewCarousel, {
      target: document.body,
      props: { samples },
    });
    await tick();

    expect(api.getSession).not.toHaveBeenCalled();

    await fireEvent.click(
      screen.getByRole("button", {
        name: m.data_reclassify_session_preview_count({ count: 2 }),
      }),
    );
    await flush();

    expect(api.getSession).toHaveBeenNthCalledWith(1, { id: "session-1" });
    expect(screen.getByText("Fix the first project")).toBeTruthy();
    expect(screen.getByText("project-a-old")).toBeTruthy();
    expect(screen.getByText("project-a")).toBeTruthy();

    await fireEvent.click(
      screen.getByRole("button", { name: m.data_reclassify_session_preview_next() }),
    );
    await flush();

    expect(api.getSession).toHaveBeenNthCalledWith(2, { id: "session-2" });
    expect(screen.getByText("Review the second project")).toBeTruthy();
    expect(screen.getByText("project-b-old")).toBeTruthy();
    expect(screen.getByText("/worktrees/project-a/branch-two")).toBeTruthy();
  });

  it("ignores a late failure after returning to cached session content", async () => {
    const second = deferred<never>();
    api.getSession.mockImplementation(({ id }: { id: string }) => {
      if (id === "session-2") return second.promise;
      return Promise.resolve({
        id,
        agent: "claude",
        created_at: "2026-08-20T12:00:00Z",
        first_message: "Cached first session",
        message_count: 4,
      });
    });
    component = mount(ProjectSessionPreviewCarousel, {
      target: document.body,
      props: { samples },
    });
    await fireEvent.click(
      screen.getByRole("button", {
        name: m.data_reclassify_session_preview_count({ count: 2 }),
      }),
    );
    await flush();
    await fireEvent.click(
      screen.getByRole("button", { name: m.data_reclassify_session_preview_next() }),
    );
    await fireEvent.click(
      screen.getByRole("button", { name: m.data_reclassify_session_preview_previous() }),
    );
    second.reject(new Error("late failure"));
    await flush();

    expect(screen.getByText("Cached first session")).toBeTruthy();
    expect(screen.queryByText(m.data_reclassify_session_preview_failed())).toBeNull();
  });
});
