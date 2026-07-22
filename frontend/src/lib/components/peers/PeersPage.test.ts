// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/svelte";
import { setLocale } from "../../i18n/index.js";

const mocks = vi.hoisted(() => ({
  getPeers: vi.fn(),
}));

vi.mock("../../api/generated/index", () => ({
  ArtifactsService: {
    getApiV1ArtifactsPeers: mocks.getPeers,
  },
}));

vi.mock("../../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
}));

// @ts-ignore
import PeersPage from "./PeersPage.svelte";

function peer(
  origin: string,
  status = "pending",
  publishedSessions = 1,
  localSessions = 0,
) {
  return {
    origin,
    status,
    is_local: false,
    checkpoint_seq: 1,
    published_sessions: publishedSessions,
    local_sessions: localSessions,
  };
}

function page(
  peers: ReturnType<typeof peer>[],
  nextCursor?: string,
) {
  return {
    peers,
    local_origin: "local-a1b2c3",
    conflict_count: 0,
    next_cursor: nextCursor,
  };
}

describe("PeersPage", () => {
  beforeEach(() => {
    mocks.getPeers.mockReset();
    setLocale("en");
  });

  afterEach(() => cleanup());

  it("renders only the first bounded page on mount", async () => {
    mocks.getPeers
      .mockResolvedValueOnce(page([peer("peer-one")], "cursor-1"))
      .mockResolvedValueOnce(page([peer("peer-two")]));

    render(PeersPage);

    await screen.findByText("peer-one");
    expect(mocks.getPeers).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("peer-two")).toBeNull();
  });

  it("loads the next page only after the user requests it", async () => {
    mocks.getPeers
      .mockResolvedValueOnce(page([peer("peer-one")], "cursor-1"))
      .mockResolvedValueOnce(page([peer("peer-two")]));
    render(PeersPage);
    await screen.findByText("peer-one");

    await fireEvent.click(screen.getByRole("button", { name: "Load more" }));

    await screen.findByText("peer-two");
    expect(mocks.getPeers).toHaveBeenNthCalledWith(2, { cursor: "cursor-1" });
  });

  it("renders the backend error status instead of inferring sync from counts", async () => {
    mocks.getPeers.mockResolvedValueOnce(
      page([peer("corrupt-peer", "error", 3, 3)]),
    );
    render(PeersPage);

    await screen.findByText("corrupt-peer");
    expect(screen.getByText("Sync error")).toBeTruthy();
    expect(screen.queryByText("In sync")).toBeNull();
  });

  it("terminates a repeated cursor and renders a pagination error", async () => {
    mocks.getPeers
      .mockResolvedValueOnce(page([peer("peer-one")], "repeat"))
      .mockResolvedValueOnce(page([peer("peer-two")], "repeat"));
    render(PeersPage);
    await screen.findByText("peer-one");

    await fireEvent.click(screen.getByRole("button", { name: "Load more" }));

    await screen.findByText("More peers could not be loaded.");
    expect(screen.queryByText("peer-two")).toBeNull();
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
    expect(mocks.getPeers).toHaveBeenCalledTimes(2);
  });

  it("refresh replaces the first page and resets pagination state", async () => {
    mocks.getPeers
      .mockResolvedValueOnce(page([peer("old-one")], "old-cursor"))
      .mockResolvedValueOnce(page([peer("old-two")]))
      .mockResolvedValueOnce(page([peer("fresh-one")], "fresh-cursor"))
      .mockResolvedValueOnce(page([peer("fresh-two")]));
    render(PeersPage);
    await screen.findByText("old-one");
    await fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await screen.findByText("old-two");

    await fireEvent.click(
      screen.getByRole("button", { name: "Refresh peer status" }),
    );

    await screen.findByText("fresh-one");
    expect(screen.queryByText("old-one")).toBeNull();
    expect(screen.queryByText("old-two")).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await screen.findByText("fresh-two");
    await waitFor(() => {
      expect(mocks.getPeers).toHaveBeenNthCalledWith(3, { cursor: undefined });
      expect(mocks.getPeers).toHaveBeenNthCalledWith(4, {
        cursor: "fresh-cursor",
      });
    });
  });
});
