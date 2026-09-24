import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { FrictionDigestResponse, FrictionPatternItem } from "../api/generated/index.js";

vi.mock("../api/runtime.js", () => ({
  isAbortError: vi.fn(() => false),
  isNotFoundError: (e: unknown) => (e as { status?: number } | null)?.status === 404,
}));

vi.mock("../api/generated/index", () => ({
  FrictionService: {
    getApiV1FrictionDigests: vi.fn(),
    getApiV1FrictionDigestsByDate: vi.fn(),
    getApiV1FrictionDigestsByDateMd: vi.fn(),
    getApiV1FrictionPatterns: vi.fn(),
    postApiV1FrictionRun: vi.fn(),
    postApiV1FrictionPatternsByFingerprintFile: vi.fn(),
    putApiV1FrictionPatternsByFingerprintLink: vi.fn(),
    deleteApiV1FrictionPatternsByFingerprintLink: vi.fn(),
  },
}));

import { FrictionService } from "../api/generated/index";
import { friction, PATTERN_MAX_PAGES, PATTERN_PAGE_LIMIT } from "./friction.svelte.js";

const service = FrictionService as unknown as {
  getApiV1FrictionDigests: ReturnType<typeof vi.fn>;
  getApiV1FrictionDigestsByDate: ReturnType<typeof vi.fn>;
  getApiV1FrictionDigestsByDateMd: ReturnType<typeof vi.fn>;
  getApiV1FrictionPatterns: ReturnType<typeof vi.fn>;
  postApiV1FrictionRun: ReturnType<typeof vi.fn>;
  postApiV1FrictionPatternsByFingerprintFile: ReturnType<typeof vi.fn>;
  putApiV1FrictionPatternsByFingerprintLink: ReturnType<typeof vi.fn>;
  deleteApiV1FrictionPatternsByFingerprintLink: ReturnType<typeof vi.fn>;
};

const LATEST = "2026-09-21";
const OLDER = "2026-09-20";
const OLDEST = "2026-09-19";

function item(date: string) {
  return {
    date,
    timezone: "UTC",
    rules_version: "friction-v1",
    built_at: `${date}T23:59:00Z`,
    revision: 1,
    sessions_scanned: 2,
  };
}

function makeDigest(date: string): FrictionDigestResponse {
  return {
    date,
    timezone: "UTC",
    rules_version: "friction-v1",
    built_at: `${date}T23:59:00Z`,
    revision: 1,
    sessions_scanned: 2,
    markdown_sha256: "0".repeat(64),
    web_url: "",
    summary: {
      schema_version: 3,
      sessions_scanned: 2,
      tracker_failures: 0,
      corrections: 0,
      errors: 0,
      workarounds: 0,
      deferrals: 0,
      patterns: 0,
      frustrations: 0,
      interruptions: 0,
      p0_alerts: {},
      spend: null,
      digest_path: `friction:${date}`,
      created_issues: [],
    },
    signals: [],
    p0_alerts: [],
  } as unknown as FrictionDigestResponse;
}

function patternItem(fingerprint: string): FrictionPatternItem {
  return {
    fingerprint,
    kind: "error",
    title: `[friction/error] bash: ${fingerprint}`,
    first_seen_date: OLDER,
    last_seen_date: LATEST,
    occurrence_count: 2,
    session_count: 2,
    last_subject_id: "session-a",
    last_ordinal: null,
  } as FrictionPatternItem;
}

beforeEach(() => {
  friction.reset();
  vi.clearAllMocks();
  // API order is deliberately not newest-first.
  service.getApiV1FrictionDigests.mockResolvedValue({
    digests: [item(OLDER), item(LATEST), item(OLDEST)],
  });
  service.getApiV1FrictionDigestsByDate.mockImplementation(({ date }: { date: string }) =>
    Promise.resolve(makeDigest(date)),
  );
  service.getApiV1FrictionPatterns.mockResolvedValue({ patterns: [], next_cursor: "" });
});

