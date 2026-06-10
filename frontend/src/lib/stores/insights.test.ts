import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
} from "vite-plus/test";
import { insights } from "./insights.svelte.js";
import type { Insight } from "../api/types.js";

const api = vi.hoisted(() => {
  class MockApiError extends Error {
    constructor(
      public readonly status: number,
      message: string,
    ) {
      super(message);
      this.name = "ApiError";
    }
  }
  return {
    listInsights: vi.fn(),
    deleteInsight: vi.fn(),
    generateInsight: vi.fn(),
    ApiError: MockApiError,
  };
});

const ApiError = api.ApiError;

vi.mock("../api/client.js", () => ({
  generateInsight: api.generateInsight,
}));

vi.mock("../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
}));

vi.mock("../api/generated/index", () => ({
  ApiError: api.ApiError,
  InsightsService: {
    getApiV1Insights: vi.fn(() => api.listInsights()),
    deleteApiV1InsightsId: vi.fn(({ id }) => api.deleteInsight(id)),
  },
}));

function makeInsight(
  overrides: Partial<Insight> = {},
): Insight {
  return {
    id: 1,
    type: "daily_activity",
    date_from: "2025-01-15",
    date_to: "2025-01-15",
    project: null,
    agent: "claude",
    model: "claude-sonnet-4-20250514",
    prompt: null,
    content: "# Summary\nThings happened.",
    created_at: "2025-01-15T12:00:00.000Z",
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  insights.items = [];
  insights.selectedId = null;
  insights.selectedTaskId = null;
  insights.loading = false;
  insights.tasks = [];
  insights.promptText = "";
});

describe("load", () => {
  it("fetches insights and updates state", async () => {
    const s1 = makeInsight({ id: 1 });
    const s2 = makeInsight({ id: 2, project: "my-app" });
    vi.mocked(api.listInsights).mockResolvedValueOnce({
      insights: [s2, s1],
    });

    await insights.load();

    expect(api.listInsights).toHaveBeenCalledWith();
    expect(insights.items).toHaveLength(2);
    expect(insights.loading).toBe(false);
  });

  it("clears selectedId when insight no longer in list", async () => {
    insights.selectedId = 99;
    vi.mocked(api.listInsights).mockResolvedValueOnce({
      insights: [makeInsight({ id: 1 })],
    });

    await insights.load();

    expect(insights.selectedId).toBeNull();
  });

  it("preserves selectedId when insight is in list", async () => {
    insights.selectedId = 1;
    vi.mocked(api.listInsights).mockResolvedValueOnce({
      insights: [makeInsight({ id: 1 })],
    });

    await insights.load();

    expect(insights.selectedId).toBe(1);
  });
});

describe("setDateFrom / setDateTo", () => {
  it("updates dateFrom without reloading", () => {
    insights.setDateFrom("2025-02-01");

    expect(insights.dateFrom).toBe("2025-02-01");
    expect(api.listInsights).not.toHaveBeenCalled();
  });

  it("updates dateTo without reloading", () => {
    insights.setDateTo("2025-02-07");

    expect(insights.dateTo).toBe("2025-02-07");
    expect(api.listInsights).not.toHaveBeenCalled();
  });
});

describe("setType", () => {
  it("updates type without reloading", () => {
    insights.setType("agent_analysis");

    expect(insights.type).toBe("agent_analysis");
    expect(api.listInsights).not.toHaveBeenCalled();
  });
});

describe("date range mode switching", () => {
  it("syncs dateTo = dateFrom for single-day mode", () => {
    insights.setDateFrom("2025-01-13");
    insights.setDateTo("2025-01-17");
    expect(insights.dateTo).toBe("2025-01-17");

    // Simulate switching to single-day mode
    insights.setDateTo(insights.dateFrom);
    expect(insights.dateFrom).toBe("2025-01-13");
    expect(insights.dateTo).toBe("2025-01-13");
  });

  it("generates with range dates when set", async () => {
    insights.setDateFrom("2025-01-13");
    insights.setDateTo("2025-01-17");
    insights.setType("daily_activity");

    const mockHandle = {
      abort: vi.fn(),
      done: Promise.resolve(makeInsight({ id: 1 })),
    };
    vi.mocked(api.generateInsight).mockReturnValueOnce(
      mockHandle,
    );

    insights.generate();

    expect(api.generateInsight).toHaveBeenCalledWith(
      expect.objectContaining({
        date_from: "2025-01-13",
        date_to: "2025-01-17",
        type: "daily_activity",
      }),
      expect.any(Function),
      expect.any(Function),
    );
  });

  it("generates with same date when synced", async () => {
    insights.setDateFrom("2025-01-15");
    insights.setDateTo("2025-01-15");

    const mockHandle = {
      abort: vi.fn(),
      done: Promise.resolve(makeInsight({ id: 1 })),
    };
    vi.mocked(api.generateInsight).mockReturnValueOnce(
      mockHandle,
    );

    insights.generate();

    expect(api.generateInsight).toHaveBeenCalledWith(
      expect.objectContaining({
        date_from: "2025-01-15",
        date_to: "2025-01-15",
      }),
      expect.any(Function),
      expect.any(Function),
    );
  });
});

