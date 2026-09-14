// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { mount, unmount } from "svelte";
// @ts-ignore
import AgentRemapRules from "./AgentRemapRules.svelte";
import { SettingsService } from "../../api/generated/index";

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  };
});

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    SettingsService: {
      getApiV1SettingsAgentRemapRules: vi.fn(),
      postApiV1SettingsAgentRemapRules: vi.fn(),
      putApiV1SettingsAgentRemapRulesById: vi.fn(),
      deleteApiV1SettingsAgentRemapRulesById: vi.fn(),
      postApiV1SettingsAgentRemapRulesPreview: vi.fn(),
      postApiV1SettingsAgentRemapRulesApply: vi.fn(),
    },
  };
});

const settingsService = SettingsService as unknown as {
  getApiV1SettingsAgentRemapRules: ReturnType<typeof vi.fn>;
  postApiV1SettingsAgentRemapRules: ReturnType<typeof vi.fn>;
  putApiV1SettingsAgentRemapRulesById: ReturnType<typeof vi.fn>;
  deleteApiV1SettingsAgentRemapRulesById: ReturnType<typeof vi.fn>;
  postApiV1SettingsAgentRemapRulesPreview: ReturnType<typeof vi.fn>;
  postApiV1SettingsAgentRemapRulesApply: ReturnType<typeof vi.fn>;
};

function rule(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    source_agent: "goose",
    model_glob: "ossington-*|rosedale-*|tofino-*",
    id_prefix: "",
    target_agent: "augure-desktop",
    enabled: true,
    created_at: "2026-09-14T00:00:00.000Z",
    updated_at: "2026-09-14T00:00:00.000Z",
    ...overrides,
  };
}

function previewResponse(overrides: Record<string, unknown> = {}) {
  return {
    token: "tok-1",
    matched_sessions: 2,
    per_rule_counts: { "1": 2 },
    samples: [
      {
        id: "goose:abc",
        current_agent: "goose",
        next_agent: "augure-desktop",
        started_at: "2026-09-01T00:00:00.000Z",
      },
    ],
    ...overrides,
  };
}

async function flush() {
  await new Promise((resolve) => setTimeout(resolve, 0));
}

describe("AgentRemapRules", () => {
  let component: Record<string, unknown> | null = null;
  let onMutated: ReturnType<typeof vi.fn<() => void>>;

  function mountRules(props: Record<string, unknown> = {}) {
    return mount(AgentRemapRules, {
      target: document.body,
      props: {
        onMutated,
        ...props,
      },
    });
  }

  beforeEach(() => {
    vi.clearAllMocks();
    settingsService.getApiV1SettingsAgentRemapRules.mockReset();
    settingsService.postApiV1SettingsAgentRemapRulesPreview.mockReset();
    settingsService.postApiV1SettingsAgentRemapRulesApply.mockReset();
    settingsService.postApiV1SettingsAgentRemapRulesPreview.mockResolvedValue(
      previewResponse(),
    );
    settingsService.postApiV1SettingsAgentRemapRulesApply.mockResolvedValue(
      previewResponse({ token: "", samples: [] }),
    );
    onMutated = vi.fn<() => void>();
  });

  afterEach(() => {
    if (component) unmount(component);
    component = null;
    document.body.innerHTML = "";
  });

  it("renders the rules as a table with the six columns", async () => {
    settingsService.getApiV1SettingsAgentRemapRules.mockResolvedValue([rule()]);

    component = mountRules();
    await flush();

    expect(document.querySelector("table")).not.toBeNull();
    const headers = screen
      .getAllByRole("columnheader")
      .map((th) => th.textContent?.trim());
    expect(headers).toEqual([
      "Source agent",
      "Model pattern",
      "ID prefix",
      "Target agent",
      "Enabled",
      "Actions",
    ]);

    const row = document.querySelector("tbody tr");
    expect(row).not.toBeNull();
    expect(row?.textContent).toContain("goose");
    expect(row?.textContent).toContain("ossington-*|rosedale-*|tofino-*");
    expect(row?.textContent).toContain("augure-desktop");
    expect(row?.textContent).toContain("on");
  });

  it("shows the empty state when no rules exist", async () => {
    settingsService.getApiV1SettingsAgentRemapRules.mockResolvedValue([]);

    component = mountRules();
    await flush();

    expect(document.querySelector("table")).toBeNull();
    expect(document.body.textContent).toContain(
      "No agent remap rules defined.",
    );
  });

  it("previews and applies with the preview token", async () => {
    settingsService.getApiV1SettingsAgentRemapRules.mockResolvedValue([rule()]);

    component = mountRules();
    await flush();

    fireEvent.click(screen.getByRole("button", { name: "Preview remap" }));
    await flush();

    expect(
      settingsService.postApiV1SettingsAgentRemapRulesPreview,
    ).toHaveBeenCalled();
    expect(document.body.textContent).toContain(
      "2 session(s) would be remapped.",
    );

    fireEvent.click(screen.getByRole("button", { name: "Apply remap" }));
    await flush();

    expect(
      settingsService.postApiV1SettingsAgentRemapRulesApply,
    ).toHaveBeenCalledWith({ token: "tok-1" });
    expect(onMutated).toHaveBeenCalled();
    expect(document.body.textContent).toContain("Remapped 2 session(s).");
  });

  it("clears the preview when apply conflicts", async () => {
    settingsService.getApiV1SettingsAgentRemapRules.mockResolvedValue([rule()]);
    settingsService.postApiV1SettingsAgentRemapRulesApply.mockRejectedValue(
      new Error("agent remap preview changed"),
    );

    component = mountRules();
    await flush();

    fireEvent.click(screen.getByRole("button", { name: "Preview remap" }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Apply remap" }));
    await flush();

    expect(
      settingsService.postApiV1SettingsAgentRemapRulesApply,
    ).toHaveBeenCalled();
    // The preview panel is dismissed so the user re-previews; the error
    // shown is the server's conflict message.
    expect(
      screen.queryByRole("button", { name: "Apply remap" }),
    ).toBeNull();
    expect(document.body.textContent).toContain(
      "agent remap preview changed",
    );
  });
});