describe("FrictionStore.load", () => {
  it("sorts dates newest first and opens the latest digest by default", async () => {
    await friction.load();
    expect(friction.dates.map((d) => d.date)).toEqual([LATEST, OLDER, OLDEST]);
    expect(friction.selectedDate).toBe(LATEST);
    expect(friction.digest?.date).toBe(LATEST);
    expect(friction.olderDate).toBe(OLDER);
    expect(friction.newerDate).toBeNull();
    expect(friction.loading.dates).toBe(false);
    expect(friction.lastUpdatedAt).not.toBeNull();
  });

  it("opens the requested date when a digest exists for it", async () => {
    await friction.load(OLDER);
    expect(friction.selectedDate).toBe(OLDER);
    expect(friction.olderDate).toBe(OLDEST);
    expect(friction.newerDate).toBe(LATEST);
  });

  it("falls back to the latest digest for a stale date bookmark", async () => {
    await friction.load("2025-01-01");
    expect(friction.selectedDate).toBe(LATEST);
    expect(friction.errors.digest).toBeNull();
    expect(service.getApiV1FrictionDigestsByDate).toHaveBeenCalledWith(
      { date: LATEST },
      expect.anything(),
    );
  });

  it("clears the selection when no digests exist", async () => {
    service.getApiV1FrictionDigests.mockResolvedValueOnce({ digests: [] });
    await friction.load();
    expect(friction.selectedDate).toBeNull();
    expect(friction.digest).toBeNull();
    expect(service.getApiV1FrictionDigestsByDate).not.toHaveBeenCalled();
  });

  it("reports a list failure without throwing (read-only backends answer with errors)", async () => {
    service.getApiV1FrictionDigests.mockRejectedValueOnce(
      Object.assign(new Error("not implemented for read-only store"), { status: 501 }),
    );
    await expect(friction.load()).resolves.toBeUndefined();
    expect(friction.errors.dates).toBe("not implemented for read-only store");
    expect(friction.loading.dates).toBe(false);
  });
});

describe("FrictionStore.selectDate", () => {
  it("keeps the newest selection when an older read resolves late", async () => {
    let resolveFirst: (value: FrictionDigestResponse) => void = () => {};
    service.getApiV1FrictionDigestsByDate
      .mockImplementationOnce(
        () =>
          new Promise<FrictionDigestResponse>((resolve) => {
            resolveFirst = resolve;
          }),
      )
      .mockImplementationOnce(({ date }: { date: string }) => Promise.resolve(makeDigest(date)));

    const first = friction.selectDate(LATEST);
    await friction.selectDate(OLDER);
    resolveFirst(makeDigest(LATEST));
    await first;

    expect(friction.selectedDate).toBe(OLDER);
    expect(friction.digest?.date).toBe(OLDER);
    expect(service.getApiV1FrictionDigestsByDate.mock.calls[0]?.[1]?.signal?.aborted).toBe(true);
    expect(friction.loading.digest).toBe(false);
  });

  it("shows a friendly message when the digest is gone", async () => {
    service.getApiV1FrictionDigestsByDate.mockRejectedValueOnce(
      Object.assign(new Error("not found"), { status: 404 }),
    );
    await friction.selectDate(OLDER);
    expect(friction.digest).toBeNull();
    expect(friction.errors.digest).toBe(`No digest exists for ${OLDER}.`);
  });

  it("pages patterns from the digest date until the cursor is empty", async () => {
    service.getApiV1FrictionPatterns
      .mockResolvedValueOnce({ patterns: [patternItem("fl1:a")], next_cursor: "c1" })
      .mockResolvedValueOnce({ patterns: [patternItem("fl1:b")], next_cursor: "" });
    await friction.selectDate(LATEST);
    expect(friction.patterns.map((p) => p.fingerprint)).toEqual(["fl1:a", "fl1:b"]);
    expect(service.getApiV1FrictionPatterns.mock.calls.map((c) => c[0])).toEqual([
      { since: LATEST, limit: PATTERN_PAGE_LIMIT },
      { since: LATEST, limit: PATTERN_PAGE_LIMIT, cursor: "c1" },
    ]);
  });

  it("stops paging patterns at the page cap", async () => {
    service.getApiV1FrictionPatterns.mockResolvedValue({
      patterns: [patternItem("fl1:x")],
      next_cursor: "more",
    });
    await friction.selectDate(LATEST);
    expect(service.getApiV1FrictionPatterns).toHaveBeenCalledTimes(PATTERN_MAX_PAGES);
  });

  it("drops Markdown loaded for the previous date", async () => {
    friction.markdown = "# old";
    await friction.selectDate(OLDER);
    expect(friction.markdown).toBeNull();
  });
});

describe("FrictionStore.loadMarkdown", () => {
  it("stores the raw Markdown bytes of the selected digest", async () => {
    await friction.load();
    service.getApiV1FrictionDigestsByDateMd.mockResolvedValueOnce(
      new Response("---\ndate: 2026-09-21\n---\n\n# Friction Log — 2026-09-21\n"),
    );
    await friction.loadMarkdown();
    expect(friction.markdown).toBe("---\ndate: 2026-09-21\n---\n\n# Friction Log — 2026-09-21\n");
    expect(friction.errors.markdown).toBeNull();
  });

  it("sets a localized error when the Markdown read fails", async () => {
    await friction.load();
    service.getApiV1FrictionDigestsByDateMd.mockRejectedValueOnce(new Error("boom"));
    await friction.loadMarkdown();
    expect(friction.markdown).toBeNull();
    expect(friction.errors.markdown).toBe("Could not load the Markdown digest.");
  });
});