describe("setProject", () => {
  it("updates project without reloading", () => {
    insights.setProject("my-app");

    expect(insights.project).toBe("my-app");
    expect(api.listInsights).not.toHaveBeenCalled();
  });
});

describe("select", () => {
  it("sets selectedId and clears selectedTaskId", () => {
    insights.selectedTaskId = "some-task";
    insights.select(42);
    expect(insights.selectedId).toBe(42);
    expect(insights.selectedTaskId).toBeNull();
  });
});

describe("selectTask", () => {
  it("sets selectedTaskId and clears selectedId", () => {
    insights.selectedId = 42;
    insights.selectTask("task-123");
    expect(insights.selectedTaskId).toBe("task-123");
    expect(insights.selectedId).toBeNull();
  });
});

describe("selectedItem", () => {
  it("returns matching insight", () => {
    const s = makeInsight({ id: 5 });
    insights.items = [s];
    insights.selectedId = 5;
    expect(insights.selectedItem).toEqual(s);
  });

  it("returns undefined when no match", () => {
    insights.items = [makeInsight({ id: 1 })];
    insights.selectedId = 99;
    expect(insights.selectedItem).toBeUndefined();
  });
});

describe("generate (multi-task)", () => {
  it("adds task to tasks[] and prepends result on completion", async () => {
    const newInsight = makeInsight({ id: 10 });
    const mockHandle = {
      abort: vi.fn(),
      done: Promise.resolve(newInsight),
    };
    vi.mocked(api.generateInsight).mockReturnValueOnce(
      mockHandle,
    );

    insights.generate();

    expect(insights.tasks).toHaveLength(1);
    expect(insights.tasks[0]!.status).toBe("generating");

    // Wait for the promise chain to settle
    await new Promise((r) => setTimeout(r, 0));

    expect(insights.tasks).toHaveLength(0);
    expect(insights.items[0]).toEqual(newInsight);
    expect(insights.selectedId).toBe(10);
  });

  it("supports multiple concurrent tasks", async () => {
    const s1 = makeInsight({ id: 10 });
    const s2 = makeInsight({ id: 11 });
    let resolve1!: (s: Insight) => void;
    let resolve2!: (s: Insight) => void;

    vi.mocked(api.generateInsight)
      .mockReturnValueOnce({
        abort: vi.fn(),
        done: new Promise((r) => {
          resolve1 = r;
        }),
      })
      .mockReturnValueOnce({
        abort: vi.fn(),
        done: new Promise((r) => {
          resolve2 = r;
        }),
      });

    insights.generate();
    insights.generate();

    expect(insights.tasks).toHaveLength(2);

    resolve1(s1);
    await new Promise((r) => setTimeout(r, 0));

    expect(insights.tasks).toHaveLength(1);
    expect(insights.items[0]).toEqual(s1);

    resolve2(s2);
    await new Promise((r) => setTimeout(r, 0));

    expect(insights.tasks).toHaveLength(0);
    expect(insights.items[0]).toEqual(s2);
  });

  it("sets error on task failure and selects task", async () => {
    const mockHandle = {
      abort: vi.fn(),
      done: Promise.reject(new Error("CLI not found")),
    };
    vi.mocked(api.generateInsight).mockReturnValueOnce(
      mockHandle,
    );

    insights.generate();
    await new Promise((r) => setTimeout(r, 0));

    expect(insights.tasks).toHaveLength(1);
    expect(insights.tasks[0]!.status).toBe("error");
    expect(insights.tasks[0]!.error).toBe("CLI not found");
    expect(insights.selectedTaskId).toBe(
      insights.tasks[0]!.clientId,
    );
    expect(insights.selectedId).toBeNull();
  });

  it("captures streaming logs per task", async () => {
    let doneResolve!: (s: Insight) => void;
    vi.mocked(api.generateInsight).mockImplementationOnce(
      (_req, _onStatus, onLog) => {
        onLog?.({ stream: "stdout", line: "{\"type\":\"system\"}" });
        onLog?.({ stream: "stderr", line: "rate limit warning" });
        return {
          abort: vi.fn(),
          done: new Promise<Insight>((resolve) => {
            doneResolve = resolve;
          }),
        };
      },
    );

    insights.generate();

    expect(insights.tasks).toHaveLength(1);
    expect(insights.tasks[0]!.logs).toEqual([
      { stream: "stdout", line: "{\"type\":\"system\"}" },
      { stream: "stderr", line: "rate limit warning" },
    ]);

    doneResolve(makeInsight({ id: 111 }));
    await new Promise((r) => setTimeout(r, 0));
    expect(insights.tasks).toHaveLength(0);
  });

  it("caps logs to the most recent 200 lines", async () => {
    let doneResolve!: (s: Insight) => void;
    vi.mocked(api.generateInsight).mockImplementationOnce(
      (_req, _onStatus, onLog) => {
        for (let i = 0; i < 250; i++) {
          onLog?.({
            stream: "stdout",
            line: `line-${i}`,
          });
        }
        return {
          abort: vi.fn(),
          done: new Promise<Insight>((resolve) => {
            doneResolve = resolve;
          }),
        };
      },
    );

    insights.generate();
    expect(insights.tasks).toHaveLength(1);
    expect(insights.tasks[0]!.logs).toHaveLength(200);
    expect(insights.tasks[0]!.logs[0]!.line).toBe("line-50");
    expect(insights.tasks[0]!.logs[199]!.line).toBe("line-249");

    doneResolve(makeInsight({ id: 222 }));
    await new Promise((r) => setTimeout(r, 0));
    expect(insights.tasks).toHaveLength(0);
  });

  it("calls load instead of prepend when filters changed", async () => {
    const newInsight = makeInsight({ id: 20 });
    let resolveDone!: (s: Insight) => void;
    const mockHandle = {
      abort: vi.fn(),
      done: new Promise<Insight>((resolve) => {
        resolveDone = resolve;
      }),
    };
    vi.mocked(api.generateInsight).mockReturnValueOnce(
      mockHandle,
    );
    vi.mocked(api.listInsights).mockResolvedValue({
      insights: [newInsight],
    });

    insights.generate();

    // Change project while generation is in flight.
    insights.project = "other-project";

    resolveDone(newInsight);
    await new Promise((r) => setTimeout(r, 0));

    // Should not have prepended -- should have called load.
    expect(api.listInsights).toHaveBeenCalled();
    expect(insights.selectedId).not.toBe(20);
  });

  it("removes task on abort without error", async () => {
    const abortError = new DOMException(
      "Aborted",
      "AbortError",
    );
    const mockHandle = {
      abort: vi.fn(),
      done: Promise.reject(abortError),
    };
    vi.mocked(api.generateInsight).mockReturnValueOnce(
      mockHandle,
    );

    insights.generate();
    await new Promise((r) => setTimeout(r, 0));

    expect(insights.tasks).toHaveLength(0);
  });
});

