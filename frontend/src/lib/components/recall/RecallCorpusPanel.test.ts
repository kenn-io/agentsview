// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";
import { router } from "../../stores/router.svelte.js";

// @ts-ignore
import RecallCorpusPanel from "./RecallCorpusPanel.svelte";

describe("RecallCorpusPanel", () => {
  let component: ReturnType<typeof mount> | undefined;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    setLocale("en");
    fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/recall/extraction/status")) {
        return new Response(
          JSON.stringify({
            configured: true,
            management_available: false,
            progress_available: true,
            fingerprint: "generation-active",
            source_runs: ["generation-active", "generation-retired", "reconcile-only"],
            generations: [
              {
                fingerprint: "generation-active",
                state: "active",
                model: "model-a",
                segmenter: "turns-v1",
                created_at: "2026-07-23T10:00:00Z",
                updated_at: "2026-07-23T11:00:00Z",
              },
              {
                fingerprint: "generation-building",
                state: "building",
                model: "model-b",
                segmenter: "turns-v1",
                created_at: "2026-07-23T09:00:00Z",
                updated_at: "2026-07-23T09:30:00Z",
              },
              {
                fingerprint: "generation-retired",
                state: "retired",
                model: "model-c",
                segmenter: "turns-v1",
                created_at: "2026-07-22T09:00:00Z",
                updated_at: "2026-07-22T09:30:00Z",
              },
            ],
            stats: {
              pending: 2,
              partial: 1,
              done: 8,
              failed: 1,
              units_done: 18,
              units_total: 20,
              entries: 12,
            },
            eligible_backlog: 3,
          }),
          {
            status: 200,
            headers: { "Content-Type": "application/json" },
          },
        );
      }
      if (url.includes("/recall/extraction/progress")) {
        const failedOnly = url.includes("state=failed");
        return new Response(
          JSON.stringify({
            generation_fingerprint: "generation-active",
            progress: [
              ...(failedOnly
                ? []
                : [
                    {
                      session_id: "session-pending",
                      generation_fingerprint: "generation-active",
                      state: "pending",
                      unit_cursor: 0,
                      units_total: 3,
                      updated_at: "2026-07-23T11:45:00Z",
                      session_title: "Trace the pending extraction",
                      project: "agentsview",
                      agent: "codex",
                    },
                  ]),
              {
                session_id: "session-failed",
                generation_fingerprint: "generation-active",
                state: "failed",
                unit_cursor: 1,
                units_total: 4,
                last_error: "Model response was empty",
                updated_at: "2026-07-23T11:30:00Z",
                session_title: "Diagnose the failed extraction",
                project: "agentsview",
                agent: "codex",
                retry_at: "2026-07-23T12:30:00Z",
                retry_eligible: true,
              },
            ],
          }),
          {
            status: 200,
            headers: { "Content-Type": "application/json" },
          },
        );
      }
      if (url.includes("/recall/entries?")) {
        if (url.includes("cursor=cursor-2")) {
          return new Response(
            JSON.stringify({
              entries: [
                {
                  id: "recall-2",
                  type: "procedure",
                  scope: "project",
                  status: "accepted",
                  review_state: "human_reviewed",
                  title: "Review the next Recall page",
                  body: "Cursor pagination keeps later entries reachable.",
                  project: "agentsview",
                  source_session_id: "session-2",
                  source_run_id: "generation-active",
                  extractor_method: "turns-v1",
                  transferable: false,
                  provenance_ok: true,
                  created_at: "2026-07-23T09:00:00Z",
                  updated_at: "2026-07-23T10:00:00Z",
                  evidence: [],
                },
              ],
              trusted_only: false,
            }),
            {
              status: 200,
              headers: { "Content-Type": "application/json" },
            },
          );
        }
        return new Response(
          JSON.stringify({
            entries: [
              {
                id: "recall-1",
                type: "decision",
                scope: "project",
                status: "accepted",
                review_state: "unreviewed_auto",
                title: "Keep extraction passes bounded",
                body: "Limit model-backed passes to an explicit session count.",
                trigger: "When proving extraction against a real archive.",
                uncertainty: "Eligibility may reduce the selected session count.",
                project: "agentsview",
                source_session_id: "session-1",
                source_run_id: "generation-active",
                extractor_method: "turns-v1",
                transferable: false,
                provenance_ok: true,
                created_at: "2026-07-23T10:00:00Z",
                updated_at: "2026-07-23T11:00:00Z",
                evidence: [
                  {
                    id: 1,
                    entry_id: "recall-1",
                    session_id: "session-1",
                    message_start_ordinal: 12,
                    message_end_ordinal: 14,
                  },
                ],
              },
            ],
            trusted_only: false,
            next_cursor: "cursor-2",
            result_cap: url.includes("q=") ? 500 : undefined,
          }),
          {
            status: 200,
            headers: { "Content-Type": "application/json" },
          },
        );
      }
      return new Response("not found", { status: 404 });
    });
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    document.body.innerHTML = "";
  });

  it("shows corpus entries and extraction coverage", async () => {
    component = mount(RecallCorpusPanel, { target: document.body });
    await tick();

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("Keep extraction passes bounded");
    });
    expect(document.querySelector('table[aria-label="Recall entries"]')).not.toBeNull();
    expect(document.body.textContent).not.toContain(
      "Limit model-backed passes to an explicit session count.",
    );
    const expand = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Expand Keep extraction passes bounded"]',
    );
    expect(expand).not.toBeNull();
    expect(expand!.getAttribute("aria-expanded")).toBe("false");

    expand!.click();
    await tick();

    expect(document.body.textContent).toContain(
      "Limit model-backed passes to an explicit session count.",
    );
    expect(document.body.textContent).toContain("When proving extraction against a real archive.");
    expect(document.body.textContent).toContain(
      "Eligibility may reduce the selected session count.",
    );
    expect(document.body.textContent).toContain("Messages 12–14");
    expect(expand!.getAttribute("aria-expanded")).toBe("true");
    expect(expand!.getAttribute("aria-label")).toBe("Collapse Keep extraction passes bounded");
    expect(document.body.textContent).toContain("Extraction status");
    expect(document.body.textContent).toContain("8 done");
    expect(document.body.textContent).toContain("1 failed");
    expect(document.body.textContent).toContain("3 eligible");
    expect(document.body.textContent).toContain("active");
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining("/recall/entries?limit=200"),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining("/recall/extraction/status"),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
  });

  it("refreshes entries and extraction coverage while the page stays open", async () => {
    const defaultFetch = fetchMock as unknown as (input: RequestInfo | URL) => Promise<Response>;
    let entryRequests = 0;
    let statusRequests = 0;
    fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/recall/entries?")) {
        entryRequests++;
        if (entryRequests > 1) {
          return new Response(
            JSON.stringify({
              entries: [
                {
                  id: "recall-new",
                  type: "fact",
                  scope: "project",
                  status: "accepted",
                  review_state: "unreviewed_auto",
                  title: "Newly distilled Recall entry",
                  body: "This entry became available after the page opened.",
                  source_session_id: "session-new",
                  source_run_id: "generation-active",
                  extractor_method: "turns-v1",
                  transferable: false,
                  provenance_ok: true,
                  created_at: "2026-07-23T12:00:00Z",
                  updated_at: "2026-07-23T12:00:00Z",
                  evidence: [],
                },
              ],
              trusted_only: false,
            }),
            {
              status: 200,
              headers: { "Content-Type": "application/json" },
            },
          );
        }
      }
      if (url.includes("/recall/extraction/status")) {
        statusRequests++;
        if (statusRequests > 1) {
          return new Response(
            JSON.stringify({
              configured: true,
              fingerprint: "generation-active",
              source_runs: ["generation-active"],
              generations: [],
              stats: {
                pending: 1,
                partial: 0,
                done: 9,
                failed: 1,
                units_done: 20,
                units_total: 20,
                entries: 13,
              },
              eligible_backlog: 0,
            }),
            {
              status: 200,
              headers: { "Content-Type": "application/json" },
            },
          );
        }
      }
      return defaultFetch(input);
    });
    vi.stubGlobal("fetch", fetchMock);
    component = mount(RecallCorpusPanel, { target: document.body });
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("Keep extraction passes bounded");
    });

    const refresh = document.querySelector<HTMLButtonElement>('button[aria-label="Refresh"]');
    expect(refresh).not.toBeNull();
    refresh!.click();

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("Newly distilled Recall entry");
      expect(document.body.textContent).toContain("9 done");
      expect(document.body.textContent).toContain("13 entries");
    });
    expect(entryRequests).toBe(2);
    expect(statusRequests).toBe(2);
  });

  it("drills into actionable extraction progress and source sessions", async () => {
    const navigate = vi.spyOn(router, "navigateToSession").mockImplementation(() => {});
    component = mount(RecallCorpusPanel, { target: document.body });
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("8 done");
    });

    const showProgress = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Show progress",
    );
    expect(showProgress).toBeDefined();
    showProgress!.click();

    await vi.waitFor(() => {
      expect(document.querySelector('table[aria-label="Extraction sessions"]')).not.toBeNull();
      expect(document.body.textContent).toContain("Trace the pending extraction");
      expect(document.body.textContent).toContain("Model response was empty");
      expect(
        Array.from(document.querySelectorAll(".progress-units")).map((cell) =>
          cell.textContent?.replace(/\s+/g, " ").trim(),
        ),
      ).toContain("1 / 4");
      expect(document.body.textContent).toContain("Retry ready");
    });
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining("/recall/extraction/progress?limit=50&generation=generation-active"),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );

    const sessionLink = Array.from(document.querySelectorAll<HTMLAnchorElement>("a")).find(
      (link) => link.textContent?.trim() === "Diagnose the failed extraction",
    );
    expect(sessionLink).toBeDefined();
    sessionLink!.dispatchEvent(
      new MouseEvent("click", {
        bubbles: true,
        cancelable: true,
        button: 0,
      }),
    );
    expect(navigate).toHaveBeenCalledWith("session-failed");

    const failedFilter = Array.from(
      document.querySelectorAll<HTMLButtonElement>('button[role="radio"]'),
    ).find((button) => button.textContent?.trim() === "Failed");
    expect(failedFilter).toBeDefined();
    failedFilter!.click();
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        expect.stringContaining("state=failed"),
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      );
      expect(document.body.textContent).not.toContain("Trace the pending extraction");
    });
  });

  it("returns refresh to idle when pending progress is hidden", async () => {
    const defaultFetch = fetchMock as unknown as (
      input: RequestInfo | URL,
      init?: RequestInit,
    ) => Promise<Response>;
    fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).includes("/recall/extraction/progress")) {
        return await new Promise<Response>((_resolve, reject) => {
          init?.signal?.addEventListener(
            "abort",
            () => {
              reject(new DOMException("Aborted", "AbortError"));
            },
            { once: true },
          );
        });
      }
      return defaultFetch(input, init);
    });
    vi.stubGlobal("fetch", fetchMock);

    component = mount(RecallCorpusPanel, { target: document.body });
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("8 done");
    });

    const refresh = document.querySelector<HTMLButtonElement>('button[aria-label="Refresh"]');
    const showProgress = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Show progress",
    );
    expect(refresh).not.toBeNull();
    expect(showProgress).toBeDefined();

    showProgress!.click();
    await vi.waitFor(() => {
      expect(refresh!.disabled).toBe(true);
    });

    const hideProgress = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Hide progress",
    );
    expect(hideProgress).toBeDefined();
    hideProgress!.click();
    await tick();

    expect(refresh!.disabled).toBe(false);
  });

  it("hides extraction progress when the backend does not support it", async () => {
    const defaultFetch = fetchMock as unknown as (input: RequestInfo | URL) => Promise<Response>;
    fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/recall/extraction/status")) {
        return new Response(
          JSON.stringify({
            configured: false,
            progress_available: false,
            source_runs: [],
          }),
          {
            status: 200,
            headers: { "Content-Type": "application/json" },
          },
        );
      }
      return defaultFetch(input);
    });
    vi.stubGlobal("fetch", fetchMock);

    component = mount(RecallCorpusPanel, { target: document.body });
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("Keep extraction passes bounded");
    });

    expect(
      Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
        (button) => button.textContent?.trim() === "Show progress",
      ),
    ).toBeUndefined();
    expect(fetchMock).not.toHaveBeenCalledWith(
      expect.stringContaining("/recall/extraction/progress"),
      expect.anything(),
    );
  });

  it("activates a configured retired generation and retires an abandoned build", async () => {
    const defaultFetch = fetchMock as unknown as (
      input: RequestInfo | URL,
      init?: RequestInit,
    ) => Promise<Response>;
    fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes("/recall/extraction/status")) {
        return new Response(
          JSON.stringify({
            configured: true,
            management_available: true,
            progress_available: true,
            fingerprint: "generation-building",
            source_runs: ["generation-active"],
            generations: [
              {
                fingerprint: "generation-active",
                state: "active",
                model: "model-a",
                segmenter: "turns-v1",
                created_at: "2026-07-23T10:00:00Z",
                updated_at: "2026-07-23T11:00:00Z",
              },
              {
                fingerprint: "generation-building",
                state: "retired",
                model: "model-b",
                segmenter: "turns-v1",
                created_at: "2026-07-24T10:00:00Z",
                updated_at: "2026-07-24T11:00:00Z",
              },
              {
                fingerprint: "generation-abandoned",
                state: "building",
                model: "model-c",
                segmenter: "turns-v1",
                created_at: "2026-07-22T10:00:00Z",
                updated_at: "2026-07-22T11:00:00Z",
              },
            ],
            stats: {
              pending: 0,
              partial: 0,
              done: 8,
              failed: 0,
              units_done: 20,
              units_total: 20,
              entries: 12,
            },
            eligible_backlog: 0,
          }),
          {
            status: 200,
            headers: { "Content-Type": "application/json" },
          },
        );
      }
      if (init?.method === "POST") {
        return new Response(null, { status: 204 });
      }
      return defaultFetch(input, init);
    });
    vi.stubGlobal("fetch", fetchMock);

    component = mount(RecallCorpusPanel, { target: document.body });
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("model-b");
    });

    const activate = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Activate",
    );
    expect(activate).toBeDefined();
    activate!.click();
    await tick();

    const confirmActivate = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Activate generation",
    );
    expect(confirmActivate).toBeDefined();
    confirmActivate!.click();
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/v1/recall/extraction/activate",
        expect.objectContaining({ method: "POST" }),
      );
      expect(
        Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
          (button) => button.textContent?.trim() === "Activate generation",
        ),
      ).toBeUndefined();
    });

    const retire = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Retire",
    );
    expect(retire).toBeDefined();
    retire!.click();
    await tick();

    const confirmRetire = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Retire generation",
    );
    expect(confirmRetire).toBeDefined();
    confirmRetire!.click();
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/v1/recall/extraction/generations/generation-abandoned/retire",
        expect.objectContaining({ method: "POST" }),
      );
    });
  });

  it("loads the next cursor page and removes the truncation action", async () => {
    component = mount(RecallCorpusPanel, { target: document.body });

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("Keep extraction passes bounded");
    });
    const loadMore = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Load more",
    );
    expect(loadMore).toBeDefined();

    loadMore!.click();

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("Review the next Recall page");
    });
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining("cursor=cursor-2"),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(
      Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
        (button) => button.textContent?.trim() === "Load more",
      ),
    ).toBeUndefined();
  });

  it("shows the ranked result cap for searched entries", async () => {
    component = mount(RecallCorpusPanel, { target: document.body });
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("Keep extraction passes bounded");
    });
    const search = document.querySelector<HTMLInputElement>(
      'input[placeholder="Search Recall entries…"]',
    );
    expect(search).not.toBeNull();

    search!.value = "bounded";
    search!.dispatchEvent(new Event("input", { bubbles: true }));

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain(
        "Ranked search is limited to the first 500 matches.",
      );
    });
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining("q=bounded"),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
  });

  it("restarts ranked pagination when the corpus changes", async () => {
    const defaultFetch = fetchMock as unknown as (input: RequestInfo | URL) => Promise<Response>;
    let initialPageRequests = 0;
    fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/recall/entries?") && url.includes("cursor=cursor-2")) {
        return new Response(
          JSON.stringify({
            error: "recall corpus changed; restart pagination",
          }),
          {
            status: 409,
            headers: { "Content-Type": "application/json" },
          },
        );
      }
      if (url.includes("/recall/entries?")) {
        initialPageRequests++;
        if (initialPageRequests > 1) {
          return new Response(
            JSON.stringify({
              entries: [
                {
                  id: "recall-refreshed",
                  type: "decision",
                  scope: "project",
                  status: "accepted",
                  review_state: "unreviewed_auto",
                  title: "Refreshed Recall results",
                  body: "The browser restarted from the changed corpus.",
                  source_session_id: "session-refreshed",
                  transferable: false,
                  provenance_ok: true,
                  created_at: "2026-07-23T12:00:00Z",
                  updated_at: "2026-07-23T12:00:00Z",
                  evidence: [],
                },
              ],
              trusted_only: false,
            }),
            {
              status: 200,
              headers: { "Content-Type": "application/json" },
            },
          );
        }
      }
      return defaultFetch(input);
    });
    vi.stubGlobal("fetch", fetchMock);
    component = mount(RecallCorpusPanel, { target: document.body });
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("Keep extraction passes bounded");
    });

    const loadMore = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Load more",
    );
    expect(loadMore).toBeDefined();
    loadMore!.click();

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("Refreshed Recall results");
    });
    expect(initialPageRequests).toBe(2);
    expect(document.body.textContent).not.toContain("Keep extraction passes bounded");
  });

  it("offers every source run represented in the served corpus", async () => {
    component = mount(RecallCorpusPanel, { target: document.body });

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("active");
    });
    const generationFilter = document.querySelector<HTMLButtonElement>(
      'button[title="Extraction generation"]',
    );
    expect(generationFilter).not.toBeNull();
    generationFilter!.click();
    await tick();

    expect(document.body.textContent).toContain("generation-active");
    expect(document.body.textContent).not.toContain("generation-building");
    expect(document.body.textContent).toContain("generation-retired");
    expect(document.body.textContent).toContain("reconcile-only");
  });
});
