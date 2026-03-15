// @vitest-environment jsdom
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
} from "vitest";
import { mount, unmount, tick } from "svelte";

const { mockUi, mockSessions, mockSearchStore } = vi.hoisted(
  () => ({
    mockUi: {
      activeModal: "commandPalette" as
        | "commandPalette"
        | null,
      scrollToOrdinal: vi.fn(),
    },
    mockSessions: {
      sessions: [] as Array<{
        id: string;
        project: string;
        machine: string;
        agent: string;
        first_message: string | null;
        started_at: string | null;
        ended_at: string | null;
        message_count: number;
        user_message_count: number;
        created_at: string;
      }>,
      filters: { project: "" },
      selectSession: vi.fn(),
    },
    mockSearchStore: {
      results: [] as Array<unknown>,
      isSearching: false,
      search: vi.fn(),
      clear: vi.fn(),
    },
  }),
);

vi.mock("../../stores/ui.svelte.js", () => ({
  ui: mockUi,
}));

vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: mockSessions,
}));

vi.mock("../../stores/search.svelte.js", () => ({
  searchStore: mockSearchStore,
}));

vi.mock("../../stores/messages.svelte.js", () => ({
  messages: {},
}));

// @ts-ignore
import CommandPalette from "./CommandPalette.svelte";

function makeSession(id: string, agent: string) {
  return {
    id,
    project: "proj-a",
    machine: "mac",
    agent,
    first_message: "hello",
    started_at: "2026-02-20T12:30:00Z",
    ended_at: "2026-02-20T12:31:00Z",
    message_count: 2,
    user_message_count: 1,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    created_at: "2026-02-20T12:30:00Z",
  };
}

describe("CommandPalette", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockSearchStore.results = [];
    mockSearchStore.isSearching = false;
    mockSessions.filters.project = "";
    mockSessions.sessions = [
      makeSession("s1", "cursor"),
      makeSession("s2", "unknown"),
    ];
  });

  it("uses agentColor for recent-session dots including fallback", async () => {
    const component = mount(CommandPalette, {
      target: document.body,
    });

    await tick();

    const dots = Array.from(
      document.querySelectorAll<HTMLElement>(".item-dot"),
    );
    expect(dots).toHaveLength(2);
    expect(dots[0]?.getAttribute("style")).toContain(
      "var(--accent-black)",
    );
    expect(dots[1]?.getAttribute("style")).toContain(
      "var(--accent-blue)",
    );

    unmount(component);
  });
});