describe("FrictionStore.buildNow", () => {
  it("counts written digests and reopens the latest", async () => {
    await friction.load(OLDER);
    service.postApiV1FrictionRun.mockResolvedValueOnce({
      reports: [
        { date: OLDEST, written: false, summary: {}, human: "" },
        { date: LATEST, written: true, summary: {}, human: "" },
      ],
    });
    await friction.buildNow();
    expect(service.postApiV1FrictionRun).toHaveBeenCalledWith({});
    expect(friction.lastBuildWritten).toBe(1);
    expect(friction.selectedDate).toBe(LATEST);
    expect(friction.building).toBe(false);
  });

  it("keeps the current date when nothing was built", async () => {
    await friction.load(OLDER);
    service.postApiV1FrictionRun.mockResolvedValueOnce({ reports: [] });
    await friction.buildNow();
    expect(friction.lastBuildWritten).toBe(0);
    expect(friction.selectedDate).toBe(OLDER);
  });

  it("reports a failed build and re-enables the button", async () => {
    service.postApiV1FrictionRun.mockRejectedValueOnce(new Error("friction review is not enabled"));
    await friction.buildNow();
    expect(friction.errors.build).toBe("friction review is not enabled");
    expect(friction.building).toBe(false);
  });
});

describe("FrictionStore Kata pattern actions", () => {
  it("files a pattern and refreshes the selected digest", async () => {
    service.getApiV1FrictionPatterns.mockResolvedValueOnce({ patterns: [patternItem("fl1:a")] });
    await friction.selectDate(LATEST);
    service.postApiV1FrictionPatternsByFingerprintFile.mockResolvedValueOnce({ status: "created" });
    service.getApiV1FrictionPatterns.mockResolvedValueOnce({
      patterns: [{ ...patternItem("fl1:a"), link: { state: "open", qualified_id: "project#abc" } }],
    });

    await friction.filePattern("fl1:a");

    expect(service.postApiV1FrictionPatternsByFingerprintFile).toHaveBeenCalledWith(
      { fingerprint: "fl1:a" },
      {},
    );
    expect(service.getApiV1FrictionDigestsByDate).toHaveBeenCalledTimes(2);
    expect(friction.patterns[0]?.link?.qualified_id).toBe("project#abc");
    expect(friction.mutationFingerprint).toBeNull();
  });

  it("trims an existing issue ref before linking and refreshes", async () => {
    await friction.selectDate(LATEST);
    service.putApiV1FrictionPatternsByFingerprintLink.mockResolvedValueOnce({});
    await friction.linkPattern("fl1:b", "  project#abc  ");
    expect(service.putApiV1FrictionPatternsByFingerprintLink).toHaveBeenCalledWith(
      { fingerprint: "fl1:b" },
      { issue_ref: "project#abc" },
    );
    expect(service.getApiV1FrictionDigestsByDate).toHaveBeenCalledTimes(2);
  });

  it("unlinks the local mapping and refreshes", async () => {
    await friction.selectDate(LATEST);
    service.deleteApiV1FrictionPatternsByFingerprintLink.mockResolvedValueOnce({ removed: true });
    await friction.unlinkPattern("fl1:c");
    expect(service.deleteApiV1FrictionPatternsByFingerprintLink).toHaveBeenCalledWith({
      fingerprint: "fl1:c",
    });
    expect(service.getApiV1FrictionDigestsByDate).toHaveBeenCalledTimes(2);
  });

  it("keeps the row on a rejected request, shows the error, and allows a retry", async () => {
    service.getApiV1FrictionPatterns.mockResolvedValue({ patterns: [patternItem("fl1:a")] });
    await friction.selectDate(LATEST);
    service.postApiV1FrictionPatternsByFingerprintFile.mockRejectedValueOnce(
      new Error("Kata offline"),
    );
    await friction.filePattern("fl1:a");
    expect(friction.patterns[0]?.fingerprint).toBe("fl1:a");
    expect(friction.mutationError).toEqual({ fingerprint: "fl1:a", message: "Kata offline" });
    expect(service.getApiV1FrictionDigestsByDate).toHaveBeenCalledTimes(1);
    service.postApiV1FrictionPatternsByFingerprintFile.mockResolvedValueOnce({});
    await friction.filePattern("fl1:a");
    expect(friction.mutationError).toBeNull();
  });

  it("ignores a second action while a mutation is in flight", async () => {
    await friction.selectDate(LATEST);
    let release: () => void = () => {};
    service.postApiV1FrictionPatternsByFingerprintFile.mockImplementationOnce(
      () =>
        new Promise<void>((resolve) => {
          release = resolve;
        }),
    );
    const pending = friction.filePattern("fl1:a");
    await friction.unlinkPattern("fl1:b");
    expect(service.deleteApiV1FrictionPatternsByFingerprintLink).not.toHaveBeenCalled();
    release();
    await pending;
  });
});
