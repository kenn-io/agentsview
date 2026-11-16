// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, unmount } from "svelte";
import { dismissFlash, getFlashMessage } from "@kenn-io/kit-ui";
import { setLocale } from "../../i18n/index.js";

const mocks = vi.hoisted(() => ({
  getApiV1Trash: vi.fn(),
  postApiV1SessionsByIdRestore: vi.fn(),
}));

vi.mock("../../api/generated/index", () => ({
  SessionsService: {
    getApiV1Trash: mocks.getApiV1Trash,
    postApiV1SessionsByIdRestore: mocks.postApiV1SessionsByIdRestore,
    deleteApiV1SessionsByIdPermanent: vi.fn(),
    deleteApiV1Trash: vi.fn(),
  },
}));

vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: {
    clearRecentlyDeleted: vi.fn(),
    invalidateFilterCaches: vi.fn(),
    load: vi.fn(),
  },
}));

// @ts-ignore
import TrashPage from "./TrashPage.svelte";

const trashed = {
  id: "s1",
  project: "agentsview",
  agent: "claude",
  display_name: "Fix the sync watcher",
  first_message: null,
  user_message_count: 4,
  deleted_at: "2026-06-24T12:00:00Z",
};

function alert(): Promise<Element> {
  return vi.waitFor(() => {
    const el = document.querySelector('[role="alert"]');
    expect(el).not.toBeNull();
    return el!;
  });
}

describe("TrashPage", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    setLocale("en");
  });

  afterEach(async () => {
    if (component) await unmount(component);
    component = undefined;
    dismissFlash();
    document.body.innerHTML = "";
  });

  it("shows a failed load with Retry instead of an empty trash", async () => {
    mocks.getApiV1Trash
      .mockRejectedValueOnce(new Error("archive is busy"))
      .mockResolvedValueOnce({ sessions: [trashed] });

    component = mount(TrashPage, { target: document.body });
    const failure = await alert();

    expect(failure.textContent).toContain("Failed to load");
    expect(failure.textContent).toContain("archive is busy");
    expect(document.body.textContent).not.toContain("Trash is empty");

    failure.querySelector<HTMLButtonElement>("button")!.click();
    await vi.waitFor(() => expect(document.body.textContent).toContain("Fix the sync watcher"));
    expect(document.querySelector('[role="alert"]')).toBeNull();
    expect(mocks.getApiV1Trash).toHaveBeenCalledTimes(2);
  });

  it("reports a failed restore and keeps the session listed", async () => {
    mocks.getApiV1Trash.mockResolvedValueOnce({ sessions: [trashed] });
    mocks.postApiV1SessionsByIdRestore.mockRejectedValueOnce(new Error("session is locked"));

    component = mount(TrashPage, { target: document.body });
    await vi.waitFor(() => expect(document.querySelector(".restore-btn")).not.toBeNull());
    document.querySelector<HTMLButtonElement>(".restore-btn")!.click();

    await vi.waitFor(() => expect(getFlashMessage()).toBe("session is locked"));
    expect(document.body.textContent).toContain("Fix the sync watcher");
  });
});
