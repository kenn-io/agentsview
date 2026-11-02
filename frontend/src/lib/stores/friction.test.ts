import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { FrictionDigestResponse } from "../api/generated/index.js";

vi.mock("../api/runtime.js", () => ({
  isAbortError: vi.fn(() => false),
  isNotFoundError: (e: unknown) => (e as { status?: number } | null)?.status === 404,
}));

vi.mock("../api/generated/index", () => ({
  FrictionService: {
    getApiV1FrictionDigests: vi.fn(),
    getApiV1FrictionDigestsByDate: vi.fn(),
    getApiV1FrictionDigestsByDateMd: vi.fn(),
    postApiV1FrictionRun: vi.fn(),
    postApiV1FrictionPatternsByFingerprintIssue: vi.fn(),
    putApiV1FrictionPatternsByFingerprintIssue: vi.fn(),
    deleteApiV1FrictionPatternsByFingerprintIssue: vi.fn(),
  },
  KataService: { getApiV1KataStatus: vi.fn() },
}));

import { FrictionService, KataService } from "../api/generated/index";
import { friction } from "./friction.svelte.js";

const service = FrictionService as unknown as {
  getApiV1FrictionDigests: ReturnType<typeof vi.fn>;
  getApiV1FrictionDigestsByDate: ReturnType<typeof vi.fn>;
  getApiV1FrictionDigestsByDateMd: ReturnType<typeof vi.fn>;
  postApiV1FrictionRun: ReturnType<typeof vi.fn>;
  postApiV1FrictionPatternsByFingerprintIssue: ReturnType<typeof vi.fn>;
  putApiV1FrictionPatternsByFingerprintIssue: ReturnType<typeof vi.fn>;
  deleteApiV1FrictionPatternsByFingerprintIssue: ReturnType<typeof vi.fn>;
};
const kataService = KataService as unknown as { getApiV1KataStatus: ReturnType<typeof vi.fn> };

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
    summary: {
      date,
      schema_version: 1,
      sessions_scanned: 2,
      corrections: 0,
      errors: 0,
      workarounds: 0,
      deferrals: 0,
      patterns: 0,
      frustrations: 0,
      interruptions: 0,
      p0_alerts: {},
    },
    signals: [],
    p0_alerts: [],
    patterns: [],
  } as unknown as FrictionDigestResponse;
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
    expect(friction.listLoads).toBe(1);
  });

  it("opens the requested date when a digest exists for it", async () => {
    await friction.load(OLDER);
    expect(friction.selectedDate).toBe(OLDER);
    expect(friction.olderDate).toBe(OLDEST);
    expect(friction.newerDate).toBe(LATEST);
  });

  it("keeps a date picked while the date list was loading", async () => {
    await friction.load(LATEST);
    const list = Promise.withResolvers<{ digests: ReturnType<typeof item>[] }>();
    service.getApiV1FrictionDigests.mockReturnValueOnce(list.promise);
    const refresh = friction.load(LATEST);
    await friction.selectDate(OLDEST);
    list.resolve({ digests: [item(OLDER), item(LATEST), item(OLDEST)] });
    await refresh;
    expect(friction.selectedDate).toBe(OLDEST);
    expect(friction.digest?.date).toBe(OLDEST);
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
    expect(friction.listLoads).toBe(0);
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
    await friction.loadMarkdown(LATEST);
    expect(friction.markdown).toBe("---\ndate: 2026-09-21\n---\n\n# Friction Log — 2026-09-21\n");
    expect(friction.errors.markdown).toBeNull();
  });

  it("reads the Markdown of the digest on screen while another date loads", async () => {
    await friction.load();
    const next = Promise.withResolvers<FrictionDigestResponse>();
    service.getApiV1FrictionDigestsByDate.mockReturnValueOnce(next.promise);
    const selecting = friction.selectDate(OLDER);
    service.getApiV1FrictionDigestsByDateMd.mockResolvedValueOnce(new Response("# latest"));
    await friction.loadMarkdown(LATEST);
    expect(service.getApiV1FrictionDigestsByDateMd).toHaveBeenLastCalledWith(
      { date: LATEST },
      expect.anything(),
    );
    expect(friction.markdown).toBe("# latest");
    next.resolve(makeDigest(OLDER));
    await selecting;
    expect(friction.digest?.date).toBe(OLDER);
    expect(friction.markdown).toBeNull();
  });

  it("sets a localized error when the Markdown read fails", async () => {
    await friction.load();
    service.getApiV1FrictionDigestsByDateMd.mockRejectedValueOnce(new Error("boom"));
    await friction.loadMarkdown(LATEST);
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
    expect(friction.listLoads).toBe(2);
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

describe("FrictionStore Kata issues", () => {
  const FP = `fl1:${"a".repeat(64)}`;
  const OTHER = `fl1:${"b".repeat(64)}`;
  const issue = {
    qualified_id: "agentsview#f001",
    web_url: "https://kata.example.test/issues/f001",
    issue_uid: "01J0ABCDEF0000000000000001",
    source: "created",
    linked_at: "2026-09-22T08:00:00Z",
  };

  beforeEach(() => {
    friction.digest = {
      ...makeDigest(LATEST),
      patterns: [
        { fingerprint: FP, kind: "error" },
        { fingerprint: OTHER, kind: "error" },
      ],
    } as unknown as FrictionDigestResponse;
  });

  function issueOf(fp: string) {
    return friction.digest?.patterns?.find((p) => p.fingerprint === fp)?.issue;
  }

  it("shows a filed issue on its pattern without reloading the digest", async () => {
    service.postApiV1FrictionPatternsByFingerprintIssue.mockResolvedValue({ issue, created: true });
    await friction.fileIssue(FP);
    expect(service.postApiV1FrictionPatternsByFingerprintIssue).toHaveBeenCalledWith(
      { fingerprint: FP },
      {},
    );
    expect(issueOf(FP)).toEqual(issue);
    expect(issueOf(OTHER)).toBeUndefined();
    expect(service.getApiV1FrictionDigestsByDate).not.toHaveBeenCalled();
    expect(friction.issueBusy[FP]).toBeUndefined();
  });

  it("keeps an action that finishes while an older digest read is in flight", async () => {
    const stale = {
      ...makeDigest(LATEST),
      patterns: [
        { fingerprint: FP, kind: "error" },
        { fingerprint: OTHER, kind: "error", issue },
      ],
    } as unknown as FrictionDigestResponse;
    const read = Promise.withResolvers<FrictionDigestResponse>();
    service.getApiV1FrictionDigestsByDate.mockReturnValueOnce(read.promise);
    const selecting = friction.selectDate(LATEST);
    service.postApiV1FrictionPatternsByFingerprintIssue.mockResolvedValue({ issue, created: true });
    await friction.fileIssue(FP);
    service.deleteApiV1FrictionPatternsByFingerprintIssue.mockResolvedValue(undefined);
    await friction.unlinkIssue(OTHER);

    read.resolve(stale);
    await selecting;
    expect(issueOf(FP), "the filed issue survives the older response").toEqual(issue);
    expect(issueOf(OTHER), "the removed link stays removed").toBeUndefined();

    service.getApiV1FrictionDigestsByDate.mockResolvedValueOnce(stale);
    await friction.selectDate(LATEST);
    expect(issueOf(OTHER), "a read started after the actions is the server's answer").toEqual(
      issue,
    );
  });

  it("links by reference and unlinks", async () => {
    service.putApiV1FrictionPatternsByFingerprintIssue.mockResolvedValue({ issue });
    await friction.linkIssue(FP, "agentsview#f001");
    expect(service.putApiV1FrictionPatternsByFingerprintIssue).toHaveBeenCalledWith(
      { fingerprint: FP },
      { issue: "agentsview#f001" },
    );
    expect(issueOf(FP)).toEqual(issue);

    service.deleteApiV1FrictionPatternsByFingerprintIssue.mockResolvedValue(undefined);
    await friction.unlinkIssue(FP);
    expect(issueOf(FP)).toBeUndefined();
  });

  it("keeps a failure with its pattern and clears it on the next try", async () => {
    service.postApiV1FrictionPatternsByFingerprintIssue.mockRejectedValueOnce(
      new Error("Kata request failed: kata: HTTP 503"),
    );
    await friction.fileIssue(FP);
    expect(friction.issueErrors[FP]).toBe("Kata request failed: kata: HTTP 503");
    expect(friction.issueErrors[OTHER]).toBeUndefined();
    expect(issueOf(FP)).toBeUndefined();

    service.postApiV1FrictionPatternsByFingerprintIssue.mockResolvedValueOnce({
      issue,
      created: false,
    });
    await friction.fileIssue(FP);
    expect(friction.issueErrors[FP]).toBeUndefined();
    expect(issueOf(FP)).toEqual(issue);
  });

  it.each([
    ["ready", { state: "ready" }, true],
    ["not the hub", { state: "not_hub" }, false],
  ])("reads Kata readiness: %s", async (_name, status, want) => {
    kataService.getApiV1KataStatus.mockResolvedValue({ project: "agentsview", ...status });
    await friction.checkKata();
    expect(friction.kataReady).toBe(want);
  });

  it("treats a failed status check as not ready", async () => {
    kataService.getApiV1KataStatus.mockRejectedValue(new Error("offline"));
    await friction.checkKata();
    expect(friction.kataReady).toBe(false);
  });
});
