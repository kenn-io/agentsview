import { describe, expect, it } from "vitest";
import {
  ApiError,
  callGenerated,
} from "./runtime.js";
import {
  ApiError as GeneratedApiError,
} from "./generated/index";

describe("callGenerated", () => {
  it("normalizes generated API error bodies", async () => {
    await expect(
      callGenerated(async () => {
        throw new GeneratedApiError(
          { method: "GET", url: "/api/v1/usage/summary" },
          {
            url: "/api/v1/usage/summary",
            ok: false,
            status: 400,
            statusText: "Bad Request",
            body: { error: "invalid timezone: Fake/Zone" },
          },
          "Bad Request",
        );
      }),
    ).rejects.toMatchObject({
      name: "ApiError",
      status: 400,
      message: "invalid timezone: Fake/Zone",
    } satisfies Partial<ApiError>);
  });

  it("preserves machine-readable generated API error codes", async () => {
    await expect(
      callGenerated(async () => {
        throw new GeneratedApiError(
          { method: "GET", url: "/api/v1/usage/summary" },
          {
            url: "/api/v1/usage/summary",
            ok: false,
            status: 400,
            statusText: "Bad Request",
            body: {
              code: "unknown_project_key",
              error: "unknown project key",
            },
          },
          "Bad Request",
        );
      }),
    ).rejects.toMatchObject({
      status: 400,
      code: "unknown_project_key",
      message: "unknown project key",
    } satisfies Partial<ApiError>);
  });
});
