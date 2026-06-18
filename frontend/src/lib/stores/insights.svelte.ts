import type {
  Insight,
  InsightsResponse,
  InsightType,
  AgentName,
} from "../api/types.js";
import {
  ApiError as GeneratedApiError,
  InsightsService,
} from "../api/generated/index";
import { configureGeneratedClient } from "../api/runtime.js";
import {
  generateInsight,
  type GenerateInsightHandle,
  type InsightLogEvent,
} from "../api/client.js";

function localDateStr(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

export interface InsightTask {
  clientId: string;
  type: InsightType;
  dateFrom: string;
  dateTo: string;
  project: string;
  agent: AgentName;
  status: "generating" | "done" | "error";
  phase: string;
  error: string | null;
  insightId: number | null;
  logs: InsightLogEvent[];
}

const MAX_TASK_LOG_LINES = 200;

class InsightsStore {
  dateFrom: string = $state(localDateStr(new Date()));
  dateTo: string = $state(localDateStr(new Date()));
  type: InsightType = $state("daily_activity");
  project: string = $state("");
  agent: AgentName = $state("claude");
  items: Insight[] = $state([]);
  selectedId: number | null = $state(null);
  selectedTaskId: string | null = $state(null);
  loading = $state(false);
  promptText: string = $state("");
  tasks: InsightTask[] = $state([]);

  #handles = new Map<string, GenerateInsightHandle>();
  #version = 0;

  get selectedItem(): Insight | undefined {
    return this.items.find(
      (s) => s.id === this.selectedId,
    );
  }

  get selectedTask(): InsightTask | undefined {
    if (this.selectedTaskId === null) return undefined;
    return this.tasks.find(
      (t) => t.clientId === this.selectedTaskId,
    );
  }

  get generatingCount(): number {
    return this.tasks.filter(
      (t) => t.status === "generating",
    ).length;
  }

  async load() {
    const v = ++this.#version;
    this.loading = true;
    try {
      configureGeneratedClient();
      const res =
        await InsightsService.getApiV1Insights({}) as unknown as InsightsResponse;
      if (this.#version === v) {
        this.items = res.insights;
        if (
          this.selectedId !== null &&
          !this.items.some(
            (s) => s.id === this.selectedId,
          )
        ) {
          this.selectedId = null;
        }
      }
    } catch {
      if (this.#version === v) {
        this.items = [];
      }
    } finally {
      if (this.#version === v) {
        this.loading = false;
      }
    }
  }

  setDateFrom(date: string) {
    this.dateFrom = date;
  }

  setDateTo(date: string) {
    this.dateTo = date;
  }

  setType(type: InsightType) {
    this.type = type;
  }

  setProject(project: string) {
    this.project = project;
  }

  setAgent(agent: AgentName) {
    this.agent = agent;
  }

  select(id: number) {
    this.selectedId = id;
    this.selectedTaskId = null;
  }

  selectTask(clientId: string) {
    this.selectedTaskId = clientId;
    this.selectedId = null;
  }

  generate() {
    const clientId = crypto.randomUUID();
    const snap = {
      type: this.type,
      dateFrom: this.dateFrom,
      dateTo: this.dateTo,
      project: this.project,
      agent: this.agent,
    };

    const task: InsightTask = {
      clientId,
      type: snap.type,
      dateFrom: snap.dateFrom,
      dateTo: snap.dateTo,
      project: snap.project,
      agent: snap.agent,
      status: "generating",
      phase: "generating",
      error: null,
      insightId: null,
      logs: [],
    };
    this.tasks = [...this.tasks, task];

    const handle = generateInsight(
      {
        type: snap.type,
        date_from: snap.dateFrom,
        date_to: snap.dateTo,
        project: snap.project || undefined,
        prompt: this.promptText || undefined,
        timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
        agent: snap.agent,
      },
      (phase) => {
        this.tasks = this.tasks.map((t) =>
          t.clientId === clientId
            ? { ...t, phase }
            : t,
        );
      },
      (logEvent) => {
        this.tasks = this.tasks.map((t) => {
          if (t.clientId !== clientId) {
            return t;
          }
          const nextLogs =
            t.logs.length >= MAX_TASK_LOG_LINES
              ? [...t.logs.slice(1), logEvent]
              : [...t.logs, logEvent];
          return { ...t, logs: nextLogs };
        });
      },
    );
    this.#handles.set(clientId, handle);

    handle.done
      .then((insight) => {
        this.#handles.delete(clientId);
        this.tasks = this.tasks.filter(
          (t) => t.clientId !== clientId,
        );

        const filtersMatch =
          this.project === snap.project;
        if (filtersMatch) {
          this.items = [insight, ...this.items];
          this.selectedId = insight.id;
        } else {
          this.load();
        }
      })
      .catch((e) => {
        this.#handles.delete(clientId);
        if (
          e instanceof DOMException &&
          e.name === "AbortError"
        ) {
          this.tasks = this.tasks.filter(
            (t) => t.clientId !== clientId,
          );
          return;
        }
        const msg =
          e instanceof Error
            ? e.message
            : "Generation failed";
        this.tasks = this.tasks.map((t) =>
          t.clientId === clientId
            ? { ...t, status: "error" as const, error: msg }
            : t,
        );
        this.selectedTaskId = clientId;
        this.selectedId = null;
      });
  }

  cancelTask(clientId: string) {
    this.#handles.get(clientId)?.abort();
  }

  dismissTask(clientId: string) {
    this.#handles.delete(clientId);
    this.tasks = this.tasks.filter(
      (t) => t.clientId !== clientId,
    );
    if (this.selectedTaskId === clientId) {
      this.selectedTaskId = null;
    }
  }

  async deleteItem(id: number) {
    try {
      configureGeneratedClient();
      await InsightsService.deleteApiV1InsightsId({ id });
    } catch (e) {
      if (!(e instanceof GeneratedApiError && e.status === 404)) {
        return;
      }
    }
    this.items = this.items.filter((s) => s.id !== id);
    if (this.selectedId === id) {
      this.selectedId = null;
    }
  }

  cancelAll() {
    for (const handle of this.#handles.values()) {
      handle.abort();
    }
  }
}

export const insights = new InsightsStore();