describe("cancelTask", () => {
  it("aborts a specific task", async () => {
    const abortFn = vi.fn();
    let rejectDone!: (err: Error) => void;
    const mockHandle = {
      abort: abortFn,
      done: new Promise<Insight>((_resolve, reject) => {
        rejectDone = reject;
      }),
    };
    vi.mocked(api.generateInsight).mockReturnValueOnce(
      mockHandle,
    );

    insights.generate();
    const clientId = insights.tasks[0]!.clientId;

    insights.cancelTask(clientId);
    expect(abortFn).toHaveBeenCalled();

    rejectDone(
      new DOMException("Aborted", "AbortError"),
    );
    await new Promise((r) => setTimeout(r, 0));

    expect(insights.tasks).toHaveLength(0);
  });
});

describe("dismissTask", () => {
  it("removes an errored task", async () => {
    const mockHandle = {
      abort: vi.fn(),
      done: Promise.reject(new Error("fail")),
    };
    vi.mocked(api.generateInsight).mockReturnValueOnce(
      mockHandle,
    );

    insights.generate();
    await new Promise((r) => setTimeout(r, 0));

    expect(insights.tasks).toHaveLength(1);
    const clientId = insights.tasks[0]!.clientId;

    insights.dismissTask(clientId);

    expect(insights.tasks).toHaveLength(0);
  });
});

