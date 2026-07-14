import { describe, expect, it, vi } from "vitest";
import {
  ApiError,
  callGenerated,
} from "./runtime.js";
import {
  ApiError as GeneratedApiError,
  CancelablePromise,
} from "./generated/index";

describe("callGenerated", () => {
  it("cancels the generated transport when its signal aborts", async () => {
    const cancelTransport = vi.fn();
    const request = new CancelablePromise<never>(
      (_resolve, _reject, onCancel) => {
        onCancel(cancelTransport);
      },
    );
    const controller = new AbortController();

    const result = callGenerated(() => request, controller.signal);
    controller.abort();

    await expect(result).rejects.toMatchObject({
      name: "CancelError",
    });
    expect(cancelTransport).toHaveBeenCalledOnce();
  });

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
