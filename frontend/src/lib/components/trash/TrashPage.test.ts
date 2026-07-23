// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import TrashPage from "./TrashPage.svelte";
import type { Session } from "../../api/types.js";
import { SessionsService } from "../../api/generated/index";

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
    configureGeneratedClient: vi.fn(),
  };
});

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    SessionsService: {
      deleteApiV1SessionsIdPermanent: vi.fn(),
      deleteApiV1Trash: vi.fn(),
      getApiV1Trash: vi.fn(),
      postApiV1SessionsIdRestore: vi.fn(),
    },
  };
});

vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: {
    clearRecentlyDeleted: vi.fn(),
    invalidateFilterCaches: vi.fn(),
    load: vi.fn(),
  },
}));

const sessionsService = SessionsService as unknown as {
  deleteApiV1SessionsIdPermanent: ReturnType<typeof vi.fn>;
  deleteApiV1Trash: ReturnType<typeof vi.fn>;
  getApiV1Trash: ReturnType<typeof vi.fn>;
  postApiV1SessionsIdRestore: ReturnType<typeof vi.fn>;
};

function makeTrashedSession(overrides: Partial<Session> = {}): Session {
  return {
    id: "s1",
    project: "alpha",
    machine: "local",
    agent: "claude",
    first_message: "build this",
    display_name: null,
    started_at: "2026-06-14T01:02:03Z",
    ended_at: "2026-06-14T01:03:03Z",
    created_at: "2026-06-14T01:02:03Z",
    deleted_at: "2026-06-14T01:04:03Z",
    message_count: 2,
    user_message_count: 1,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    is_automated: false,
    ...overrides,
  } as Session;
}

function buttonByText(label: string): HTMLButtonElement {
  const button = Array.from(document.querySelectorAll("button"))
    .find((el) => el.textContent?.trim() === label);
  expect(button).toBeTruthy();
  return button as HTMLButtonElement;
}

beforeEach(() => {
  sessionsService.getApiV1Trash
    .mockReset()
    .mockResolvedValue({ sessions: [makeTrashedSession()] });
  sessionsService.deleteApiV1SessionsIdPermanent
    .mockReset()
    .mockResolvedValue(undefined);
  sessionsService.deleteApiV1Trash
    .mockReset()
    .mockResolvedValue({ deleted: 1 });
  sessionsService.postApiV1SessionsIdRestore
    .mockReset()
    .mockResolvedValue(undefined);
});

afterEach(() => {
  document.body.innerHTML = "";
});

describe("TrashPage", () => {
  it("requires confirmation before deleting a session everywhere", async () => {
    const component = mount(TrashPage, { target: document.body });

    await vi.waitFor(() => {
      expect(buttonByText("Delete Everywhere")).toBeTruthy();
    });

    buttonByText("Delete Everywhere").dispatchEvent(
      new MouseEvent("click", { bubbles: true }),
    );
    await tick();

    expect(
      sessionsService.deleteApiV1SessionsIdPermanent,
    ).not.toHaveBeenCalled();
    expect(document.body.textContent).toContain("Delete everywhere?");

    buttonByText("Confirm").dispatchEvent(
      new MouseEvent("click", { bubbles: true }),
    );

    await vi.waitFor(() => {
      expect(
        sessionsService.deleteApiV1SessionsIdPermanent,
      ).toHaveBeenCalledWith({ id: "s1" });
    });

    unmount(component);
  });

  it("keeps empty trash on the local endpoint", async () => {
    const component = mount(TrashPage, { target: document.body });

    await vi.waitFor(() => {
      expect(buttonByText("Empty Local Trash")).toBeTruthy();
    });

    buttonByText("Empty Local Trash").dispatchEvent(
      new MouseEvent("click", { bubbles: true }),
    );

    await vi.waitFor(() => {
      expect(sessionsService.deleteApiV1Trash).toHaveBeenCalled();
    });
    expect(
      sessionsService.deleteApiV1SessionsIdPermanent,
    ).not.toHaveBeenCalled();

    unmount(component);
  });
});
