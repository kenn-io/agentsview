import {
  describe,
  expect,
  it,
  vi,
  beforeEach,
} from "vite-plus/test";

const { request } = vi.hoisted(() => ({
  request: vi.fn(),
}));

vi.mock("../core/OpenAPI", () => ({
  OpenAPI: {},
}));

vi.mock("../core/request", () => ({
  request,
}));

import { AnalyticsService } from "./AnalyticsService";

describe("AnalyticsService signal sessions", () => {
  beforeEach(() => {
    request.mockReset();
    request.mockResolvedValue({});
  });

  it("includes the model filter in signal session requests", async () => {
    await AnalyticsService.getApiV1AnalyticsSignalSessions({
      signal: "runaway_tool_loop_count",
      from: "2024-06-01",
      to: "2024-06-07",
      timezone: "UTC",
      model: "gpt-4o",
    });

    expect(request).toHaveBeenCalledWith(
      {},
      expect.objectContaining({
        query: expect.objectContaining({
          signal: "runaway_tool_loop_count",
          model: "gpt-4o",
        }),
      }),
    );
  });
});
