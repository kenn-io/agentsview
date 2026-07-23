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
import { setLocale } from "../../i18n/index.js";

// @ts-ignore
import RecallPage from "./RecallPage.svelte";

describe("RecallPage", () => {
  let component: ReturnType<typeof mount> | undefined;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    setLocale("en");
    fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/recall/extraction/status")) {
        return new Response(JSON.stringify({
          configured: true,
          fingerprint: "generation-active",
          generations: [{
            fingerprint: "generation-active",
            state: "active",
            model: "model-a",
            segmenter: "turns-v1",
            created_at: "2026-07-23T10:00:00Z",
            updated_at: "2026-07-23T11:00:00Z",
          }, {
            fingerprint: "generation-building",
            state: "building",
            model: "model-b",
            segmenter: "turns-v1",
            created_at: "2026-07-23T09:00:00Z",
            updated_at: "2026-07-23T09:30:00Z",
          }, {
            fingerprint: "generation-retired",
            state: "retired",
            model: "model-c",
            segmenter: "turns-v1",
            created_at: "2026-07-22T09:00:00Z",
            updated_at: "2026-07-22T09:30:00Z",
          }],
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
        }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.includes("/recall/entries?")) {
        if (url.includes("cursor=cursor-2")) {
          return new Response(JSON.stringify({
            entries: [{
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
            }],
            trusted_only: false,
          }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          });
        }
        return new Response(JSON.stringify({
          entries: [{
            id: "recall-1",
            type: "decision",
            scope: "project",
            status: "accepted",
            review_state: "unreviewed_auto",
            title: "Keep extraction passes bounded",
            body: "Limit model-backed passes to an explicit session count.",
            project: "agentsview",
            source_session_id: "session-1",
            source_run_id: "generation-active",
            extractor_method: "turns-v1",
            transferable: false,
            provenance_ok: true,
            created_at: "2026-07-23T10:00:00Z",
            updated_at: "2026-07-23T11:00:00Z",
            evidence: [],
          }],
          trusted_only: false,
          next_cursor: "cursor-2",
        }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
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
    document.body.innerHTML = "";
  });

  it("shows corpus entries and extraction coverage", async () => {
    component = mount(RecallPage, { target: document.body });
    await tick();

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain(
        "Keep extraction passes bounded",
      );
    });
    expect(document.body.textContent).toContain(
      "Limit model-backed passes to an explicit session count.",
    );
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

  it("loads the next cursor page and removes the truncation action", async () => {
    component = mount(RecallPage, { target: document.body });

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain(
        "Keep extraction passes bounded",
      );
    });
    const loadMore = Array.from(
      document.querySelectorAll<HTMLButtonElement>("button"),
    ).find((button) => button.textContent?.trim() === "Load more");
    expect(loadMore).toBeDefined();

    loadMore!.click();

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain(
        "Review the next Recall page",
      );
    });
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining("cursor=cursor-2"),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(Array.from(
      document.querySelectorAll<HTMLButtonElement>("button"),
    ).find((button) => button.textContent?.trim() === "Load more"))
      .toBeUndefined();
  });

  it("offers only the active extraction generation as a served filter", async () => {
    component = mount(RecallPage, { target: document.body });

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
    expect(document.body.textContent).not.toContain("generation-retired");
  });
});
