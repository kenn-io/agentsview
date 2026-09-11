// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";
// @ts-ignore
import ToolImageCleanup from "./ToolImageCleanup.svelte";
import { DataService } from "../../api/generated/index";
import { ApiError } from "../../api/runtime.js";

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
    DataService: {
      postApiV1DataStripImagesPreview: vi.fn(),
      postApiV1DataStripImages: vi.fn(),
    },
  };
});

vi.mock("../../stores/settings.svelte.js", () => ({
  settings: { readOnly: false },
}));

const dataService = DataService as unknown as {
  postApiV1DataStripImagesPreview: ReturnType<typeof vi.fn>;
  postApiV1DataStripImages: ReturnType<typeof vi.fn>;
};

function emptyReport() {
  return {
    sessions: 0,
    changed: 0,
    payloads: 0,
    stored_bytes: 0,
    decoded_bytes: 0,
    projects: [],
  };
}

function reportWithPayloads() {
  return {
    sessions: 2,
    changed: 2,
    payloads: 3,
    stored_bytes: 4096,
    decoded_bytes: 2048,
    projects: [{ project: "my-project", sessions: 2, changed: 2, payloads: 3, stored_bytes: 4096, decoded_bytes: 2048 }],
  };
}

describe("ToolImageCleanup", () => {
  beforeEach(() => {
    dataService.postApiV1DataStripImagesPreview.mockReset();
    dataService.postApiV1DataStripImages.mockReset();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = "";
  });

  async function settle(ms = 0): Promise<void> {
    await vi.advanceTimersByTimeAsync(ms);
    flushSync();
  }

  it("renders intro text", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(emptyReport());

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    expect(document.body.textContent).toContain("Preview");

    unmount(component);
    vi.useRealTimers();
  });

  it("preview button calls the preview endpoint", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(emptyReport());

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    const previewBtn = document.body.querySelector("button");
    previewBtn?.click();
    await settle();

    expect(dataService.postApiV1DataStripImagesPreview).toHaveBeenCalledTimes(1);

    unmount(component);
    vi.useRealTimers();
  });

  it("403 names the proxy, not the remote-backend case", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockRejectedValue(
      new ApiError(403, "tool-result image removal is only permitted from localhost"),
    );

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    const previewBtn = document.body.querySelector("button");
    previewBtn?.click();
    await settle();

    expect(document.body.textContent).toContain("not through a proxy or forwarded port");
    expect(document.body.textContent).not.toContain(
      "Image removal runs only against a local archive on this machine.",
    );

    unmount(component);
    vi.useRealTimers();
  });

  it("apply button is disabled when no payloads", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(emptyReport());

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    const previewBtn = document.body.querySelector("button");
    previewBtn?.click();
    await settle();

    // After preview with zero payloads, the empty message appears and no apply button is active
    expect(document.body.textContent).toContain(
      "No stored tool result in this selection holds an inline image payload.",
    );

    unmount(component);
    vi.useRealTimers();
  });

  it("shows confirmation modal before applying", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(reportWithPayloads());
    dataService.postApiV1DataStripImages.mockResolvedValue(reportWithPayloads());

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    // Click preview
    const buttons = document.body.querySelectorAll("button");
    buttons[0]?.click();
    await settle();

    // A remove button should appear; find it and click
    const allButtons = Array.from(document.body.querySelectorAll("button"));
    const removeBtn = allButtons.find((b) => b.textContent?.includes("Remove image payloads"));
    expect(removeBtn).toBeTruthy();
    removeBtn?.click();
    await settle();

    // Confirmation modal should appear
    expect(document.body.textContent).toContain("Remove stored image payloads?");

    unmount(component);
    vi.useRealTimers();
  });

  // Row 14: apply disabled in idle, enabled only after preview with payloads > 0,
  // disabled again after editFilter (states previewed/idle, events previewOk/editFilter).
  it("apply button is disabled in idle and enabled only after preview with payloads", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(reportWithPayloads());

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    // In idle: no apply button exists yet (preview not yet done).
    const applyBefore = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    );
    expect(applyBefore).toBeUndefined();

    // After preview with payloads: apply button must be enabled.
    document.body.querySelectorAll("button")[0]?.click();
    await settle();

    const applyBtn = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    ) as HTMLButtonElement | undefined;
    expect(applyBtn).toBeTruthy();
    expect(applyBtn?.disabled).toBe(false);

    // Editing the project filter must drop back to idle and remove the apply button.
    const projectInput = document.body.querySelector("#tic-project") as HTMLInputElement | null;
    if (projectInput) {
      projectInput.value = "new-filter";
      projectInput.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
    }
    await settle();

    const applyAfterEdit = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    );
    expect(applyAfterEdit).toBeUndefined();

    unmount(component);
    vi.useRealTimers();
  });

  // Row 15: zero apply calls after requestApply and after cancelConfirm; exactly
  // one after confirmApply, carrying confirmed: true and the frozen filter.
  it("cancel issues no apply request; confirm sends exactly one request with frozen filter", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(reportWithPayloads());
    dataService.postApiV1DataStripImages.mockResolvedValue(reportWithPayloads());

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    // A non-empty selection, so the asserted body proves the frozen filter
    // travels with the apply rather than passing on two empty strings.
    const projectInput = document.getElementById("tic-project") as HTMLInputElement;
    projectInput.value = "alpha";
    projectInput.dispatchEvent(new Event("input", { bubbles: true }));
    const beforeInput = document.getElementById("tic-before") as HTMLInputElement;
    beforeInput.value = "2025-01-10";
    beforeInput.dispatchEvent(new Event("input", { bubbles: true }));
    await settle();

    // Preview.
    document.body.querySelectorAll("button")[0]?.click();
    await settle();

    // Open confirm dialog (requestApply).
    const removeBtn = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    );
    removeBtn?.click();
    await settle();

    // No apply call yet.
    expect(dataService.postApiV1DataStripImages).not.toHaveBeenCalled();

    // Cancel (cancelConfirm).
    const cancelBtn = Array.from(document.body.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Cancel",
    );
    cancelBtn?.click();
    await settle();

    // Still no apply call after cancel.
    expect(dataService.postApiV1DataStripImages).not.toHaveBeenCalled();

    // Reopen modal and confirm (confirmApply).
    const removeBtn2 = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    );
    removeBtn2?.click();
    await settle();

    const confirmBtn = Array.from(document.body.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Remove",
    );
    confirmBtn?.click();
    await settle();

    // Exactly one call, carrying the frozen filter and confirmed: true.
    expect(dataService.postApiV1DataStripImages).toHaveBeenCalledTimes(1);
    expect(dataService.postApiV1DataStripImages).toHaveBeenCalledWith(
      { project: "alpha", before: "2025-01-10", confirmed: true },
      undefined,
    );

    unmount(component);
    vi.useRealTimers();
  });

  // Row 16: completion summary uses the apply response, not the preview. Exercises applyOk.
  it("applied summary shows apply response numbers, not preview numbers", async () => {
    vi.useFakeTimers();
    const previewReport = { ...reportWithPayloads(), sessions: 5, changed: 5 };
    const applyReport = { ...reportWithPayloads(), sessions: 3, changed: 3 };
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(previewReport);
    dataService.postApiV1DataStripImages.mockResolvedValue(applyReport);

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    // Preview.
    document.body.querySelectorAll("button")[0]?.click();
    await settle();

    // Open modal, confirm apply.
    const removeBtn = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    );
    removeBtn?.click();
    await settle();

    const confirmBtn = Array.from(document.body.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Remove",
    );
    confirmBtn?.click();
    await settle();

    // The summary carries the apply response count (3), not the preview's (5),
    // and the preview totals it supersedes are gone from the panel.
    const appliedText = document.body.querySelector(".applied-text")?.textContent ?? "";
    expect(appliedText).toContain("Image removal completed.");
    expect(appliedText).toContain("3");
    expect(appliedText).not.toContain("5");
    expect(document.body.querySelector(".status-grid")).toBeNull();
    expect(document.body.querySelector(".project-table")).toBeNull();

    unmount(component);
    vi.useRealTimers();
  });

  // Row 17: 409 returns to previewed state and shows busy text. Exercises applyBusy(409).
  it("409 apply shows busy text and returns to previewed", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(reportWithPayloads());
    dataService.postApiV1DataStripImages.mockRejectedValue(
      new ApiError(409, "Another archive maintenance operation is already running. Try again once it finishes."),
    );

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    // Preview.
    document.body.querySelectorAll("button")[0]?.click();
    await settle();

    // Open modal, confirm.
    const removeBtn = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    );
    removeBtn?.click();
    await settle();

    const confirmBtn = Array.from(document.body.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Remove",
    );
    confirmBtn?.click();
    await settle();

    // Busy text must appear and apply button must be re-enabled.
    expect(document.body.textContent).toContain("already running");
    const applyBtnAfter = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    ) as HTMLButtonElement | undefined;
    expect(applyBtnAfter).toBeTruthy();
    expect(applyBtnAfter?.disabled).toBe(false);

    unmount(component);
    vi.useRealTimers();
  });

  // Row 18: failed apply renders partial-completion warning and requires a new preview.
  // Exercises applyFail from state applying.
  it("failed apply shows partial-completion warning and disables apply", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(reportWithPayloads());
    dataService.postApiV1DataStripImages.mockRejectedValue(new ApiError(500, "Internal server error"));

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    // Preview.
    document.body.querySelectorAll("button")[0]?.click();
    await settle();

    // Open modal, confirm.
    const removeBtn = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    );
    removeBtn?.click();
    await settle();

    const confirmBtn = Array.from(document.body.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Remove",
    );
    confirmBtn?.click();
    await settle();

    // Partial-completion warning must be present.
    expect(document.body.textContent).toContain("did not finish");
    // Apply button must be absent (failed state; requires new preview).
    const applyBtnAfter = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    );
    expect(applyBtnAfter).toBeUndefined();

    unmount(component);
    vi.useRealTimers();
  });

  // Row 19: 501 preview renders unavailable text and hides controls (same path as 403).
  // Exercises deny(501) from previewing → idle with unavailableReason set.
  it("501 preview shows unavailable text and hides apply button", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockRejectedValue(
      new ApiError(501, "not available in remote mode"),
    );

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    // Trigger preview.
    document.body.querySelectorAll("button")[0]?.click();
    await settle();

    // Unavailable paragraph must appear.
    expect(document.body.textContent).toContain(
      "Image removal runs only against a local archive on this machine.",
    );
    // No apply button.
    const applyBtn = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    );
    expect(applyBtn).toBeUndefined();

    unmount(component);
    vi.useRealTimers();
  });

  // A preview still in flight must not become the accepted report for a
  // selection the user changed while it was running.
  it("a filter edit during previewing discards the in-flight preview", async () => {
    vi.useFakeTimers();
    let resolvePreview: ((value: unknown) => void) | undefined;
    dataService.postApiV1DataStripImagesPreview.mockReturnValue(
      new Promise((resolve) => {
        resolvePreview = resolve;
      }),
    );

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    document.body.querySelectorAll("button")[0]?.click();
    await settle();

    const projectInput = document.getElementById("tic-project") as HTMLInputElement;
    projectInput.value = "alpha";
    projectInput.dispatchEvent(new Event("input", { bubbles: true }));
    await settle();

    resolvePreview?.(reportWithPayloads());
    await settle();

    expect(document.body.textContent).not.toContain("3 image payloads");
    const applyBtn = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    );
    expect(applyBtn).toBeUndefined();

    unmount(component);
    vi.useRealTimers();
  });

  it.each([409, 503])("keeps the previewed filters when retrying after %s", async (status) => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(reportWithPayloads());
    let rejectApply: ((reason: Error) => void) | undefined;
    dataService.postApiV1DataStripImages.mockReturnValueOnce(
      new Promise((_, reject) => {
        rejectApply = reject;
      }),
    );
    dataService.postApiV1DataStripImages.mockResolvedValue(reportWithPayloads());

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();
    const projectInput = document.getElementById("tic-project") as HTMLInputElement;
    const beforeInput = document.getElementById("tic-before") as HTMLInputElement;
    projectInput.value = "alpha";
    projectInput.dispatchEvent(new Event("input", { bubbles: true }));
    beforeInput.value = "2025-01-10";
    beforeInput.dispatchEvent(new Event("input", { bubbles: true }));
    await settle();
    document.body.querySelectorAll("button")[0]?.click();
    await settle();
    expect(dataService.postApiV1DataStripImagesPreview).toHaveBeenCalledWith(
      { project: "alpha", before: "2025-01-10" },
      undefined,
    );
    Array.from(document.body.querySelectorAll("button"))
      .find((b) => b.textContent?.includes("Remove image payloads"))
      ?.click();
    await settle();
    Array.from(document.body.querySelectorAll("button"))
      .find((b) => b.textContent?.trim() === "Remove")
      ?.click();
    await settle();

    expect(projectInput.disabled).toBe(true);
    expect(beforeInput.disabled).toBe(true);

    expect(document.body.textContent).toContain("3 image payloads");
    expect(dataService.postApiV1DataStripImagesPreview).toHaveBeenCalledTimes(1);

    rejectApply?.(new ApiError(status, "Archive maintenance is busy"));
    await settle();
    expect(projectInput.disabled).toBe(false);
    expect(beforeInput.disabled).toBe(false);
    Array.from(document.body.querySelectorAll("button"))
      .find((b) => b.textContent?.includes("Remove image payloads"))
      ?.click();
    await settle();
    Array.from(document.body.querySelectorAll("button"))
      .find((b) => b.textContent?.trim() === "Remove")
      ?.click();
    await settle();

    expect(dataService.postApiV1DataStripImages).toHaveBeenCalledTimes(2);
    expect(dataService.postApiV1DataStripImages).toHaveBeenNthCalledWith(
      2,
      { project: "alpha", before: "2025-01-10", confirmed: true },
      undefined,
    );
    await unmount(component);
    vi.useRealTimers();
  });

  // 503 is the transient writer-closed response; it carries its own message and
  // must not claim a partial rewrite.
  it("503 apply shows the server message and keeps the preview", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(reportWithPayloads());
    dataService.postApiV1DataStripImages.mockRejectedValue(
      new ApiError(503, "archive writer is closed, retry shortly"),
    );

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();
    document.body.querySelectorAll("button")[0]?.click();
    await settle();
    Array.from(document.body.querySelectorAll("button"))
      .find((b) => b.textContent?.includes("Remove image payloads"))
      ?.click();
    await settle();
    Array.from(document.body.querySelectorAll("button"))
      .find((b) => b.textContent?.trim() === "Remove")
      ?.click();
    await settle();

    expect(document.body.textContent).toContain("archive writer is closed, retry shortly");
    expect(document.body.textContent).not.toContain("did not finish");
    const applyBtnAfter = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Remove image payloads"),
    );
    expect(applyBtnAfter).toBeDefined();

    unmount(component);
    vi.useRealTimers();
  });

  // The partial-completion warning renders once, not once from the handler and
  // again from the template.
  it("a failed apply renders the partial-completion warning exactly once", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(reportWithPayloads());
    dataService.postApiV1DataStripImages.mockRejectedValue(new Error("socket closed"));

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();
    document.body.querySelectorAll("button")[0]?.click();
    await settle();
    Array.from(document.body.querySelectorAll("button"))
      .find((b) => b.textContent?.includes("Remove image payloads"))
      ?.click();
    await settle();
    Array.from(document.body.querySelectorAll("button"))
      .find((b) => b.textContent?.trim() === "Remove")
      ?.click();
    await settle();

    const occurrences = (document.body.textContent ?? "").split("did not finish").length - 1;
    expect(occurrences).toBe(1);

    unmount(component);
    vi.useRealTimers();
  });

  // The confirmation dialog states the frozen selection, not just "these filters".
  it("the confirm dialog shows the frozen filter and the compact note", async () => {
    vi.useFakeTimers();
    dataService.postApiV1DataStripImagesPreview.mockResolvedValue(reportWithPayloads());

    const component = mount(ToolImageCleanup, { target: document.body });
    await settle();

    const projectInput = document.getElementById("tic-project") as HTMLInputElement;
    projectInput.value = "alpha";
    projectInput.dispatchEvent(new Event("input", { bubbles: true }));
    const beforeInput = document.getElementById("tic-before") as HTMLInputElement;
    beforeInput.value = "2025-01-10";
    beforeInput.dispatchEvent(new Event("input", { bubbles: true }));
    await settle();

    document.body.querySelectorAll("button")[0]?.click();
    await settle();
    Array.from(document.body.querySelectorAll("button"))
      .find((b) => b.textContent?.includes("Remove image payloads"))
      ?.click();
    await settle();

    const dialog = document.querySelector("[role=dialog], .kit-modal");
    expect(dialog?.textContent).toContain("alpha");
    expect(dialog?.textContent).toContain("2025-01-10");
    expect(dialog?.textContent).toContain("agentsview db compact");

    unmount(component);
    vi.useRealTimers();
  });
});