describe("deleteItem", () => {
  it("removes item and clears selection", async () => {
    const s = makeInsight({ id: 5 });
    insights.items = [s];
    insights.selectedId = 5;
    vi.mocked(api.deleteInsight).mockResolvedValueOnce(
      undefined,
    );

    await insights.deleteItem(5);

    expect(api.deleteInsight).toHaveBeenCalledWith(5);
    expect(insights.items).toHaveLength(0);
    expect(insights.selectedId).toBeNull();
  });

  it("keeps selection if deleting non-selected item", async () => {
    const s1 = makeInsight({ id: 1 });
    const s2 = makeInsight({ id: 2 });
    insights.items = [s1, s2];
    insights.selectedId = 1;
    vi.mocked(api.deleteInsight).mockResolvedValueOnce(
      undefined,
    );

    await insights.deleteItem(2);

    expect(insights.items).toHaveLength(1);
    expect(insights.selectedId).toBe(1);
  });

  it("does not remove on non-404 API error", async () => {
    const s = makeInsight({ id: 5 });
    insights.items = [s];
    insights.selectedId = 5;
    vi.mocked(api.deleteInsight).mockRejectedValueOnce(
      new ApiError(500, "internal error"),
    );

    await insights.deleteItem(5);

    expect(insights.items).toHaveLength(1);
    expect(insights.selectedId).toBe(5);
  });

  it("removes item locally when server returns 404", async () => {
    const s = makeInsight({ id: 5 });
    insights.items = [s];
    insights.selectedId = 5;
    vi.mocked(api.deleteInsight).mockRejectedValueOnce(
      new ApiError(404, "not found"),
    );

    await insights.deleteItem(5);

    expect(insights.items).toHaveLength(0);
    expect(insights.selectedId).toBeNull();
  });
});

describe("setAgent", () => {
  it("updates agent without reloading", () => {
    insights.setAgent("codex");

    expect(insights.agent).toBe("codex");
    expect(api.listInsights).not.toHaveBeenCalled();
  });
});

describe("cancelAll", () => {
  it("aborts all generating tasks", async () => {
    const abort1 = vi.fn();
    const abort2 = vi.fn();
    let reject1!: (err: Error) => void;
    let reject2!: (err: Error) => void;

    vi.mocked(api.generateInsight)
      .mockReturnValueOnce({
        abort: abort1,
        done: new Promise<Insight>((_r, rej) => {
          reject1 = rej;
        }),
      })
      .mockReturnValueOnce({
        abort: abort2,
        done: new Promise<Insight>((_r, rej) => {
          reject2 = rej;
        }),
      });

    insights.generate();
    insights.generate();

    expect(insights.tasks).toHaveLength(2);

    insights.cancelAll();

    expect(abort1).toHaveBeenCalled();
    expect(abort2).toHaveBeenCalled();

    const abortErr = new DOMException(
      "Aborted",
      "AbortError",
    );
    reject1(abortErr);
    reject2(abortErr);
    await new Promise((r) => setTimeout(r, 0));

    expect(insights.tasks).toHaveLength(0);
  });
});

describe("load error handling", () => {
  it("clears items on API error", async () => {
    insights.items = [makeInsight({ id: 1 })];
    vi.mocked(api.listInsights).mockRejectedValueOnce(
      new Error("network error"),
    );

    await insights.load();

    expect(insights.items).toHaveLength(0);
    expect(insights.loading).toBe(false);
  });

  it("discards stale responses", async () => {
    let resolve1!: (v: { insights: Insight[] }) => void;
    vi.mocked(api.listInsights)
      .mockImplementationOnce(
        () =>
          new Promise((r) => {
            resolve1 = r;
          }),
      )
      .mockResolvedValueOnce({
        insights: [makeInsight({ id: 2 })],
      });

    // Start first load (will be slow)
    const p1 = insights.load();
    // Start second load (resolves immediately)
    const p2 = insights.load();
    await p2;

    // First response arrives late
    resolve1({ insights: [makeInsight({ id: 1 })] });
    await p1;

    // Stale response should be ignored
    expect(insights.items).toHaveLength(1);
    expect(insights.items[0]!.id).toBe(2);
  });
});

describe("generatingCount", () => {
  it("counts active generating tasks", async () => {
    vi.mocked(api.generateInsight)
      .mockReturnValueOnce({
        abort: vi.fn(),
        done: new Promise(() => {}),
      })
      .mockReturnValueOnce({
        abort: vi.fn(),
        done: Promise.reject(new Error("fail")),
      });

    insights.generate();
    insights.generate();
    await new Promise((r) => setTimeout(r, 0));

    // One still generating, one errored
    expect(insights.generatingCount).toBe(1);
  });
});
