// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { screen, waitFor } from "@testing-library/svelte";
import { mount, unmount } from "svelte";
// @ts-ignore
import DuplicateSessionReview from "./DuplicateSessionReview.svelte";
import {
  getApiV1SettingsDuplicateGroups,
  postApiV1SettingsDuplicateGroupsRebuild,
} from "../../api/generated/settings/settings.js";

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  };
});

vi.mock("../../api/generated/settings/settings.js", async () => {
  return {
    getApiV1SettingsDuplicateGroups: vi.fn(),
    postApiV1SettingsDuplicateGroupsRebuild: vi.fn(),
  };
});

const settingsService = (await import(
  "../../api/generated/settings/settings.js",
)) as unknown as {
  getApiV1SettingsDuplicateGroups: ReturnType<typeof vi.fn>;
  postApiV1SettingsDuplicateGroupsRebuild: ReturnType<typeof vi.fn>;
};

vi.mock("../../stores/router.svelte.js", () => ({
  router: {
    buildSessionHref: (id: string) => `#/session/${id}`,
  },
}));

const en = (await import("../../../../messages/en.json")).default as Record<
  string,
  unknown
>;

function textOf(key: string): string {
  const value = en[key];
  return typeof value === "string" ? value : key;
}

function group(overrides: Record<string, unknown> = {}) {
  return {
    group_key: "key-1",
    members: [
      {
        session_id: "goose:20260101_10",
        group_key: "key-1",
        role: "canonical",
        canonical_id: "",
        member_count: 2,
        agent: "goose",
        started_at: "2026-01-01T10:00:00.000Z",
        message_count: 5,
      },
      {
        session_id: "augure-desktop:20260101_10",
        group_key: "key-1",
        role: "duplicate",
        canonical_id: "goose:20260101_10",
        member_count: 2,
        agent: "augure-desktop",
        started_at: "2026-01-01T10:00:00.000Z",
        message_count: 4,
      },
    ],
    ...overrides,
  };
}

let mounted: Record<string, unknown> = {};

async function render(props: Record<string, unknown> = {}) {
  const instance = mount(DuplicateSessionReview, {
    target: document.body,
    props,
  });
  mounted.instance = instance;
  await waitFor(() => {
    expect(
      settingsService.getApiV1SettingsDuplicateGroups,
    ).toHaveBeenCalled();
  });
  await waitFor(() => {
    // Settled: either groups render or the empty state appears.
    expect(
      screen.queryByText(textOf("duplicate_review_rebuilding")) === null ||
        (settingsService.postApiV1SettingsDuplicateGroupsRebuild as ReturnType<typeof vi.fn>)
          .mock.calls.length === 0,
    ).toBe(true);
  });
}

afterEach(async () => {
  if (mounted.instance) {
    await unmount(mounted.instance as never);
    mounted = {};
  }
  vi.clearAllMocks();
});

describe("DuplicateSessionReview", () => {
  beforeEach(() => {
    settingsService.getApiV1SettingsDuplicateGroups.mockResolvedValue([]);
    settingsService.postApiV1SettingsDuplicateGroupsRebuild.mockResolvedValue({
      groups: 0,
      members: 0,
      notified_ids: 0,
    });
  });

  it("renders the empty state when no groups exist", async () => {
    await render();
    await waitFor(() => {
      expect(screen.getByText(textOf("duplicate_review_empty"))).toBeTruthy();
    });
  });

  it("lists groups with member counts and roles", async () => {
    settingsService.getApiV1SettingsDuplicateGroups.mockResolvedValue([
      group(),
    ]);
    await render();
    await waitFor(() => {
      expect(screen.getByText("2 copies")).toBeTruthy();
    });
    expect(screen.getAllByText(
      textOf("duplicate_review_role_canonical"),
    ).length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText(
      textOf("duplicate_review_role_duplicate"),
    ).length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("goose:20260101_10")).toBeTruthy();
    expect(screen.getByText("augure-desktop:20260101_10")).toBeTruthy();
  });

  it("recheck triggers a rebuild and reloads", async () => {
    settingsService.getApiV1SettingsDuplicateGroups.mockResolvedValue([
      group(),
    ]);
    settingsService.postApiV1SettingsDuplicateGroupsRebuild.mockResolvedValue({
      groups: 1,
      members: 2,
      notified_ids: 2,
    });
    await render();
    const button = await screen.findByText(textOf("duplicate_review_recheck"));
    (button as HTMLButtonElement).click();
    await waitFor(() => {
      expect(
        settingsService.postApiV1SettingsDuplicateGroupsRebuild,
      ).toHaveBeenCalled();
    });
    await waitFor(() => {
      expect(
        settingsService.getApiV1SettingsDuplicateGroups.mock.calls.length,
      ).toBeGreaterThanOrEqual(2);
    });
  });

  it("hides the recheck control in read-only mode", async () => {
    await render({ readOnly: true });
    await waitFor(() => {
      expect(screen.getByText(textOf("duplicate_review_empty"))).toBeTruthy();
    });
    expect(
      screen.queryByText(textOf("duplicate_review_recheck")),
    ).toBeNull();
  });
});
